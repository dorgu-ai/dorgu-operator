/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// Approval re-check and ticket (task 1.3, 1.5, design answer 3), against a fake
// client: the phase machine needs no API server.

func ticketPatchedMemory(t *testing.T, ra *dorguv1.RemediationAction) string {
	t.Helper()
	if ra.Status.ApplyTicket == nil {
		return ""
	}
	return gate.PatchLeafValues(ra.Status.ApplyTicket.Patch.Raw)["spec.resources.limits.memory"]
}

func TestApproved_ReChecksAndIssuesATicketThenApplies(t *testing.T) {
	ra := lcRA("Approved")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	res, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseApplying {
		t.Fatalf("phase = %q, want Applying (ticketed and applied in one pass)", got.Status.Phase)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want the verification delay", res.RequeueAfter)
	}
	ticket := got.Status.ApplyTicket
	if ticket == nil {
		t.Fatal("no status.applyTicket was written")
	}
	if ticket.WorkloadUID != lcDeploymentUID || ticket.ObservedResourceVersion != lcDeploymentRV || ticket.Container != lcPersona {
		t.Errorf("ticket is not bound to the live workload: %+v", ticket)
	}
	if mem := ticketPatchedMemory(t, got); mem != lcApplied {
		t.Errorf("ticket memory = %q, want %s", mem, lcApplied)
	}
	if len(ticket.Verdicts) == 0 {
		t.Error("the ticket carries no verdicts")
	}
	if !ticket.ExpiresAt.After(time.Now()) {
		t.Errorf("ticket already expired: %v", ticket.ExpiresAt)
	}
	if cond := lcCondition(got, dorguv1.ConditionTicketed); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ticketed condition = %+v, want True", cond)
	}
	if got.Status.Gate == nil || got.Status.Gate.Phase != string(gate.PhaseApprovalRecheck) {
		t.Errorf("status.gate = %+v, want the ApprovalRecheck decision", got.Status.Gate)
	}
	if mem := lcPersonaMemory(t, c); mem != lcApplied {
		t.Errorf("persona memory = %s, want %s", mem, lcApplied)
	}
}

// spec.action is deprecated: whatever it says, the executor applies the ticket.
func TestApproved_TheTicketIsAppliedNotTheSpecAction(t *testing.T) {
	ra := lcRA("Approved", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action.Patch = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"999Gi"}}}}`)}
	})
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseApplying {
		t.Fatalf("phase = %q, want Applying", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcApplied {
		t.Errorf("persona memory = %s, want the planned %s and not the edited spec.action", mem, lcApplied)
	}
}

func TestApproved_RefusedAtApprovalIsFailedWithAPersistedReasonAndNoCooldown(t *testing.T) {
	// The live limit was cut to 64Mi after the proposal: 512Mi is now 8x live and,
	// as the only change in the plan, nothing is left to apply.
	ra := lcRA("Approved")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), lcDeployment("64Mi"), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.ApplyTicket != nil || got.Status.AppliedAt != nil {
		t.Errorf("a refused approval wrote a ticket or applied: %+v", got.Status)
	}
	applied := lcCondition(got, ConditionApplied)
	if applied == nil || applied.Reason != dorguv1.ReasonGateRefusedAtApproval {
		t.Fatalf("Applied condition = %+v, want reason GateRefusedAtApproval", applied)
	}
	refused := lcCondition(got, dorguv1.ConditionGateRefused)
	if refused == nil || refused.Status != metav1.ConditionTrue || refused.Reason == "" || strings.Contains(refused.Reason, "-") {
		t.Fatalf("GateRefused condition = %+v, want True with a CamelCase enum reason", refused)
	}
	if got.Status.Gate == nil || got.Status.Gate.Result != string(gate.ResultRefuse) {
		t.Errorf("status.gate = %+v, want the refusing decision", got.Status.Gate)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: a refused approval must not apply", mem)
	}

	// No cooldown: nothing was applied, so the persona is free to be proposed for
	// again at once.
	fresh := gate.New(c, logr.Discard())
	d, err := fresh.Run(context.Background(), gate.Candidate{
		Target: got.Spec.PersonaRef, Namespace: lcNamespace, Name: "ra-next",
		IncidentRef: got.Spec.IncidentRef,
		Steps: []dorguv1.RemediationStep{{
			Order: 1, ID: "s", Type: "persona-update", AutoExecutable: true,
			Patch: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"96Mi"}}}}`)},
		}},
	}, gate.PhaseProposal)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range d.Verdicts {
		if v.Guardrail == gate.GuardrailFailedCooldown && v.Verdict != dorguv1.GateVerdictPass {
			t.Errorf("a gate refusal at approval started the failure cooldown: %+v", v)
		}
	}
}

