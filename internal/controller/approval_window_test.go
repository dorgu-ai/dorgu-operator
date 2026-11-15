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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

// C2 review F-4: an approval written while the admission guard was not ready (closed, in its
// startup window, or open but unable to have noticed a deleted policy) was not checked by
// working policies. When the guard is ready again the action goes back to Pending and has to
// be approved again, and what says WHEN it was approved is the API server's managedFields
// stamp for the status write, never the person-writable approvedAt.

var windowStart = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// notReadyBetween is a guard that was not ready from from to to.
func notReadyBetween(from, to time.Time) func(time.Time) bool {
	return func(t time.Time) bool { return !t.Before(from) && !t.After(to) }
}

// statusWrite is a managedFields entry for a manager that wrote the approval through the
// status subresource at the given time. fields are the status fields it owns.
func statusWrite(manager string, at *time.Time, fields ...string) metav1.ManagedFieldsEntry {
	var owned []string
	for _, f := range fields {
		owned = append(owned, `"f:`+f+`":{}`)
	}
	e := metav1.ManagedFieldsEntry{
		Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "dorgu.io/v1",
		FieldsType: "FieldsV1", Subresource: "status",
		FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{` + strings.Join(owned, ",") + `}}`)},
	}
	if at != nil {
		e.Time = &metav1.Time{Time: *at}
	}
	return e
}

func approvedWithManagedFields(entries ...metav1.ManagedFieldsEntry) *dorguv1.RemediationAction {
	return lcRA(RemediationPhaseApproved, approvedBy(agentBot, "alice"), func(ra *dorguv1.RemediationAction) {
		ra.ManagedFields = entries
	})
}

func guardedController(t *testing.T, ra *dorguv1.RemediationAction, guard TicketGuard) (*RemediationController, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rc.OperatorUsername = opUser
	recorder := record.NewFakeRecorder(20)
	rc.Recorder = recorder
	rc.TicketGuard = guard
	return rc, c, recorder
}

func TestApproved_AnApprovalWrittenInAGuardWindowGoesBackToPending(t *testing.T) {
	at := windowStart.Add(30 * time.Second)
	ra := approvedWithManagedFields(
		statusWrite("operator", &windowStart, "gate", "conditions"),
		statusWrite("dorgu-cli", &at, "phase", "approvedBy", "approvedAt"),
	)
	rc, c, events := guardedController(t, ra, stubTicketGuard{notReady: notReadyBetween(windowStart, windowStart.Add(time.Minute))})

	_, got := lcReconcile(t, rc, ra)

	assert.Equal(t, RemediationPhasePending, got.Status.Phase, "sent back for a new approval")
	assert.Empty(t, got.Status.ApprovedBy, "the approval is cleared")
	assert.Nil(t, got.Status.ApprovedAt)
	assert.Nil(t, got.Status.ApplyTicket, "no ticket")
	assert.Equal(t, lcBaseline, lcPersonaMemory(t, c), "nothing applied")
	voided := findCondition(got.Status.Conditions, dorguv1.ConditionApprovalVoided)
	require.NotNil(t, voided, "a condition says why")
	assert.Equal(t, metav1.ConditionTrue, voided.Status)
	assert.Equal(t, dorguv1.ReasonAdmissionGuardWasNotReady, voided.Reason)
	assert.Contains(t, voided.Message, "approve it again")
	require.NotNil(t, got.Status.Gate, "the proposal record stays, so it can be approved again")
	select {
	case event := <-events.Events:
		assert.Contains(t, event, "Warning")
		assert.Contains(t, event, EventReasonApprovalVoided)
	default:
		t.Error("an Event says why the approval was sent back")
	}
}

func TestApproved_AnApprovalWrittenOutsideEveryWindowIsTicketed(t *testing.T) {
	at := windowStart.Add(10 * time.Minute)
	ra := approvedWithManagedFields(statusWrite("dorgu-cli", &at, "phase", "approvedBy", "approvedAt"))
	rc, _, _ := guardedController(t, ra, stubTicketGuard{notReady: notReadyBetween(windowStart, windowStart.Add(time.Minute))})

	_, got := lcReconcile(t, rc, ra)

	assert.NotEqual(t, RemediationPhasePending, got.Status.Phase)
	assert.NotNil(t, got.Status.ApplyTicket)
	assert.Nil(t, findCondition(got.Status.Conditions, dorguv1.ConditionApprovalVoided))
}

// The person who writes an approval also writes approvedAt, so it proves nothing about when.
func TestApproved_ApprovedAtIsNeverTrusted(t *testing.T) {
	window := stubTicketGuard{notReady: notReadyBetween(windowStart, windowStart.Add(time.Minute))}

	t.Run("a forged approvedAt outside the window does not hide an approval written inside it", func(t *testing.T) {
		at := windowStart.Add(20 * time.Second)
		ra := approvedWithManagedFields(statusWrite("dorgu-cli", &at, "phase", "approvedBy", "approvedAt"))
		forged := metav1.NewTime(windowStart.Add(-24 * time.Hour))
		ra.Status.ApprovedAt = &forged
		rc, _, _ := guardedController(t, ra, window)

		_, got := lcReconcile(t, rc, ra)

		assert.Equal(t, RemediationPhasePending, got.Status.Phase)
	})

	t.Run("an approvedAt inside the window does not void an approval written outside it", func(t *testing.T) {
		at := windowStart.Add(time.Hour)
		ra := approvedWithManagedFields(statusWrite("dorgu-cli", &at, "phase", "approvedBy", "approvedAt"))
		inside := metav1.NewTime(windowStart.Add(20 * time.Second))
		ra.Status.ApprovedAt = &inside
		rc, _, _ := guardedController(t, ra, window)

		_, got := lcReconcile(t, rc, ra)

		assert.NotEqual(t, RemediationPhasePending, got.Status.Phase)
		assert.NotNil(t, got.Status.ApplyTicket)
	})
}

// If the stamp cannot be read with certainty and the guard was ever not ready, the approval is
// not honoured.
func TestApproved_AnUnreadableApprovalTimeFailsClosed(t *testing.T) {
	outside := windowStart.Add(time.Hour)
	window := stubTicketGuard{notReady: notReadyBetween(windowStart, windowStart.Add(time.Minute))}
	cases := map[string][]metav1.ManagedFieldsEntry{
		"no managedFields at all": nil,
		"only the operator's own entry": {
			statusWrite("operator", &outside, "gate", "conditions", "phase"),
		},
		"the entry has no time": {
			statusWrite("dorgu-cli", nil, "phase", "approvedBy", "approvedAt"),
		},
		"two writers own the approval": {
			statusWrite("dorgu-cli", &outside, "phase", "approvedBy", "approvedAt"),
			statusWrite("someone-else", &outside, "approvedBy"),
		},
		"the entry is not for the status subresource": {
			func() metav1.ManagedFieldsEntry {
				e := statusWrite("dorgu-cli", &outside, "phase", "approvedBy", "approvedAt")
				e.Subresource = ""
				return e
			}(),
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			ra := approvedWithManagedFields(entries...)
			rc, _, _ := guardedController(t, ra, window)

			_, got := lcReconcile(t, rc, ra)

			assert.Equal(t, RemediationPhasePending, got.Status.Phase, "fail closed: approve it again")
			assert.Nil(t, got.Status.ApplyTicket)
		})
	}
}

// With a guard that was never not ready (nothing to compare with), the stamp is not even read.
func TestApproved_NoWindowMeansTheApprovalTimeIsNotNeeded(t *testing.T) {
	ra := approvedWithManagedFields() // no managedFields
	rc, _, _ := guardedController(t, ra, stubTicketGuard{})

	_, got := lcReconcile(t, rc, ra)

	assert.NotNil(t, got.Status.ApplyTicket)
}

// The guard is asked again at the moment a ticket would be issued, not trusted from its last
// periodic run, and a guard that says no means no ticket and no change to the action.
func TestApproved_TheGuardIsAskedAgainRightBeforeTheTicket(t *testing.T) {
	asked := 0
	ra := approvedWithManagedFields()
	rc, _, _ := guardedController(t, ra, stubTicketGuard{asked: &asked})

	lcReconcile(t, rc, ra)

	assert.Equal(t, 1, asked, "once, synchronously, before the ticket")
}

func TestApproved_AGuardThatIsClosedNowIssuesNothingAndChangesNothing(t *testing.T) {
	asked := 0
	at := windowStart.Add(time.Hour)
	ra := approvedWithManagedFields(statusWrite("dorgu-cli", &at, "phase", "approvedBy", "approvedAt"))
	failure := &intake.Failure{Check: "admission-policies", Reason: intake.ReasonAdmissionPoliciesMissing, Message: "deleted"}
	rc, _, _ := guardedController(t, ra, stubTicketGuard{blocked: failure, asked: &asked, notReady: func(time.Time) bool { return true }})

	res, got := lcReconcile(t, rc, ra)

	assert.Equal(t, RemediationPhaseApproved, got.Status.Phase, "it waits; the window decides what to do once it is open again")
	assert.Equal(t, "alice", got.Status.ApprovedBy)
	assert.Nil(t, got.Status.ApplyTicket)
	assert.Positive(t, res.RequeueAfter)
	assert.Equal(t, 1, asked)
}

// ApprovalWrittenAt reads the status write that recorded the approval.
func TestApprovalWrittenAt(t *testing.T) {
	at := windowStart.Add(7 * time.Second)
	later := windowStart.Add(time.Hour)
	ra := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{ManagedFields: []metav1.ManagedFieldsEntry{
		statusWrite("operator", &later, "gate", "phase"),
		statusWrite("dorgu-cli", &at, "approvedBy", "approvedAt"),
	}}}
	got, err := ApprovalWrittenAt(ra)
	require.NoError(t, err)
	assert.True(t, at.Equal(got), "the entry that owns approvedBy, not the operator's later one")
}

// An entry whose fields cannot be read could be the one that owns the approval, so it is an
// error, not something to skip. (The fake client refuses such an entry, so this is direct.)
func TestApprovalWrittenAt_AnUnreadableEntryIsAnError(t *testing.T) {
	at := windowStart
	bad := statusWrite("dorgu-cli", &at, "phase")
	bad.FieldsV1 = &metav1.FieldsV1{Raw: []byte(`{"f:status":5}`)}
	ra := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{ManagedFields: []metav1.ManagedFieldsEntry{bad}}}
	_, err := ApprovalWrittenAt(ra)
	require.Error(t, err)
}
