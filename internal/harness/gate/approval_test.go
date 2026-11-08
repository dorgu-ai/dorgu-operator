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

package gate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// canaryOf turns a Deployment into a second one that also answers to the test
// persona, which makes the workload ambiguous.
const canaryUID = "uid-2"

func canaryOf(d *appsv1.Deployment) { d.Name, d.UID = "web-canary", canaryUID }

// proposedAction is a RemediationAction the operator proposed and a human has
// approved: a cleared Proposal record in status, the Proposed condition at the
// current generation, and a one-step memory plan.
func proposedAction(mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	now := metav1Now()
	approved := metav1.NewTime(fixedNow.Add(-time.Second))
	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: "ra-1", Namespace: testNS, Generation: 1},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef:  target(),
			IncidentRef: dorguv1.IncidentReference{Name: incidentOne, Namespace: testNS},
			PlanSource:  dorguv1.PlanSourceRuleBased,
			Steps:       []dorguv1.RemediationStep{personaUpdateStep(memPatch("384Mi"))},
		},
		Status: dorguv1.RemediationActionStatus{
			Phase:      "Approved",
			ApprovedAt: &approved,
			Gate: &dorguv1.GateStatus{
				Phase: string(PhaseProposal), Result: string(ResultPass), EvaluatedAt: now,
				Observed: priorObservation("uid-1"),
			},
			Conditions: []metav1.Condition{{
				Type: dorguv1.ConditionProposed, Status: metav1.ConditionTrue,
				Reason: dorguv1.ReasonGateCleared, ObservedGeneration: 1,
			}},
		},
	}
	for _, m := range mutate {
		m(ra)
	}
	return ra
}

func approvalRun(t *testing.T, g *Gate, ra *dorguv1.RemediationAction) *Decision {
	t.Helper()
	d, err := g.Run(context.Background(), ApprovalCandidate(ra), PhaseApprovalRecheck)
	require.NoError(t, err)
	return d
}

func standardGate(t *testing.T, extra ...client.Object) *Gate {
	t.Helper()
	return newTestGate(t, fakeClient(t, append([]client.Object{persona("256Mi"), deployment("256Mi")}, extra...)...))
}

// --- L1: only an explicit Pass or Clamp allows; unknown phases are rejected ------

func TestL1_ZeroDecisionIsNotAllowed(t *testing.T) {
	assert.False(t, (&Decision{}).Allowed())
	assert.False(t, (&Decision{Result: Result("Whatever")}).Allowed())
	assert.True(t, (&Decision{Result: ResultPass}).Allowed())
	assert.True(t, (&Decision{Result: ResultClamp}).Allowed())
	assert.False(t, (&Decision{Result: ResultRefuse}).Allowed())
}

func TestL1_UnknownPhaseIsAnErrorNotAPass(t *testing.T) {
	g := standardGate(t)
	d, err := g.Run(context.Background(), planCandidate(personaUpdateStep(memPatch("300Mi"))), Phase("bogus"))
	require.Error(t, err)
	assert.Nil(t, d)
}

// --- M1: approval is always Final -------------------------------------------------

func TestM1_ApprovalRecheckRefusesAnOwnerCommandWhateverFinalSays(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("512Mi", func(d *appsv1.Deployment) {
		d.Labels["app.kubernetes.io/managed-by"] = "Helm"
		d.Annotations = map[string]string{"meta.helm.sh/release-name": "web"}
	})))
	step := dorguv1.RemediationStep{Order: 1, ID: "s", Type: dorguv1.StepTypeManual, Description: "x",
		Command: "kubectl set resources deploy/web --limits=memory=1Gi -n shop"}
	for _, final := range []bool{false, true} {
		cand := planCandidate(step)
		cand.Final = final
		d := run(t, g, cand, PhaseApprovalRecheck)
		assert.Equal(t, ResultRefuse, d.Result, "final=%v", final)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailOwnerCommands), "final=%v", final)
	}
}

