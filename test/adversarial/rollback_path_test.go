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

package adversarial

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// Ticketed rollback (C6, P2-D10): OP-20 to OP-24.
//
// A rollback is a person's request that the operator restore what it recorded before
// it applied an action. The attacks try to make it restore something Dorgu never
// recorded, roll back what never applied, put a request in someone else's name, let an
// agent ask, or stack rollbacks on one action. The operator-side scenarios run the real
// intake over the envtest API server (no admission policies: they prove the operator's
// own checks and the CRD); the identity ones run against the policy-enforcing server.

const (
	alicePerson = "alice@example.com"
	rollbackRA  = "ra-applied"
)

// intakeProcessor is the real intake the operator runs, over the envtest API server.
func intakeProcessor() *intake.Processor {
	return intake.New(intake.Config{
		Client: k8sClient, Reader: k8sClient, Lister: k8sClient,
		Gate:             testGate(),
		Incidents:        &controller.ProposalIncidents{Client: k8sClient},
		OperatorUsername: operatorUsername,
		Logger:           logr.Discard(),
	})
}

// appliedFixture makes the persona, workload and incident, then a real action that the
// operator proposed, a person approved, and the operator ticketed and applied: its gate
// record and ticket are the real ones. The workload is then given the change (as the
// CLI's heal does with the person's own credentials) and the action ends Completed.
func appliedFixture(t *testing.T, id string) (ns string, ra *dorguv1.RemediationAction) {
	t.Helper()
	ns = newNamespace(t, id)
	createPersona(t, ns)
	createIncident(t, ns)
	deploy := createDeployment(t, ns)

	ra = createRA(t, newRA(ns, rollbackRA, withProposer(operatorUsername)), "Pending")
	approveAs(t, ra, alicePerson)
	if _, err := reconcile(t, newRemediationController(), ra); err != nil {
		t.Fatalf("applying the fixture action: %v", err)
	}
	if ra.Status.ApplyTicket == nil || ra.Status.Gate == nil || ra.Status.Gate.PrePatchState == nil {
		t.Fatalf("the fixture action has no real ticket or baseline (phase %q)", ra.Status.Phase)
	}

	deploy.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
	if err := k8sClient.Update(context.Background(), deploy); err != nil {
		t.Fatalf("giving the workload the change: %v", err)
	}
	ra.Status.Phase = "Completed"
	if err := k8sClient.Status().Update(context.Background(), ra); err != nil {
		t.Fatalf("ending the fixture action Completed: %v", err)
	}
	return ns, ra
}

// rollbackProposalFor is what `dorgu remediation rollback` writes.
func rollbackProposalFor(ns, name, user, action string) *dorguv1alpha1.RemediationProposal {
	return &dorguv1alpha1.RemediationProposal{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: dorguv1alpha1.RemediationProposalSpec{
			Proposer:       dorguv1.ProposerIdentity{Username: user},
			Source:         dorguv1.ProposalSourceRollback,
			IdempotencyKey: name,
			Target:         dorguv1alpha1.ProposalTarget{Kind: kindPersona, Namespace: ns, Name: appName},
			RollbackOf:     &dorguv1alpha1.ActionRef{Name: action},
			Rationale:      "the new limit starves the node",
		},
	}
}

// process creates the proposal and has the real intake decide it.
func process(t *testing.T, p *intake.Processor, prop *dorguv1alpha1.RemediationProposal) *intake.Outcome {
	t.Helper()
	mustCreate(t, prop)
	out, err := p.Process(context.Background(), types.NamespacedName{Namespace: prop.Namespace, Name: prop.Name}, nil)
	if err != nil {
		t.Fatalf("deciding proposal %s: %v", prop.Name, err)
	}
	return out
}

func rollbackActions(t *testing.T, ns string) []dorguv1.RemediationAction {
	t.Helper()
	var list dorguv1.RemediationActionList
	if err := k8sClient.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing actions: %v", err)
	}
	var out []dorguv1.RemediationAction
	for _, ra := range list.Items {
		if ra.Annotations[dorguv1.AnnotationRollbackOf] != "" {
			out = append(out, ra)
		}
	}
	return out
}

