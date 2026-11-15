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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// C2 review F-2 (task 2.6, defence in depth): the admission policy is where "the approver is
// a person who is not the proposer" is enforced, and the operator checks it again before it
// issues a ticket. An approval that got in without the policies (a window while they were
// down, an edited policy, a legacy role) must not become an apply ticket.

const (
	opUser   = "system:serviceaccount:dorgu-operator-system:dorgu-operator-controller-manager"
	agentBot = "agent-x"
)

func approvedBy(proposer, approver string) func(*dorguv1.RemediationAction) {
	return func(ra *dorguv1.RemediationAction) {
		now := metav1.Now()
		if proposer != "" {
			ra.Spec.Provenance = &dorguv1.RemediationProvenance{
				Proposal: "p", Proposer: dorguv1.ProposerIdentity{Username: proposer}, Source: dorguv1.ProposalSourceAgent,
			}
		}
		ra.Status.ApprovedBy = approver
		ra.Status.ApprovedAt = &now
	}
}

func TestApproved_TheOperatorRefusesAnApproverThatIsNotAPersonOtherThanTheProposer(t *testing.T) {
	cases := []struct {
		name               string
		proposer, approver string
	}{
		{"approver is the proposer", agentBot, agentBot},
		{"approver is the proposer, padded (the ticket trims it)", agentBot, "  " + agentBot + " "},
		{"a service account approves", agentBot, "system:serviceaccount:ns:bot"},
		{"a system: user, padded", agentBot, " system:kube-controller-manager"},
		{"the operator approves its own action (no provenance)", "", opUser},
		{"the operator is the recorded proposer", opUser, opUser},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ra := lcRA(RemediationPhaseApproved, approvedBy(tc.proposer, tc.approver))
			rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
			rc.OperatorUsername = opUser

			_, got := lcReconcile(t, rc, ra)

			if got.Status.ApplyTicket != nil {
				t.Fatalf("a ticket was issued for approvedBy %q (proposer %q)", tc.approver, tc.proposer)
			}
			if got.Status.Phase != RemediationPhaseFailed {
				t.Fatalf("phase = %q, want Failed (terminal refusal)", got.Status.Phase)
			}
			if !gate.RejectedBeforeApply(got) {
				t.Errorf("the refusal must count as before-apply: no cooldown, no failed-heal outcome")
			}
			refused := findCondition(got.Status.Conditions, dorguv1.ConditionGateRefused)
			if refused == nil || refused.Reason != dorguv1.ReasonApproverNotPermitted || refused.Message == "" {
				t.Errorf("GateRefused condition = %+v, want reason %s with a message", refused, dorguv1.ReasonApproverNotPermitted)
			}
			if outcome := lcIncidentOutcome(t, c); outcome != "" {
				t.Errorf("incident outcome = %q, want none", outcome)
			}
		})
	}
}

// Nobody named as the approver is refused too, terminally and before a ticket: by the gate's
// own NoApprover guardrail, the documented behaviour (CHANGELOG, C3), which the approver
// check leaves to it.
func TestApproved_AnApprovalThatNamesNobodyIsRefusedBeforeATicket(t *testing.T) {
	for _, approver := range []string{"", " \t "} {
		ra := lcRA(RemediationPhaseApproved, approvedBy(agentBot, approver))
		rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
		rc.OperatorUsername = opUser

		_, got := lcReconcile(t, rc, ra)

		refused := findCondition(got.Status.Conditions, dorguv1.ConditionGateRefused)
		if got.Status.ApplyTicket != nil || got.Status.Phase != RemediationPhaseFailed || refused == nil || refused.Reason != "NoApprover" {
			t.Errorf("approver %q: phase %q, ticket %v, GateRefused %+v", approver, got.Status.Phase, got.Status.ApplyTicket != nil, refused)
		}
	}
}

func TestApproved_AnotherPersonsApprovalIsTicketed(t *testing.T) {
	ra := lcRA(RemediationPhaseApproved, approvedBy(agentBot, "alice"))
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rc.OperatorUsername = opUser

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase == RemediationPhaseFailed || findCondition(got.Status.Conditions, dorguv1.ConditionGateRefused) != nil {
		t.Fatalf("a different person's approval was refused: phase %q, GateRefused %+v", got.Status.Phase, findCondition(got.Status.Conditions, dorguv1.ConditionGateRefused))
	}
	if got.Status.ApplyTicket == nil && got.Status.Phase == RemediationPhaseApproved {
		t.Fatalf("no ticket issued for a valid approval")
	}
}

// With no operator username known, an action with no provenance cannot be compared, but a
// service account is still never a person.
func TestApproved_WithoutAnOperatorUsernameASystemApproverIsStillRefused(t *testing.T) {
	ra := lcRA(RemediationPhaseApproved, approvedBy("", opUser))
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.ApplyTicket != nil || got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase %q, ticket %v", got.Status.Phase, got.Status.ApplyTicket != nil)
	}
}