func TestM1_ApprovalIgnoresCallerFacts(t *testing.T) {
	g := standardGate(t)
	cand := ApprovalCandidate(proposedAction())
	cand.Facts = Facts{AdvisoryAllowed: true, TargetPath: ResourcePathCPU}

	d, err := g.Run(context.Background(), cand, PhaseApprovalRecheck)
	require.NoError(t, err)
	// A caller-supplied TargetPath would have made the completeness check refuse
	// this memory-only plan; the re-check does not read it.
	assert.NotEqual(t, "refuse", verdictKind(d, GuardrailNothingApplies))
	assert.True(t, d.Allowed(), d.SkipReason())
}

// --- M2: the prior observation is derived from operator-written status ------------

func TestM2_ApprovalWithoutAProposalRecordIsRefused(t *testing.T) {
	g := standardGate(t)
	ra := proposedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Gate = nil })

	d := approvalRun(t, g, ra)

	assert.Equal(t, ResultRefuse, d.Result)
	stage, guardrail := d.RefusedBy()
	assert.Equal(t, StageGround, stage)
	assert.Equal(t, GuardrailNoProposalRecord, guardrail)
}

func TestM2_ARecordTheOperatorDidNotClearIsNotARecord(t *testing.T) {
	g := standardGate(t)
	for name, mutate := range map[string]func(*dorguv1.RemediationAction){
		"a refused proposal": func(ra *dorguv1.RemediationAction) { ra.Status.Gate.Result = string(ResultRefuse) },
		"an approval record": func(ra *dorguv1.RemediationAction) { ra.Status.Gate.Phase = string(PhaseApprovalRecheck) },
		"no observed block":  func(ra *dorguv1.RemediationAction) { ra.Status.Gate.Observed = nil },
		"no observation time": func(ra *dorguv1.RemediationAction) {
			ra.Status.Gate.Observed.ObservedAt = nil
		},
		"no Proposed condition": func(ra *dorguv1.RemediationAction) { ra.Status.Conditions = nil },
	} {
		d := approvalRun(t, g, proposedAction(mutate))
		_, guardrail := d.RefusedBy()
		assert.Equal(t, GuardrailNoProposalRecord, guardrail, name)
	}
}

func TestM2_NoPriorUIDIsRefused(t *testing.T) {
	g := standardGate(t)
	ra := proposedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Gate.Observed.WorkloadUID = "" })

	d := approvalRun(t, g, ra)

	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailWorkloadUnbound, guardrail)
}

func TestM2_ACallerCannotSupplyAPriorToRunDirectly(t *testing.T) {
	g := standardGate(t)
	// A candidate not built by ApprovalCandidate has no prior.
	d, err := g.Run(context.Background(), planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseApprovalRecheck)
	require.NoError(t, err)
	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailNoProposalRecord, guardrail)
}

func TestM2_ASpecEditedAfterTheProposalIsRefused(t *testing.T) {
	g := standardGate(t)
	ra := proposedAction(func(ra *dorguv1.RemediationAction) { ra.Generation = 2 })

	d := approvalRun(t, g, ra)

	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailSpecChanged, guardrail)
}

func TestM2_AReplacedWorkloadIsRefused(t *testing.T) {
	g := standardGate(t)
	ra := proposedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Gate.Observed.WorkloadUID = "older" })

	d := approvalRun(t, g, ra)

	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailLiveWorkload, guardrail)
}

func TestApproval_ACleanProposalPassesAndIssuesATicket(t *testing.T) {
	g := standardGate(t)

	d := approvalRun(t, g, proposedAction())
	require.True(t, d.Allowed(), d.SkipReason())

	ticket, err := d.Ticket()
	require.NoError(t, err)
	assert.Equal(t, "uid-1", ticket.WorkloadUID)
	assert.Equal(t, "42", ticket.ObservedResourceVersion)
	assert.Equal(t, testPersona, ticket.Container)
	assert.Equal(t, "384Mi", PatchLeafValues(ticket.Patch.Raw)[memLimit])
	assert.True(t, ticket.ExpiresAt.Time.Equal(fixedNow.Add(TicketTTL)))
	assert.NotEmpty(t, ticket.Verdicts)
}