func TestApproved_ObserveModeRefusesAtApproval(t *testing.T) {
	cp := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: dorguv1.ClusterPersonaSpec{
			Name:     "cluster",
			Policies: &dorguv1.ClusterPolicies{SelfHealing: &dorguv1.SelfHealingPolicy{Mode: dorguv1.SelfHealingModeObserve}},
		},
	}
	ra := lcRA("Approved")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), cp, ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed: observe mode at approval means refuse", got.Status.Phase)
	}
	if refused := lcCondition(got, dorguv1.ConditionGateRefused); refused == nil || refused.Reason != "SelfHealingMode" {
		t.Errorf("GateRefused = %+v, want reason SelfHealingMode", refused)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s, want it untouched", mem)
	}
}

func TestApproved_AnEditedSpecIsRefused(t *testing.T) {
	ra := lcRA("Approved", func(ra *dorguv1.RemediationAction) { ra.Generation = 2 })
	// lcRecordProposal pinned generation 0; the spec is now at generation 2.
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if refused := lcCondition(got, dorguv1.ConditionGateRefused); refused == nil || refused.Reason != "SpecChanged" {
		t.Errorf("GateRefused = %+v, want reason SpecChanged", refused)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: an edited spec must not apply", mem)
	}
}

func TestApproved_WithoutAGateNothingIsApplied(t *testing.T) {
	ra := lcRA("Approved")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rc.Gate = nil

	_, err := rc.Reconcile(context.Background(), reqFor(ra))

	if err == nil {
		t.Fatal("want an error: with no gate the approval cannot be re-checked")
	}
	var got dorguv1.RemediationAction
	if getErr := c.Get(context.Background(), client.ObjectKeyFromObject(ra), &got); getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status.Phase != RemediationPhaseApproved {
		t.Errorf("phase = %q, want it left Approved (retried), not applied or failed", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: nothing may apply without a gate", mem)
	}
}

func TestApproved_AnUnreadableClusterIsRetriedNotPassed(t *testing.T) {
	ra := lcRA("Approved")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	broken := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*appsv1.DeploymentList); ok {
				return errors.New("the API server is unavailable")
			}
			return inner.List(ctx, list, opts...)
		},
	})
	rc.Client = broken
	rc.Gate = gate.New(broken, logr.Discard())

	if _, err := rc.Reconcile(context.Background(), reqFor(ra)); err == nil {
		t.Fatal("want an error when the live workload cannot be read")
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: an unreadable cluster must never pass an approval", mem)
	}
}

func TestApproved_AdvisoryPlansNeedNoGate(t *testing.T) {
	ra := lcRA("Approved", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Steps = []dorguv1.RemediationStep{{Order: 1, ID: "s", Type: "manual", Description: "look"}}
		ra.Spec.Action = dorguv1.RemediationActionDetail{Type: "notification"}
	})
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rc.Gate = nil

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseAcknowledged {
		t.Fatalf("phase = %q, want Acknowledged", got.Status.Phase)
	}
}

// --- Ticketed: the apply-time check ---------------------------------------------

