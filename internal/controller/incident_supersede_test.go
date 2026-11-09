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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Re-review N-2: when an incident escalates, the proposal made for the milder shape
// is advice for a fault that has since become a worse one. A Pending one is
// superseded (Expired, reason Superseded) so the worse shape is proposed in the
// same cycle; a proposal a human already acted on is not touched, and the worse
// shape waits for it.

// advisoryFor adds an outstanding proposal for the incident, as the AI planner
// leaves one for a health incident, in the given phase.
func advisoryFor(t *testing.T, e *recurrenceEnv, incident, phase string) *dorguv1.RemediationAction {
	t.Helper()
	ra := raFor("advisory-"+phase, incident, phase)
	ra.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	ra.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	require.NoError(t, e.c.Create(context.Background(), ra))
	require.NoError(t, e.c.Status().Update(context.Background(), ra))
	return ra
}

func (e *recurrenceEnv) actionNamed(name string) dorguv1.RemediationAction {
	e.t.Helper()
	var ra dorguv1.RemediationAction
	require.NoError(e.t, e.c.Get(context.Background(), client.ObjectKey{Namespace: recurrenceNS, Name: name}, &ra))
	return ra
}

func (e *recurrenceEnv) otherActions(except string) int {
	n := 0
	for _, a := range e.actions() {
		if a.Name != except {
			n++
		}
	}
	return n
}

func TestSupersede_APendingProposalForTheMilderShapeIsExpiredAndTheWorseOneIsProposed(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	rec := record.NewFakeRecorder(50)
	e.r.Recorder = rec
	e.setReadiness()
	e.cycle()
	ra := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhasePending)
	transitionsBefore := metricValue(t, "dorgu_remediation_transitions_total", map[string]string{"from": "Pending", "to": "Expired"})

	e.setOOM()
	e.cycle()

	got := e.actionNamed(ra.Name)
	assert.Equal(t, RemediationPhaseExpired, got.Status.Phase)
	var cond *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == ConditionExpired {
			cond = &got.Status.Conditions[i]
		}
	}
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, ReasonSuperseded, cond.Reason, "a bounded reason of its own, not ApprovalTimedOut")
	assert.Contains(t, cond.Message, "Nothing was applied")

	assert.NotZero(t, e.otherActions(ra.Name), "the worse shape is proposed in the same cycle")
	assert.True(t, hasEvent(drainEvents(rec), "Normal", EventReasonSuperseded), "and an Event says what happened")
	assert.EqualValues(t, 1, metricValue(t, "dorgu_remediation_transitions_total",
		map[string]string{"from": "Pending", "to": "Expired"})-transitionsBefore)
}

// A proposal that a human has approved, or that is being applied or verified, is
// not the operator's to withdraw. The worse shape waits until it is finished.
func TestSupersede_AnActedOnProposalIsNeverSuperseded(t *testing.T) {
	for _, phase := range []string{RemediationPhaseApproved, RemediationPhaseTicketed,
		RemediationPhaseApplying, RemediationPhaseVerifying} {
		t.Run(phase, func(t *testing.T) {
			e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
			e.setReadiness()
			e.cycle()
			ra := advisoryFor(t, e, e.onlyOpen().Name, phase)

			e.setOOM()
			e.cycle()

			assert.Equal(t, phase, e.actionNamed(ra.Name).Status.Phase, "left exactly as it was")
			assert.Zero(t, e.otherActions(ra.Name), "the worse shape defers until it finishes")
			assert.Equal(t, "OOMKilled", e.onlyOpen().Spec.Detection.Signal, "the incident itself still escalates")
		})
	}
}

func TestSupersede_OnlyTheEscalatingIncidentsProposalsAreTouched(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	mine := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhasePending)
	other := raFor("someone-elses", "im-shop-api-imagepullbackoff-0123456789ab", RemediationPhasePending)
	other.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	require.NoError(t, e.c.Create(context.Background(), other))
	require.NoError(t, e.c.Status().Update(context.Background(), other))

	e.setOOM()
	e.cycle()

	assert.Equal(t, RemediationPhaseExpired, e.actionNamed(mine.Name).Status.Phase)
	assert.Equal(t, RemediationPhasePending, e.actionNamed(other.Name).Status.Phase)
}

func TestSupersede_AMildlyChangedShapeThatDoesNotEscalateSupersedesNothing(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.container = "main"
	e.setOOM()
	e.cycle()
	ra := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhasePending)

	e.setReadiness() // de-escalation
	e.cycle()

	assert.Equal(t, RemediationPhasePending, e.actionNamed(ra.Name).Status.Phase)
}

// A superseded proposal can no longer be approved. The CLI refuses to approve
// anything that is not Pending; and if an approval is forced onto the status
// anyway, the operator puts it back to Expired and issues no ticket.
func TestSupersede_ASupersededProposalCannotBeApproved(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	ra := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhasePending)
	e.setOOM()
	e.cycle()
	require.Equal(t, RemediationPhaseExpired, e.actionNamed(ra.Name).Status.Phase)

	forged := e.actionNamed(ra.Name)
	forged.Status.Phase = RemediationPhaseApproved
	require.NoError(t, e.c.Status().Update(context.Background(), &forged))

	rec := record.NewFakeRecorder(10)
	rc := &RemediationController{Client: e.c, Logger: noiseLogger(), Recorder: rec}
	_, err := rc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
	require.NoError(t, err)

	after := e.actionNamed(ra.Name)
	assert.Equal(t, RemediationPhaseExpired, after.Status.Phase, "approval of a superseded proposal does not stick")
	assert.Nil(t, after.Status.ApplyTicket, "and no ticket is issued")
	assert.True(t, hasEvent(drainEvents(rec), "Warning", EventReasonSuperseded))
}

// An ordinary expired proposal is untouched by that rule.
func TestSupersede_AnOrdinaryExpiryIsNotReasserted(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	ra := raFor("timed-out", "im-x", RemediationPhaseExpired)
	ra.Status.Conditions = []metav1.Condition{{
		Type: ConditionExpired, Status: metav1.ConditionTrue, Reason: ReasonApprovalTimedOut, LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, e.c.Create(context.Background(), ra))
	require.NoError(t, e.c.Status().Update(context.Background(), ra))
	rc := &RemediationController{Client: e.c, Logger: noiseLogger()}
	_, err := rc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
	require.NoError(t, err)
	assert.Equal(t, RemediationPhaseExpired, e.actionNamed(ra.Name).Status.Phase)
}
