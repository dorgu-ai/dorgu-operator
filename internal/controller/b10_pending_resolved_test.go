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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// B10 (a), D1.1 finding 1: a Pending proposal for an incident that has since
// Resolved is stale advice, and it silenced the whole persona (25 deferrals in
// D1.1). It now expires with the reason IncidentResolved.

func expiredReason(ra dorguv1.RemediationAction) string {
	for _, c := range ra.Status.Conditions {
		if c.Type == ConditionExpired && c.Status == metav1.ConditionTrue {
			return c.Reason
		}
	}
	return ""
}

func TestPendingResolved_APendingProposalForAResolvedIncidentExpiresAsIncidentResolved(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	rec := record.NewFakeRecorder(50)
	e.r.Recorder = rec
	e.cycle()
	first := e.onlyOpen()
	ras := e.actions()
	require.Len(t, ras, 1)
	require.Equal(t, RemediationPhasePending, ras[0].Status.Phase)

	// The application recovered by itself: the incident resolves and nothing is reported.
	e.resolve(first.Name)
	e.count.signals = nil
	transitions := metricValue(t, "dorgu_remediation_transitions_total", map[string]string{"from": "Pending", "to": "Expired"})
	refusals := metricValue(t, "dorgu_gate_decisions_total",
		map[string]string{"phase": "Proposal", "stage": "DedupCooldown", "result": "refuse", "reason": "active-remediation"})

	e.cycle()

	got := e.actionNamed(ras[0].Name)
	assert.Equal(t, RemediationPhaseExpired, got.Status.Phase)
	assert.Equal(t, ReasonIncidentResolved, expiredReason(got), "a bounded reason of its own")
	cond := lcCondition(&got, ConditionExpired)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "Nothing was applied")
	assert.Contains(t, cond.Message, "not a rejection")
	assert.Nil(t, got.Status.ApplyTicket)
	assert.True(t, hasEvent(drainEvents(rec), "Normal", EventReasonExpired), "an Event says what happened")
	assert.EqualValues(t, 1, metricValue(t, "dorgu_remediation_transitions_total",
		map[string]string{"from": "Pending", "to": "Expired"})-transitions)
	assert.Zero(t, metricValue(t, "dorgu_gate_decisions_total",
		map[string]string{"phase": "Proposal", "stage": "DedupCooldown", "result": "refuse", "reason": "active-remediation"})-refusals,
		"not a refusal")
}

// The point of the change: the same fault coming back is proposed for in the normal way, in
// the very cycle that notices the recurrence, with no cooldown held against the application.
func TestPendingResolved_AFaultThatRecursIsProposedForAgain(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.cycle()
	first := e.onlyOpen()
	stale := e.actions()[0]
	e.resolve(first.Name)

	// The fault is still being reported: it is a recurrence of the resolved incident.
	e.cycle()

	recurrence := e.onlyOpen()
	require.NotEqual(t, first.Name, recurrence.Name, "the recurrence is a new incident")
	assert.Equal(t, RemediationPhaseExpired, e.actionNamed(stale.Name).Status.Phase)
	var fresh []dorguv1.RemediationAction
	for _, a := range e.actions() {
		if a.Spec.IncidentRef.Name == recurrence.Name {
			fresh = append(fresh, a)
		}
	}
	require.Len(t, fresh, 1, "the normal proposal path ran for the recurrence")
	assert.Equal(t, RemediationPhasePending, fresh[0].Status.Phase)
	assert.Nil(t, findCondition(recurrence.Status.Conditions, dorguv1.ConditionProposalRefused),
		"the stale proposal's expiry is not recorded as a refusal")
}

func TestPendingResolved_OnlyPendingIsTouched(t *testing.T) {
	for _, phase := range []string{RemediationPhaseApproved, RemediationPhaseTicketed,
		RemediationPhaseApplying, RemediationPhaseVerifying} {
		t.Run(phase, func(t *testing.T) {
			e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
			e.cycle()
			first := e.onlyOpen()
			ra := e.actions()[0]
			ra.Status.Phase = phase
			require.NoError(t, e.c.Status().Update(context.Background(), &ra))
			e.resolve(first.Name)
			e.count.signals = nil

			e.cycle()

			got := e.actionNamed(ra.Name)
			assert.Equal(t, phase, got.Status.Phase, "a decision a human or the operator acted on is left exactly as it was")
			assert.Empty(t, expiredReason(got))
		})
	}
}

func TestPendingResolved_AnOpenIncidentsProposalIsKept(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.cycle()
	ra := e.actions()[0]

	e.cycle()

	assert.Equal(t, RemediationPhasePending, e.actionNamed(ra.Name).Status.Phase)
}

// One incident's resolution does not expire the proposal of a different incident on the same
// persona, only its own.
func TestPendingResolved_OtherIncidentsProposalIsKept(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.cycle()
	first := e.onlyOpen()
	other := raFor("someone-elses", "im-shop-api-imagepullbackoff-0123456789ab", RemediationPhasePending)
	other.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	require.NoError(t, e.c.Create(context.Background(), other))
	require.NoError(t, e.c.Status().Update(context.Background(), other))
	e.resolve(first.Name)
	e.count.signals = nil

	e.cycle()

	assert.Equal(t, RemediationPhasePending, e.actionNamed(other.Name).Status.Phase)
}

// A proposal the 0.13.0 operator left behind for an incident that resolved long ago is swept
// on the first cycle after the upgrade, which is the D1.1 situation.
func TestPendingResolved_AProposalLeftBehindIsSwept(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.cycle()
	first := e.onlyOpen()
	stale := e.actions()[0]
	e.resolve(first.Name)
	e.count.signals = nil
	e.cycle() // expired now; put it back to Pending the way an old operator left it
	got := e.actionNamed(stale.Name)
	got.Status.Phase = RemediationPhasePending
	got.Status.Conditions = nil
	require.NoError(t, e.c.Status().Update(context.Background(), &got))

	e.cycle()

	assert.Equal(t, ReasonIncidentResolved, expiredReason(e.actionNamed(stale.Name)))
}

// A human who approves between the list and the write wins.
func TestPendingResolved_AnApprovalThatLandsFirstWins(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.cycle()
	first := e.onlyOpen()
	ra := e.actions()[0]
	e.resolve(first.Name)
	e.count.signals = nil

	var approved dorguv1.RemediationAction
	require.NoError(t, e.c.Get(context.Background(), client.ObjectKeyFromObject(&ra), &approved))
	approved.Status.Phase = RemediationPhaseApproved
	require.NoError(t, e.c.Status().Update(context.Background(), &approved))

	e.cycle()

	assert.Equal(t, RemediationPhaseApproved, e.actionNamed(ra.Name).Status.Phase)
}