func TestTicketed_AppliesAValidTicket(t *testing.T) {
	ra := lcRA("Ticketed")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseApplying {
		t.Fatalf("phase = %q, want Applying", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcApplied {
		t.Errorf("persona memory = %s, want %s", mem, lcApplied)
	}
}

func TestTicketed_RefusesWithoutAValidTicket(t *testing.T) {
	cases := map[string]func(*dorguv1.RemediationAction){
		"no ticket":        func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket = nil },
		"another workload": func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.WorkloadUID = "uid-someone-else" },
		"policy in a patch": func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.Patch = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"policies":{"security":{"runAsNonRoot":false}}}}`)}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ra := lcRA("Ticketed", mutate)
			rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

			_, got := lcReconcile(t, rc, ra)

			if got.Status.Phase != RemediationPhaseFailed {
				t.Fatalf("phase = %q, want Failed", got.Status.Phase)
			}
			applied := lcCondition(got, ConditionApplied)
			if applied == nil || applied.Reason != dorguv1.ReasonPreconditionRejected ||
				!strings.Contains(applied.Message, "nothing was applied") {
				t.Errorf("Applied = %+v, want PreconditionRejected, nothing applied (no cooldown)", applied)
			}
			if mem := lcPersonaMemory(t, c); mem != lcBaseline {
				t.Errorf("persona memory = %s: a refused ticket must not apply", mem)
			}
		})
	}
}

func TestTicketed_AWorkloadChangedUnderTheTicketIsRefused(t *testing.T) {
	// Someone else edited the Deployment after the ticket was issued.
	ra := lcRA("Ticketed")
	dep := lcDeployment(lcBaseline)
	dep.ResourceVersion = "8"
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), dep, ra.DeepCopy()})
	edited := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := inner.List(ctx, list, opts...); err != nil {
				return err
			}
			if deployments, ok := list.(*appsv1.DeploymentList); ok {
				stamp := metav1.NewTime(time.Now().Add(time.Second))
				for i := range deployments.Items {
					deployments.Items[i].ManagedFields = []metav1.ManagedFieldsEntry{{
						Manager: "kubectl-edit", Operation: metav1.ManagedFieldsOperationUpdate,
						APIVersion: "apps/v1", FieldsType: "FieldsV1", Time: &stamp,
					}}
				}
			}
			return nil
		},
	})
	rc.Client = edited
	rc.Gate = gate.New(edited, logr.Discard())
	rc.Executor = remediation.NewExecutor(edited, rc.Gate, logr.Discard())

	_, err := rc.Reconcile(context.Background(), reqFor(ra))
	if err != nil {
		t.Fatal(err)
	}
	var got dorguv1.RemediationAction
	if getErr := c.Get(context.Background(), client.ObjectKeyFromObject(ra), &got); getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: a stale ticket must not apply", mem)
	}
}

// --- design answer 3: a healthy verification over a diverged workload -----------

func healthyController(t *testing.T, ra *dorguv1.RemediationAction, objs ...client.Object) (*RemediationController, client.WithWatch) {
	t.Helper()
	all := append([]client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), ra.DeepCopy()}, objs...)
	return lcController(t, all) // no signals: verification is Healthy
}

func TestVerifyingHealthy_CleanWhenTheWorkloadMatchesTheTicket(t *testing.T) {
	ra := lcRA("Verifying", lcPatchedByTheCLI)
	rc, c := healthyController(t, ra, lcDeploymentAtGeneration(2))

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseCompleted {
		t.Fatalf("phase = %q, want Completed", got.Status.Phase)
	}
	if out := lcIncidentOutcome(t, c); out != IncidentOutcomeResolved {
		t.Errorf("incident outcome = %q, want resolved", out)
	}
	if cond := lcCondition(got, ConditionWorkloadDiverged); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("WorkloadDiverged = %+v, want False", cond)
	}
}

func TestVerifyingHealthy_PartialWhenTheWorkloadWasReplaced(t *testing.T) {
	ra := lcRA("Verifying")
	replaced := lcDeployment(lcBaseline)
	replaced.UID = "a-different-deployment"
	rc, c := healthyController(t, ra, replaced)

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseCompleted {
		t.Fatalf("phase = %q, want it to stay Completed: verification did pass", got.Status.Phase)
	}
	if out := lcIncidentOutcome(t, c); out != IncidentOutcomePartial {
		t.Errorf("incident outcome = %q, want partial", out)
	}
	cond := lcCondition(got, ConditionWorkloadDiverged)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != dorguv1.ReasonWorkloadDivergedFromTicket {
		t.Errorf("WorkloadDiverged = %+v, want True / WorkloadDiverged", cond)
	}
}

func TestVerifyingHealthy_PartialWhenSomeoneElseEditedTheWorkload(t *testing.T) {
	ra := lcRA("Verifying")
	dep := lcDeployment(lcBaseline)
	dep.ResourceVersion = "9"
	rc, c := healthyController(t, ra, dep)
	edited := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := inner.List(ctx, list, opts...); err != nil {
				return err
			}
			if deployments, ok := list.(*appsv1.DeploymentList); ok {
				stamp := metav1.NewTime(time.Now().Add(time.Second))
				for i := range deployments.Items {
					deployments.Items[i].ManagedFields = []metav1.ManagedFieldsEntry{{
						Manager: "argocd", Operation: metav1.ManagedFieldsOperationUpdate,
						APIVersion: "apps/v1", FieldsType: "FieldsV1", Time: &stamp,
					}}
				}
			}
			return nil
		},
	})
	rc.Client = edited
	rc.Gate = gate.New(edited, logr.Discard())

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseCompleted {
		t.Fatalf("phase = %q, want Completed", got.Status.Phase)
	}
	if out := lcIncidentOutcome(t, c); out != IncidentOutcomePartial {
		t.Errorf("incident outcome = %q, want partial", out)
	}
}

func TestVerifyingHealthy_TheCLIsOwnPatchIsNotDivergence(t *testing.T) {
	ra := lcRA("Verifying", lcPatchedByTheCLI)
	dep := lcDeploymentAtGeneration(2)
	dep.ResourceVersion = "9" // the CLI's patch moved it
	rc, c := healthyController(t, ra, dep)
	patched := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := inner.List(ctx, list, opts...); err != nil {
				return err
			}
			if deployments, ok := list.(*appsv1.DeploymentList); ok {
				for i := range deployments.Items {
					deployments.Items[i].ManagedFields = []metav1.ManagedFieldsEntry{
						lcStamp(gate.DorguFieldManager, lcTicketFields),
						lcStamp("kube-controller-manager", lcRevisionFields),
					}
				}
			}
			return nil
		},
	})
	rc.Client = patched
	rc.Gate = gate.New(patched, logr.Discard())

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseCompleted {
		t.Fatalf("phase = %q, want Completed", got.Status.Phase)
	}
	if out := lcIncidentOutcome(t, c); out != IncidentOutcomeResolved {
		t.Errorf("incident outcome = %q, want resolved: the approved patch is not divergence", out)
	}
}

func TestVerifyingHealthy_AnUnreadableWorkloadIsNotACleanHeal(t *testing.T) {
	ra := lcRA("Verifying")
	rc, c := healthyController(t, ra)
	broken := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*appsv1.DeploymentList); ok {
				return errors.New("the API server is unavailable")
			}
			return inner.List(ctx, list, opts...)
		},
	})
	rc.Client = broken
	rc.Gate = gate.New(broken, logr.Discard())

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseCompleted {
		t.Fatalf("phase = %q, want Completed", got.Status.Phase)
	}
	if out := lcIncidentOutcome(t, c); out != IncidentOutcomePartial {
		t.Errorf("incident outcome = %q, want partial: an uncheckable heal is not clean", out)
	}
	if cond := lcCondition(got, ConditionWorkloadDiverged); cond == nil || cond.Status != metav1.ConditionUnknown {
		t.Errorf("WorkloadDiverged = %+v, want Unknown", cond)
	}
}

func TestVerifyingHealthy_ALegacyActionWithoutATicketStaysResolved(t *testing.T) {
	ra := lcRA("Verifying", func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket = nil })
	rc, c := healthyController(t, ra)

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseCompleted {
		t.Fatalf("phase = %q, want Completed", got.Status.Phase)
	}
	if out := lcIncidentOutcome(t, c); out != IncidentOutcomeResolved {
		t.Errorf("incident outcome = %q, want resolved", out)
	}
}

// The signal still present: degraded verification rolls back to the gate's baseline.
func TestVerifyingDegraded_RollsBackToTheGatesBaselineNotTheSpecs(t *testing.T) {
	ra := lcRA("Verifying", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action.PrePatchState = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"1Mi"}}}}`)}
	})
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), ra.DeepCopy()}, lcOOMSignal())

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseRolledBack {
		t.Fatalf("phase = %q, want RolledBack", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s, want the gate's baseline %s and not the spec's 1Mi", mem, lcBaseline)
	}
}