func TestApproval_TheSpecActionIsNotThePlan(t *testing.T) {
	g := standardGate(t)
	ra := proposedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch("999Gi")}
	})

	d := approvalRun(t, g, ra)
	require.True(t, d.Allowed(), d.SkipReason())
	ticket, err := d.Ticket()
	require.NoError(t, err)

	assert.Equal(t, "384Mi", PatchLeafValues(ticket.Patch.Raw)[memLimit], "steps[] is the only plan; spec.action is derived and ignored")
}

func TestApproval_ALegacyActionOnlyPlanIsReadAsOneStep(t *testing.T) {
	g := standardGate(t)
	ra := proposedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Steps = nil
		ra.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch("384Mi")}
	})

	d := approvalRun(t, g, ra)
	require.True(t, d.Allowed(), d.SkipReason())
	ticket, err := d.Ticket()
	require.NoError(t, err)
	assert.Equal(t, "384Mi", PatchLeafValues(ticket.Patch.Raw)[memLimit])
}

func TestApproval_MultipleStepsMergeIntoOneTicket(t *testing.T) {
	withCPU := deployment("256Mi", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = mustQuantity("250m")
	})
	g := newTestGate(t, fakeClient(t, persona("256Mi"), withCPU))
	cpu := personaUpdateStep(jsonOf(`{"spec":{"resources":{"limits":{"cpu":"300m"}}}}`))
	cpu.Order, cpu.ID = 2, "step-2"
	ra := proposedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Steps = []dorguv1.RemediationStep{personaUpdateStep(memPatch("384Mi")), cpu}
	})

	d := approvalRun(t, g, ra)
	require.True(t, d.Allowed(), d.SkipReason())
	ticket, err := d.Ticket()
	require.NoError(t, err)

	values := PatchLeafValues(ticket.Patch.Raw)
	assert.Equal(t, "384Mi", values[memLimit])
	assert.Equal(t, "300m", values[cpuLimit])
}

func TestApproval_ARefusedDecisionCarriesNoTicket(t *testing.T) {
	g := standardGate(t)
	d := approvalRun(t, g, proposedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Gate = nil }))
	_, err := d.Ticket()
	require.Error(t, err)
}

func TestApproval_AnAdvisoryPlanCarriesNoTicket(t *testing.T) {
	g := standardGate(t)
	ra := proposedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Steps = []dorguv1.RemediationStep{{Order: 1, ID: "s", Type: dorguv1.StepTypeManual, Description: "look"}}
	})
	d := approvalRun(t, g, ra)
	require.True(t, d.Allowed(), d.SkipReason())
	_, err := d.Ticket()
	require.Error(t, err)
}

func TestApproval_LiveStateChangedAfterProposalIsReMeasured(t *testing.T) {
	// OP-04 at the gate: proposed 256Mi -> 384Mi, live cut to 64Mi before approval.
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("64Mi")))

	d := approvalRun(t, g, proposedAction())

	if d.Allowed() {
		ticket, err := d.Ticket()
		require.NoError(t, err)
		got, ceiling := mustQuantity(PatchLeafValues(ticket.Patch.Raw)[memLimit]), mustQuantity("128Mi")
		assert.LessOrEqual(t, got.Cmp(ceiling), 0, "a ticket may never exceed 2x the live value")
	}
	assert.NotEqual(t, ResultPass, d.Result, "384Mi against a live 64Mi is 6x")
}

func TestApproval_ObserveModeRefusesAtApproval(t *testing.T) {
	cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModeObserve })
	g := standardGate(t, cp)

	d := approvalRun(t, g, proposedAction())

	assert.Equal(t, ResultRefuse, d.Result)
	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailSelfHealingMode, guardrail)
}

func TestApproval_ASecondApprovedPlanForOnePersonaIsRefused(t *testing.T) {
	// OP-05 at the gate: another plan for the persona is already Applying.
	g := standardGate(t, sibling("other", "Applying"))

	d := approvalRun(t, g, proposedAction())

	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailConcurrent, guardrail)
}

