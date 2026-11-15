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
	"fmt"
	"testing"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// OP-17 to OP-19 are the operator's own look at who approved (task 2.6, defence in depth).
// They run on the API server WITHOUT the admission policies, on purpose: they model a
// cluster where an approval got in anyway (the policies were missing for a minute, a
// policy was edited, a legacy role still lets a service account patch status), and prove the
// operator does not turn it into an apply ticket. The admission side is ID-01 to ID-03.

// approvedBy runs one approval written by approver on an action proposed by proposer ("" for
// an action with no recorded proposer) through the real RemediationController.
func approvedBy(t *testing.T, id, proposer, approver string) (detail string, blocked bool) {
	t.Helper()
	ns := newNamespace(t, id)
	createPersona(t, ns)
	createIncident(t, ns)
	createDeployment(t, ns)

	var opts []raOption
	if proposer != "" {
		opts = append(opts, withProposer(proposer))
	}
	ra := createRA(t, newRA(ns, "approved", opts...), "Pending")
	approveAs(t, ra, approver)

	_, err := reconcile(t, newRemediationController(), ra)

	mem := personaMemory(t, ns)
	byApproverCheck := false
	for _, c := range ra.Status.Conditions {
		byApproverCheck = byApproverCheck ||
			(c.Type == dorguv1.ConditionGateRefused && c.Reason == dorguv1.ReasonApproverNotPermitted)
	}
	// Refused by the approver check itself, not by some unrelated guardrail that would hide
	// the attack.
	blocked = mem == baselineMemory && ra.Status.Phase == phaseFailed && ra.Status.ApplyTicket == nil &&
		refusedAtApproval(ra) && byApproverCheck
	return fmt.Sprintf("approver %q, proposer %q: persona memory %s -> %s, RA phase %q, ticket %v, reconcile err %v",
		approver, proposer, baselineMemory, mem, ra.Status.Phase, ra.Status.ApplyTicket != nil, err), blocked
}

// OP-17: the approver is the proposer. Nothing may be ticketed or applied.
func TestOP17ApproverIsTheProposer(t *testing.T) {
	detail, blocked := approvedBy(t, "op-17", "agent-x", "agent-x")
	Expect(t, "OP-17", blocked, detail)
}

// OP-18: the approval names a service account, as a status patch by an agent's token would.
func TestOP18ServiceAccountApprover(t *testing.T) {
	detail, blocked := approvedBy(t, "op-18", "agent-x", "system:serviceaccount:agents:agent-x")
	Expect(t, "OP-18", blocked, detail)
}

// OP-19: the operator approves its own action (no recorded proposer: made before provenance
// existed, so the operator is the proposer).
func TestOP19OperatorApprovesItsOwnAction(t *testing.T) {
	detail, blocked := approvedBy(t, "op-19", "", operatorUsername)
	Expect(t, "OP-19", blocked, detail)
}