func TestVerifyingDegraded_WithoutATicketThereIsNothingToRollBack(t *testing.T) {
	ra := lcRA("Verifying", func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket = nil })
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), ra.DeepCopy()}, lcOOMSignal())

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed: an action with no ticket has no baseline to restore", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcApplied {
		t.Errorf("persona memory = %s, want it untouched", mem)
	}
}

func TestStatusGateRefusalReasonsAreConditionSafe(t *testing.T) {
	for _, guardrail := range []string{
		gate.GuardrailBlastRadius, gate.GuardrailWorkloadGone, gate.GuardrailNoProposalRecord,
		gate.GuardrailSpecChanged, gate.GuardrailSelfHealingMode, gate.GuardrailConcurrent,
		gate.GuardrailWorkloadAmbiguous, gate.GuardrailWorkloadUnbound, gate.GuardrailLiveWorkload,
		gate.GuardrailNothingLeft, gate.GuardrailNoPlan, gate.GuardrailPatchShape,
	} {
		reason := gate.ReasonEnum(guardrail)
		if reason == "" || strings.ContainsAny(reason, "- _") || reason[0] < 'A' || reason[0] > 'Z' {
			t.Errorf("ReasonEnum(%q) = %q is not a CamelCase condition reason", guardrail, reason)
		}
	}
}