func TestApproval_ATicketedSiblingBlocksToo(t *testing.T) {
	g := standardGate(t, sibling("other", "Ticketed"))
	d := approvalRun(t, g, proposedAction())
	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailConcurrent, guardrail)
}

// --- M3: a persona-only baseline is not trusted when ambiguity is known -----------

func TestM3_AmbiguousWorkloadRefusesAPersonaOnlyBaselineAtProposal(t *testing.T) {
	second := deployment("256Mi", canaryOf)
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("256Mi"), second))

	// 75% below the (stale) persona value would pass a persona-only baseline.
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("128Mi"))), PhaseProposal)

	assert.Equal(t, ResultRefuse, d.Result)
	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailWorkloadAmbiguous, guardrail)
}

func TestM3_AmbiguityDoesNotRefuseAnAdvisoryOnlyPlan(t *testing.T) {
	second := deployment("256Mi", canaryOf)
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("256Mi"), second))
	advisory := planCandidate(dorguv1.RemediationStep{Order: 1, ID: "s", Type: dorguv1.StepTypeManual, Description: "look"})

	d := run(t, g, advisory, PhaseProposal)

	assert.NotEqual(t, "refuse", verdictKind(d, GuardrailWorkloadAmbiguous))
}

// --- L4: a rolled-back remediation holds the persona quiet ------------------------

func TestL4_ARolledBackSiblingStartsTheCooldown(t *testing.T) {
	g := standardGate(t, sibling("undone", "RolledBack"))

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

	assert.Equal(t, "refuse", verdictKind(d, GuardrailFailedCooldown))
}

func TestL4_ARolledBackSiblingPastTheCooldownDoesNot(t *testing.T) {
	old := sibling("undone", "RolledBack", func(ra *dorguv1.RemediationAction) {
		ra.CreationTimestamp = metav1.NewTime(fixedNow.Add(-2 * FailedCooldown))
	})
	g := standardGate(t, old)
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
	assert.Equal(t, "pass", verdictKind(d, GuardrailFailedCooldown))
}

func TestL4_AGateRefusalAtApprovalStartsNoCooldown(t *testing.T) {
	refused := sibling("refused", "Failed", func(ra *dorguv1.RemediationAction) {
		ra.Status.Conditions = []metav1.Condition{{
			Type: "Applied", Status: metav1.ConditionFalse, Reason: dorguv1.ReasonGateRefusedAtApproval,
		}}
	})
	g := standardGate(t, refused)
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
	assert.Equal(t, "pass", verdictKind(d, GuardrailFailedCooldown))
}

// --- R11: dedup, concurrency and the rate limit read the API server, not the cache -

func TestR11_SiblingReadsUseTheUncachedReader(t *testing.T) {
	// The cached client lags: it cannot see the sibling the API server has.
	truth := fakeClient(t, persona("256Mi"), deployment("256Mi"), sibling("in-flight", "Applying"))
	lagging := interceptor.NewClient(fakeClient(t, persona("256Mi"), deployment("256Mi")), interceptor.Funcs{})

	cached := newTestGate(t, lagging)
	assert.Equal(t, "pass", verdictKind(run(t, cached, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal), GuardrailConcurrent),
		"the cache alone misses the in-flight sibling")

	g := newTestGate(t, lagging, WithReader(truth))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailConcurrent))
}

func TestR11_AnUnreadableAPIServerRefusesRatherThanPassing(t *testing.T) {
	broken := interceptor.NewClient(fakeClient(t, persona("256Mi"), deployment("256Mi")), interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			if _, ok := list.(*dorguv1.RemediationActionList); ok {
				return assert.AnError
			}
			return nil
		},
	})
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")), WithReader(broken))

	d, err := g.Run(context.Background(), planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

	require.Error(t, err, "an uncached read that fails means the gate cannot decide")
	assert.Nil(t, d)
}