// OP-20: a rollback of an action that never applied anything: one still Pending, one
// Failed with no ticket, one Rejected. There is no recorded change, so there is nothing
// to restore; a rollback proposal must not become a way to write a value Dorgu never
// recorded.
func TestOP20RollbackOfAnActionThatNeverApplied(t *testing.T) {
	ns := newNamespace(t, "op-20")
	createPersona(t, ns)
	createIncident(t, ns)
	createDeployment(t, ns)

	// All three are proposed first (a Failed action would start the gate's failure
	// cooldown for the persona), then two of them end without ever applying.
	pending := createRA(t, newRA(ns, "never-applied", withProposer(operatorUsername)), "Pending")
	failed := createRA(t, newRA(ns, "never-ticketed", withProposer(operatorUsername)), "Pending")
	rejected := createRA(t, newRA(ns, "declined", withProposer(operatorUsername)), "Pending")
	failed.Status.Phase = phaseFailed
	if err := k8sClient.Status().Update(context.Background(), failed); err != nil {
		t.Fatalf("failing the action: %v", err)
	}
	setPhase(t, rejected, "Rejected")

	p := intakeProcessor()
	reasons := map[string]string{}
	for _, target := range []*dorguv1.RemediationAction{pending, failed, rejected} {
		out := process(t, p, rollbackProposalFor(ns, "rb-"+target.Name, alicePerson, target.Name))
		reasons[target.Name] = "converted"
		if out.Rejection != nil {
			reasons[target.Name] = out.Rejection.Reason
		}
	}

	blocked := len(rollbackActions(t, ns)) == 0 && personaMemory(t, ns) == baselineMemory
	for _, reason := range reasons {
		blocked = blocked && reason == dorguv1alpha1.ReasonRollbackNotApplied
	}
	Expect(t, "OP-20", blocked, fmt.Sprintf("rollback proposals for never-applied actions ended %v; rollback actions made: %d; persona memory %s",
		reasons, len(rollbackActions(t, ns)), personaMemory(t, ns)))
}

// OP-21: a rollback proposal that carries its own steps: a 16x memory patch, as if the
// requester were choosing what the "restore" writes. The steps are never read: the API
// server refuses the proposal (the CRD rule), and where a cluster's CRD lacks the rule
// the operator derives its own steps (intake unit tests). The reserved source cannot be
// claimed by an ordinary proposal either, so a forged "rollback" label cannot make an
// action look like the operator's own restore.
func TestOP21RollbackWithForgedSteps(t *testing.T) {
	ns, _ := appliedFixture(t, "op-21")

	forged := rollbackProposalFor(ns, "rb-forged-steps", alicePerson, rollbackRA)
	forged.Spec.Steps = []dorguv1.RemediationStep{{
		Order: 1, ID: "evil", Type: personaUpdate, AutoExecutable: true, Description: "restore", Patch: memoryPatch("4Gi"),
	}}
	stepsErr := k8sClient.Create(context.Background(), forged)

	claim := rollbackProposalFor(ns, "rb-claimed-source", alicePerson, rollbackRA)
	claim.Spec.RollbackOf = nil
	claim.Spec.Steps = []dorguv1.RemediationStep{{
		Order: 1, ID: "evil", Type: personaUpdate, AutoExecutable: true, Description: "restore", Patch: memoryPatch("4Gi"),
	}}
	claimErr := k8sClient.Create(context.Background(), claim)

	// And the honest rollback, to see what the restore actually is.
	out := process(t, intakeProcessor(), rollbackProposalFor(ns, "rb-honest", alicePerson, rollbackRA))
	restores := ""
	if out.Action != nil && len(out.Action.Spec.Steps) == 1 {
		restores = string(out.Action.Spec.Steps[0].Patch.Raw)
	}

	blocked := apierrors.IsInvalid(stepsErr) && apierrors.IsInvalid(claimErr) &&
		out.Action != nil && restores == `{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}` &&
		personaMemory(t, ns) == "512Mi"
	Expect(t, "OP-21", blocked, fmt.Sprintf(
		"steps on a rollback err=%v; claimed source err=%v; the honest rollback restores %s", stepsErr, claimErr, restores))
}