// --- B5 fix round (F1, F4, F10) -----------------------------------------------

// lcPatchedByTheCLI is what a heal leaves behind: the CLI's WorkloadPatched
// condition on the action, and a Deployment whose generation moved past the
// ticket's observed one.
func lcPatchedByTheCLI(ra *dorguv1.RemediationAction) {
	ra.Status.ApplyTicket.ObservedGeneration = 1
	ra.Status.Conditions = append(ra.Status.Conditions, metav1.Condition{
		Type: "WorkloadPatched", Status: metav1.ConditionTrue, Reason: "CLIAppliedResourceChange",
		Message: "dorgu CLI applied resources.limits.memory=512Mi", LastTransitionTime: metav1.Now(),
	})
}

func lcDeploymentAtGeneration(generation int64) *appsv1.Deployment {
	dep := lcDeployment(lcApplied)
	dep.Generation = generation
	return dep
}

func TestF1_ATicketOnAnActionNobodyApprovedIsRefusedWithAReasonEnum(t *testing.T) {
	ra := lcRA("Ticketed", func(ra *dorguv1.RemediationAction) { ra.Status.ApprovedAt = nil })
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	refused := lcCondition(got, dorguv1.ConditionGateRefused)
	if refused == nil || refused.Status != metav1.ConditionTrue || refused.Reason != gate.ReasonEnum(gate.GuardrailNotApproved) {
		t.Fatalf("GateRefused = %+v, want True with reason %s", refused, gate.ReasonEnum(gate.GuardrailNotApproved))
	}
	applied := lcCondition(got, ConditionApplied)
	if applied == nil || applied.Reason != dorguv1.ReasonPreconditionRejected {
		t.Errorf("Applied = %+v, want PreconditionRejected (nothing applied, no cooldown)", applied)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: a forged ticket was applied", mem)
	}
}

