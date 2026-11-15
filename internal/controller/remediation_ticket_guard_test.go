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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

type stubTicketGuard struct{ blocked *intake.Failure }

func (s stubTicketGuard) TicketsBlocked() *intake.Failure { return s.blocked }

// P2-D8: with the admission policies missing the operator issues no ticket. An approved
// action waits as it is, nothing is applied, nothing fails and nothing is rewritten; it
// proceeds the moment the guard opens.
func TestApproved_NoTicketIsIssuedWhileTheAdmissionPoliciesAreMissing(t *testing.T) {
	ra := lcRA("Approved")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	recorder := record.NewFakeRecorder(10)
	rc.Recorder = recorder
	failure := &intake.Failure{Check: "admission-policies", Reason: intake.ReasonAdmissionPoliciesMissing, Message: "the policies are not installed"}
	rc.TicketGuard = stubTicketGuard{blocked: failure}

	res, got := lcReconcile(t, rc, ra)

	assert.Equal(t, RemediationPhaseApproved, got.Status.Phase, "left Approved, not failed and not ticketed")
	assert.Nil(t, got.Status.ApplyTicket, "no ticket")
	assert.Nil(t, lcCondition(got, dorguv1.ConditionTicketed))
	assert.Equal(t, lcBaseline, lcPersonaMemory(t, c), "nothing was applied")
	assert.Positive(t, res.RequeueAfter, "it looks again, so the approval proceeds once the policies are back")
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "Warning")
		assert.Contains(t, event, intake.ReasonAdmissionPoliciesMissing)
	default:
		t.Error("a Warning Event says why the approved action is waiting")
	}

	// The policies come back: the same action is ticketed and applied.
	rc.TicketGuard = stubTicketGuard{}
	_, got = lcReconcile(t, rc, ra)
	assert.Equal(t, RemediationPhaseApplying, got.Status.Phase)
	require.NotNil(t, got.Status.ApplyTicket)
}

// An action already Ticketed keeps going: the guard stops tickets being issued, it does not
// undo one that was.
func TestTicketed_AnIssuedTicketIsStillAppliedWhileTheGuardIsClosed(t *testing.T) {
	ra := lcRA("Ticketed")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rc.TicketGuard = stubTicketGuard{blocked: &intake.Failure{Reason: intake.ReasonAdmissionPoliciesMissing}}

	_, got := lcReconcile(t, rc, ra)

	assert.Equal(t, RemediationPhaseApplying, got.Status.Phase)
	assert.Equal(t, lcApplied, lcPersonaMemory(t, c))
}

// An advisory plan has no ticket to issue, so it is not held up.
func TestApproved_AnAdvisoryPlanIsAcknowledgedWhileTheGuardIsClosed(t *testing.T) {
	ra := lcRA("Approved", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Steps = []dorguv1.RemediationStep{{Order: 1, ID: "s", Type: "manual", Description: "look"}}
		ra.Spec.Action = dorguv1.RemediationActionDetail{Type: "notification"}
	})
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rc.TicketGuard = stubTicketGuard{blocked: &intake.Failure{Reason: intake.ReasonAdmissionPoliciesMissing}}

	_, got := lcReconcile(t, rc, ra)

	assert.Equal(t, RemediationPhaseAcknowledged, got.Status.Phase)
}
