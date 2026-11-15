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
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// C6b review F-5: two paths the OP-29..33 scenarios do not reach, on a real API server
// and the real controller. They are not attack scenarios, so they carry no OP number:
// the first writes the clamp-with-advice result into a real RemediationAction (the CRD
// validates the new safety entry and the new verdict reason), the second runs a plan
// that was Pending when the rollback completed through the real approval.

type fixedPlanner struct{ plan *planner.RemediationPlan }

func (f fixedPlanner) PlanRemediation(context.Context, planner.RemediationContext) (*planner.RemediationPlan, error) {
	return f.plan, nil
}

func memoryPlanWithAdvice() *planner.RemediationPlan {
	return &planner.RemediationPlan{
		RootCause: "memory limit too low", Confidence: 0.86,
		Steps: []planner.PlannedStep{
			{
				Order: 1, Type: "persona-update", Risk: "low", Description: "Raise the memory limit to " + raisedMemory,
				Rationale: "the container needs " + raisedMemory,
				Patch:     json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"` + raisedMemory + `"}}}}`),
			},
			{
				Order: 2, Type: "manual", Risk: "low",
				Description: "Watch the container for ten minutes.", Rationale: "to confirm the cause",
			},
		},
	}
}

func TestRolledBackValueAdviceIsKeptAndPersistedInARealAction(t *testing.T) {
	ns, rollback := completedRollbackFixture(t, "rbv-advice")
	diag, incident := nextIncident(t, ns)
	proposer := remediation.NewProposer(k8sClient, testGate(), logr.Discard(),
		remediation.WithIntake(intakeProcessor()), remediation.WithPlanner(fixedPlanner{memoryPlanWithAdvice()}))

	result, err := proposer.Propose(testCtx(t), diag, incident)
	if err != nil {
		t.Fatalf("proposing after the rollback: %v", err)
	}
	if !result.Proposed || result.Action == nil {
		t.Fatalf("the advice must survive as a proposal: reason %q (%s)", result.Reason, result.SkipReason)
	}

	var stored dorguv1.RemediationAction
	if err := k8sClient.Get(testCtx(t), client.ObjectKeyFromObject(result.Action), &stored); err != nil {
		t.Fatalf("the API server did not keep the action (CRD validation of the new entries): %v", err)
	}
	if stored.HasAutoApplicableChange() {
		t.Errorf("nothing is left for the operator to apply, but the stored action carries a change")
	}
	for _, step := range stored.Spec.Steps {
		if step.Patch != nil && strings.Contains(string(step.Patch.Raw), raisedMemory) {
			t.Errorf("a stored patch still offers %s: %s", raisedMemory, step.Patch.Raw)
		}
	}
	if len(stored.Spec.Steps) != 2 {
		t.Fatalf("stored steps = %d, want the persona step (withheld) and the advice", len(stored.Spec.Steps))
	}
	rejected := stored.Spec.Steps[0].Safety
	if len(rejected) != 1 || rejected[0].Verdict != dorguv1.SafetyVerdictRejected ||
		!strings.Contains(rejected[0].Message, rollback.Name) {
		t.Errorf("the withheld step must record Dorgu's sentence naming the rollback: %+v", rejected)
	}
	if stored.Spec.Steps[1].Type != dorguv1.StepTypeManual {
		t.Errorf("the advice step was not kept: %+v", stored.Spec.Steps[1])
	}
	if !strings.Contains(stored.Spec.Explanation, "rolled back by a person") {
		t.Errorf("the explanation does not say why: %q", stored.Spec.Explanation)
	}
}

func TestRolledBackValuePlanPendingWhenTheRollbackCompletedIsRefusedAtApproval(t *testing.T) {
	ns := appliedFixture(t, "rbv-pending")
	out := process(t, intakeProcessor(), rollbackProposalFor(ns, "rb-person", alicePerson, rollbackRA))
	if out.Action == nil {
		t.Fatalf("the rollback was not converted: %+v %s", out.Rejection, out.Message)
	}
	rollback := out.Action
	approveAs(t, rollback, alicePerson)
	if _, err := reconcile(t, newRemediationController(), rollback); err != nil {
		t.Fatalf("applying the rollback: %v", err)
	}
	setLiveMemory(t, ns, baselineMemory)

	// A plan for the same move is proposed and left Pending while the rollback is still
	// in flight (nothing is withheld yet), then the rollback completes.
	setPhase(t, rollback, "Pending")
	pending := createRA(t, newRA(ns, "ra-pending-same-raise", withProposer(operatorUsername)), "Pending")
	setPhase(t, rollback, "Completed")

	approveAs(t, pending, alicePerson)
	if _, err := reconcile(t, newRemediationController(), pending); err != nil {
		t.Fatalf("approving the old plan: %v", err)
	}

	if pending.Status.Phase != "Failed" {
		t.Fatalf("phase = %q, want Failed: the plan repeats a value a person rolled back", pending.Status.Phase)
	}
	if pending.Status.ApplyTicket != nil || pending.Status.AppliedAt != nil {
		t.Errorf("a ticket was issued or the plan applied: %+v", pending.Status)
	}
	var reason string
	for _, c := range pending.Status.Conditions {
		if c.Type == dorguv1.ConditionGateRefused {
			reason = c.Reason + ": " + c.Message
		}
	}
	if !strings.HasPrefix(reason, "RolledBackValue") || !strings.Contains(reason, "rolled back by a person") {
		t.Errorf("GateRefused = %q, want reason RolledBackValue with Dorgu's sentence", reason)
	}
	if mem := personaMemory(t, ns); mem != baselineMemory {
		t.Errorf("the persona holds %s, want %s: a refused approval must not apply", mem, baselineMemory)
	}
}