// OP-22: a ServiceAccount asks for a rollback. It holds every right the approver role
// grants plus create on proposals, so only being a service account can stop it. A
// rollback ends as an action the operator proposed itself, which one person approves
// alone: letting an agent start one would let it launder its request into the operator's
// name. Denied at admission, and the same account may still propose as itself.
func TestOP22RollbackRequestedByAServiceAccount(t *testing.T) {
	e := admissionEnvFor(t)
	ns, _ := e.NewNamespace(context.Background(), "op-22")

	rules := append(append([]rbacv1.PolicyRule{}, approverRules...), rbacv1.PolicyRule{
		APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationproposals"}, Verbs: []string{"create", "get", "list"},
	})
	botName := "system:serviceaccount:" + ns + ":agent"
	bot := idUser(t, e, ns, botName,
		[]string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"}, rules)

	prop := admissionenv.NewProposal(ns, "agent-rollback", botName)
	prop.Spec.Source = dorguv1.ProposalSourceRollback
	prop.Spec.Steps = nil
	prop.Spec.RollbackOf = &dorguv1alpha1.ActionRef{Name: rollbackRA}
	err := bot.Create(context.Background(), prop)

	controlErr := bot.Create(context.Background(), admissionenv.NewProposal(ns, "agent-ordinary", botName))
	Expect(t, "OP-22", admissionenv.IsDorguDenial(err, "ServiceAccountCannotApprove") && controlErr == nil,
		fmt.Sprintf("service account rollback request err=%v; its ordinary proposal err=%v", err, controlErr))
}

// OP-23: a rollback whose requester is forged. requestedBy is the proposer of the
// proposal, which admission holds equal to the authenticated user, so mallory cannot put
// victim's name on her request; and no other field of the proposal reaches requestedBy.
func TestOP23RollbackWithAForgedRequester(t *testing.T) {
	e := admissionEnvFor(t)
	ns, _ := e.NewNamespace(context.Background(), "op-23")

	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationactions"}, Verbs: []string{"get", "list", "approve", "reject"}},
		{APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationproposals"}, Verbs: []string{"create", "get", "list"}},
	}
	mallory := idUser(t, e, ns, "mallory@example.com", nil, rules)

	forged := admissionenv.NewProposal(ns, "as-victim", "victim@example.com")
	forged.Spec.Source = dorguv1.ProposalSourceRollback
	forged.Spec.Steps = nil
	forged.Spec.RollbackOf = &dorguv1alpha1.ActionRef{Name: rollbackRA}
	forgedErr := mallory.Create(context.Background(), forged)

	// The control: the same user, as herself.
	own := admissionenv.NewProposal(ns, "as-herself", "mallory@example.com")
	own.Spec.Source = dorguv1.ProposalSourceRollback
	own.Spec.Steps = nil
	own.Spec.RollbackOf = &dorguv1alpha1.ActionRef{Name: rollbackRA}
	ownErr := mallory.Create(context.Background(), own)

	Expect(t, "OP-23", admissionenv.IsDorguDenial(forgedErr, "ProposerMismatch") && ownErr == nil,
		fmt.Sprintf("rollback written as victim err=%v; as herself err=%v", forgedErr, ownErr))
}

// OP-24: a double rollback: two people ask for a rollback of the same action, one after
// the other and at the same moment. Exactly one rollback action may exist, or the
// restore would be applied twice from two approvals.
func TestOP24DoubleRollback(t *testing.T) {
	ns, _ := appliedFixture(t, "op-24")
	p := intakeProcessor()

	first := process(t, p, rollbackProposalFor(ns, "rb-first", alicePerson, rollbackRA))
	second := process(t, p, rollbackProposalFor(ns, "rb-second", "bob@example.com", rollbackRA))

	sequential := first.Action != nil && second.Rejection != nil &&
		second.Rejection.Reason == dorguv1alpha1.ReasonRollbackAlreadyOpen

	// The same moment, on another action.
	ns2, _ := appliedFixture(t, "op-24b")
	p2 := intakeProcessor()
	var wg sync.WaitGroup
	for i := range 4 {
		prop := rollbackProposalFor(ns2, fmt.Sprintf("rb-race-%d", i), fmt.Sprintf("user-%d@example.com", i), rollbackRA)
		mustCreate(t, prop)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p2.Process(context.Background(), client.ObjectKeyFromObject(prop), nil)
		}()
	}
	wg.Wait()

	Expect(t, "OP-24", sequential && len(rollbackActions(t, ns)) == 1 && len(rollbackActions(t, ns2)) == 1,
		fmt.Sprintf("sequential: first converted=%v second=%v; rollback actions %d; racing four: %d",
			first.Action != nil, second.Rejection, len(rollbackActions(t, ns)), len(rollbackActions(t, ns2))))
}