func TestR11_ApprovalReadsTheLiveWorkloadUncached(t *testing.T) {
	stale := fakeClient(t, persona("256Mi"), deployment("256Mi", func(d *appsv1.Deployment) { d.UID = "stale-uid" }))
	truth := fakeClient(t, persona("256Mi"), deployment("256Mi"))
	g := newTestGate(t, stale, WithReader(truth))

	d := approvalRun(t, g, proposedAction())

	assert.True(t, d.Allowed(), "the re-check decides on the uncached read: %s", d.SkipReason())
}

// --- the ticket at apply time -----------------------------------------------------

func ticketedAction(mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	return proposedAction(append([]func(*dorguv1.RemediationAction){func(ra *dorguv1.RemediationAction) {
		ra.Status.Phase = "Ticketed"
		ra.Status.ApplyTicket = &dorguv1.ApplyTicket{
			WorkloadUID: "uid-1", Container: testPersona, Patch: memPatch("384Mi"),
			ObservedResourceVersion: "42", ExpiresAt: metav1.NewTime(fixedNow.Add(TicketTTL)),
		}
		ra.Status.Gate.PrePatchState = memPatch("256Mi")
	}}, mutate...)...)
}

func checkTicket(t *testing.T, g *Gate, ra *dorguv1.RemediationAction) *TicketProblem {
	t.Helper()
	problem, err := g.CheckTicket(context.Background(), ra)
	require.NoError(t, err)
	return problem
}

func TestTicket_AValidTicketStands(t *testing.T) {
	assert.Nil(t, checkTicket(t, standardGate(t), ticketedAction()))
}

func TestTicket_RefusalsAreEnumerated(t *testing.T) {
	g := standardGate(t)
	cases := map[string]struct {
		mutate    func(*dorguv1.RemediationAction)
		guardrail string
	}{
		"no ticket": {func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket = nil }, GuardrailTicket},
		"expired": {func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(fixedNow.Add(-time.Second))
		}, GuardrailTicket},
		"no expiry":   {func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.ExpiresAt = metav1.Time{} }, GuardrailTicket},
		"unbound":     {func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.WorkloadUID = "" }, GuardrailNoProposalRecord},
		"other UID":   {func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.WorkloadUID = "uid-other" }, GuardrailLiveWorkload},
		"empty patch": {func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.Patch = nil }, GuardrailPatchShape},
		"policy patch": {func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.Patch = jsonOf(`{"spec":{"policies":{"security":{"runAsNonRoot":false}}}}`)
		}, GuardrailPatchShape},
		"null delete": {func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.Patch = jsonOf(`{"spec":{"resources":{"limits":{"cpu":null}}}}`)
		}, GuardrailPatchShape},
		"dotted key": {func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.Patch = jsonOf(`{"spec.resources":{"limits":{"cpu":"1"}}}`)
		}, GuardrailPatchShape},
	}
	for name, tc := range cases {
		problem := checkTicket(t, g, ticketedAction(tc.mutate))
		require.NotNil(t, problem, name)
		assert.Equal(t, tc.guardrail, problem.Guardrail, name)
	}
}

func TestTicket_ANilGateRefuses(t *testing.T) {
	var g *Gate
	problem, err := g.CheckTicket(context.Background(), ticketedAction())
	require.NoError(t, err)
	require.NotNil(t, problem)
}

func TestTicket_ADeletedOrAmbiguousWorkloadRefuses(t *testing.T) {
	gone := newTestGate(t, fakeClient(t, persona("256Mi")))
	problem := checkTicket(t, gone, ticketedAction())
	require.NotNil(t, problem)
	assert.Equal(t, GuardrailWorkloadGone, problem.Guardrail)

	second := deployment("256Mi", canaryOf)
	amb := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), second))
	problem = checkTicket(t, amb, ticketedAction())
	require.NotNil(t, problem)
	assert.Equal(t, GuardrailWorkloadAmbiguous, problem.Guardrail)
}