func TestF1_AForgedTicketPatchOnAnApprovedActionIsNotApplied(t *testing.T) {
	// The action was properly proposed and approved, then a status writer replaced
	// the ticket's patch with 999Gi and a century of validity.
	ra := lcRA("Ticketed", func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.Patch = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"999Gi"}}}}`)}
		ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(time.Now().Add(100 * 365 * 24 * time.Hour))
	})
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: the forged patch was applied", mem)
	}
}

func TestF3_ApplyAtTheControllerRefusesAnObserveModeFlippedAfterApproval(t *testing.T) {
	ra := lcRA("Ticketed")
	observe := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: dorguv1.ClusterPersonaSpec{Name: "cluster", Policies: &dorguv1.ClusterPolicies{
			SelfHealing: &dorguv1.SelfHealingPolicy{Mode: dorguv1.SelfHealingModeObserve},
		}},
	}
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), observe, ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if refused := lcCondition(got, dorguv1.ConditionGateRefused); refused == nil || refused.Reason != gate.ReasonEnum(gate.GuardrailSelfHealingMode) {
		t.Errorf("GateRefused = %+v", refused)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s", mem)
	}
}

func TestF4_ARollbackLeavesAHumansEditAndSaysSo(t *testing.T) {
	ra := lcRA("Verifying")
	// Dorgu wrote 512Mi; somebody then set the persona to 2Gi. The signal is still
	// there, so verification degrades and the change is rolled back.
	rc, c := lcController(t, []client.Object{lcPersonaObj("2Gi"), lcIncidentObj(), ra.DeepCopy()}, lcOOMSignal())

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseRolledBack {
		t.Fatalf("phase = %q, want RolledBack", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != "2Gi" {
		t.Errorf("persona memory = %s: the rollback overwrote a human's edit", mem)
	}
	if out := lcIncidentOutcome(t, c); out != IncidentOutcomePartial {
		t.Errorf("incident outcome = %q, want partial", out)
	}
	cond := lcCondition(got, ConditionPersonaEdited)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != gate.ReasonRollbackHumanEdited ||
		!strings.Contains(cond.Message, "spec.resources.limits.memory") {
		t.Errorf("%s = %+v, want True / HumanEdited naming the field", ConditionPersonaEdited, cond)
	}
}

func TestF10_NothingPatchedAndTheIncidentResolvesIsSelfResolved(t *testing.T) {
	ra := lcRA("Verifying") // no WorkloadPatched: --no-heal, or a declined prompt
	rc, c := healthyController(t, ra)

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseCompleted {
		t.Fatalf("phase = %q, want Completed", got.Status.Phase)
	}
	if out := lcIncidentOutcome(t, c); out != IncidentOutcomeSelfResolved {
		t.Errorf("incident outcome = %q, want %s: nothing was patched, so Dorgu did not heal it", out, IncidentOutcomeSelfResolved)
	}
	cond := lcCondition(got, ConditionHealCredited)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonNothingPatched {
		t.Errorf("%s = %+v, want False / %s", ConditionHealCredited, cond, ReasonNothingPatched)
	}
	var im dorguv1.IncidentMemory
	if err := c.Get(context.Background(), client.ObjectKey{Name: lcIncident, Namespace: lcNamespace}, &im); err != nil {
		t.Fatal(err)
	}
	if im.Status.Phase != PhaseResolved {
		t.Errorf("incident phase = %q: a self-resolved incident is still resolved", im.Status.Phase)
	}
}

func TestF10_AWorkloadThePatchedByTheCLIIsADorguHeal(t *testing.T) {
	ra := lcRA("Verifying", lcPatchedByTheCLI)
	rc, c := healthyController(t, ra, lcDeploymentAtGeneration(2))

	_, got := lcReconcile(t, rc, ra)

	if out := lcIncidentOutcome(t, c); out != IncidentOutcomeResolved {
		t.Errorf("incident outcome = %q, want resolved", out)
	}
	if cond := lcCondition(got, ConditionHealCredited); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("%s = %+v, want True", ConditionHealCredited, cond)
	}
}

func TestF10_APatchedClaimWithAnUnchangedGenerationIsNotCredited(t *testing.T) {
	// The CLI recorded a patch, but the Deployment's spec never moved: the patch
	// changed nothing (an unchanged workload), so the recovery was not Dorgu's.
	ra := lcRA("Verifying", lcPatchedByTheCLI)
	rc, c := healthyController(t, ra, lcDeploymentAtGeneration(1))

	_, got := lcReconcile(t, rc, ra)

	if out := lcIncidentOutcome(t, c); out != IncidentOutcomeSelfResolved {
		t.Errorf("incident outcome = %q, want %s", out, IncidentOutcomeSelfResolved)
	}
	if cond := lcCondition(got, ConditionHealCredited); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("%s = %+v, want False", ConditionHealCredited, cond)
	}
}
