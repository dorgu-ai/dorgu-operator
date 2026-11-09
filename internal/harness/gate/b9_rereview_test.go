package gate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// B9 PR #60 re-review (head c084586), judgement call (b). The approval re-check and
// the apply ticket are built from the RemediationAction alone: Facts (the incident,
// its signal, its severity, the diagnosis) are filled in only at Proposal. So
// neither binds to the incident's CURRENT state, and an incident that escalated
// after the proposal is invisible to them. Safe today because only an OOM
// (resource-adjustment) plan is auto-applicable and nothing escalates out of OOM;
// pinned here so that a future auto-applicable plan for a milder shape fails this
// test and forces a decision.
func TestRereview_ApprovalAndTicketCandidatesDoNotReadTheIncident_Characterization(t *testing.T) {
	ra := &dorguv1.RemediationAction{}
	ra.Spec.IncidentRef = dorguv1.IncidentReference{Name: "im-shop-api-readinessnotready-f4653f1c2af7", Namespace: "shop"}

	for name, cand := range map[string]Candidate{"approval": ApprovalCandidate(ra), "ticket": TicketCandidate(ra)} {
		assert.Nil(t, cand.Facts.Incident, name+": no incident object")
		assert.Empty(t, cand.Facts.PrimarySignal, name+": no signal")
		assert.Empty(t, cand.Facts.Severity, name+": no severity")
	}
}