func TestTicket_AnUnreadableWorkloadIsAnErrorNotAPass(t *testing.T) {
	broken := failingListClient(t, func(l client.ObjectList) bool { _, ok := l.(*appsv1.DeploymentList); return ok })
	g := newTestGate(t, broken)
	_, err := g.CheckTicket(context.Background(), ticketedAction())
	require.Error(t, err)
}

func managed(manager string, at time.Time, subresource string) metav1.ManagedFieldsEntry {
	ts := metav1.NewTime(at)
	return metav1.ManagedFieldsEntry{
		Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1",
		FieldsType: "FieldsV1", Time: &ts, Subresource: subresource,
	}
}

// withManagedFields returns a client whose Deployment lists carry the given
// managedFields: the fake client drops them, and the API server sets them.
func withManagedFields(t *testing.T, entries []metav1.ManagedFieldsEntry, objs ...client.Object) client.Client {
	t.Helper()
	return interceptor.NewClient(fakeClient(t, objs...), interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := inner.List(ctx, list, opts...); err != nil {
				return err
			}
			if deployments, ok := list.(*appsv1.DeploymentList); ok {
				for i := range deployments.Items {
					deployments.Items[i].ManagedFields = entries
				}
			}
			return nil
		},
	})
}

func TestTicket_AWorkloadChangedBySomeoneElseSinceTheTicketIsStale(t *testing.T) {
	issued := fixedNow
	changed := deployment("256Mi", func(d *appsv1.Deployment) { d.ResourceVersion = "99" })
	g := newTestGate(t, withManagedFields(t,
		[]metav1.ManagedFieldsEntry{managed("kubectl-edit", issued.Add(time.Second), "")}, persona("256Mi"), changed))

	problem := checkTicket(t, g, ticketedAction())

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailLiveWorkload, problem.Guardrail)
}

func TestTicket_DorgusOwnPatchAndStatusWritesAreNotSomeoneElse(t *testing.T) {
	issued := fixedNow
	touched := deployment("256Mi", func(d *appsv1.Deployment) { d.ResourceVersion = "99" })
	g := newTestGate(t, withManagedFields(t, []metav1.ManagedFieldsEntry{
		managed(DorguFieldManager, issued.Add(time.Second), ""),
		managed("kube-controller-manager", issued.Add(2*time.Second), "status"),
		managed("kubectl-edit", issued.Add(-time.Hour), ""),
	}, persona("256Mi"), touched))

	assert.Nil(t, checkTicket(t, g, ticketedAction()),
		"the CLI's own patch moves the resourceVersion; that is the approved change, not divergence")
}

func TestWorkloadDivergence(t *testing.T) {
	issued := fixedNow
	replaced := deployment("256Mi", func(d *appsv1.Deployment) { d.UID = "uid-new" })
	g := newTestGate(t, fakeClient(t, persona("256Mi"), replaced))
	problem, err := g.WorkloadDivergence(context.Background(), ticketedAction())
	require.NoError(t, err)
	require.NotNil(t, problem)

	edited := deployment("256Mi", func(d *appsv1.Deployment) { d.ResourceVersion = "77" })
	g = newTestGate(t, withManagedFields(t,
		[]metav1.ManagedFieldsEntry{managed("someone", issued.Add(time.Minute), "")}, persona("256Mi"), edited))
	problem, err = g.WorkloadDivergence(context.Background(), ticketedAction())
	require.NoError(t, err)
	require.NotNil(t, problem)

	same := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	problem, err = same.WorkloadDivergence(context.Background(), ticketedAction())
	require.NoError(t, err)
	assert.Nil(t, problem)

	// An expired ticket is still comparable: divergence is not about expiry.
	expired := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(fixedNow.Add(-time.Hour))
	})
	problem, err = same.WorkloadDivergence(context.Background(), expired)
	require.NoError(t, err)
	assert.Nil(t, problem)

	var nilGate *Gate
	_, err = nilGate.WorkloadDivergence(context.Background(), ticketedAction())
	require.Error(t, err)
	_, err = same.WorkloadDivergence(context.Background(), proposedAction())
	require.Error(t, err)
}

func TestWorkloadDivergence_NoPersonaNameIsGone(t *testing.T) {
	g := standardGate(t)
	ra := ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Spec.PersonaRef.Name = "" })
	problem, err := g.WorkloadDivergence(context.Background(), ra)
	require.NoError(t, err)
	require.NotNil(t, problem)
	assert.Equal(t, GuardrailWorkloadGone, problem.Guardrail)
}

func TestTicketKeyFallsBackToTheActionNamespace(t *testing.T) {
	g := standardGate(t)
	ra := ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Spec.PersonaRef.Namespace = "" })
	assert.Nil(t, checkTicket(t, g, ra))
}

// --- the rollback baseline ---------------------------------------------------------

func TestRollbackBaseline(t *testing.T) {
	baseline, problem := RollbackBaseline(ticketedAction())
	require.Nil(t, problem)
	assert.Equal(t, "256Mi", PatchLeafValues(baseline.Raw)[memLimit])

	_, problem = RollbackBaseline(ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket = nil }))
	require.NotNil(t, problem, "no ticket, nothing was applied, nothing to roll back")
	assert.Equal(t, GuardrailTicket, problem.Guardrail)

	_, problem = RollbackBaseline(ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Gate = nil }))
	require.NotNil(t, problem)
	assert.Equal(t, GuardrailNoBaseline, problem.Guardrail)

	_, problem = RollbackBaseline(ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.Gate.PrePatchState = jsonOf(`{"spec":{"policies":{"security":{"runAsNonRoot":false}}}}`)
	}))
	require.NotNil(t, problem, "the baseline is held to the same allowlist as the patch")
	assert.Equal(t, GuardrailPatchShape, problem.Guardrail)

	_, problem = RollbackBaseline(ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.Gate.PrePatchState = jsonOf(`[]`)
	}))
	require.NotNil(t, problem)

	expired := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(fixedNow.Add(-24 * time.Hour))
	})
	_, problem = RollbackBaseline(expired)
	assert.Nil(t, problem, "a heal that degrades after the ticket expired must still roll back")
}

// --- derived action and reasons -----------------------------------------------------

func TestDeriveAction(t *testing.T) {
	cpu := personaUpdateStep(jsonOf(`{"spec":{"resources":{"limits":{"cpu":"300m"}}}}`))
	cpu.Order = 2
	cpu.PrePatchState = jsonOf(`{"spec":{"resources":{"limits":{"cpu":"250m"}}}}`)
	mem := personaUpdateStep(memPatch("384Mi"))
	mem.PrePatchState = memPatch("256Mi")

	action := DeriveAction([]dorguv1.RemediationStep{cpu, mem})

	assert.Equal(t, dorguv1.ActionTypePersonaUpdate, action.Type)
	values := PatchLeafValues(action.Patch.Raw)
	assert.Equal(t, "384Mi", values[memLimit])
	assert.Equal(t, "300m", values[cpuLimit])
	pre := PatchLeafValues(action.PrePatchState.Raw)
	assert.Equal(t, "256Mi", pre[memLimit])
	assert.Equal(t, "250m", pre[cpuLimit])

	advisory := DeriveAction([]dorguv1.RemediationStep{{Order: 1, ID: "s", Type: dorguv1.StepTypeManual}})
	assert.Equal(t, dorguv1.ActionTypeNotification, advisory.Type)
	assert.Equal(t, dorguv1.ActionTypeNotification, DeriveAction(nil).Type)
}

func TestReasonEnum(t *testing.T) {
	assert.Equal(t, "BlastRadius", ReasonEnum(GuardrailBlastRadius))
	assert.Equal(t, "WorkloadGone", ReasonEnum(GuardrailWorkloadGone))
	assert.Equal(t, "NoProposalRecord", ReasonEnum(GuardrailNoProposalRecord))
	assert.Equal(t, "SelfHealingMode", ReasonEnum(GuardrailSelfHealingMode))
	assert.Equal(t, "Refused", ReasonEnum(""))
}
