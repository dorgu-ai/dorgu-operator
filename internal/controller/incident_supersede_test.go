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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
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

// A human who approves between the list and the write wins: a proposal that is no
// longer Pending is left alone.
func TestSupersede_ALateApprovalWins(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	approved := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhaseApproved)

	require.NoError(t, e.r.supersede(context.Background(), client.ObjectKeyFromObject(approved), "ReadinessNotReady", "OOMKilled"))

	assert.Equal(t, RemediationPhaseApproved, e.actionNamed(approved.Name).Status.Phase)
}

func TestSupersede_AFailureToListIsReportedAndDoesNotPanic(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(noiseScheme(t)).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("the API server is unavailable")
		},
	})
	r := newNoiseReconciler(c)
	require.Error(t, r.supersedePending(context.Background(), &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: "im-x", Namespace: recurrenceNS},
	}, "ReadinessNotReady"))
}

// ---- Re-review 2: which proposals an escalation supersedes -------------------
//
// R2-1: the gate defers by lineage, so a Pending proposal of an EARLIER generation of
// the same fault is as much in the way as one of the incident itself, and is
// superseded too.
//
// R2-2: only a proposal for a strictly milder shape is advice the escalation has
// overtaken. One that answers the shape the incident now has, or a worse one, still
// fits, and superseding it on a flap (OOM, then Readiness, then OOM again) expired the
// standing proposal and minted another each time until the rate limit ran out, leaving
// a critical incident with nothing for a human to answer.
//
// A proposal that does not say what it answered (made before proposals recorded it)
// is judged by the incident's escalated-at: it is superseded only if the incident
// escalated AFTER the proposal was created, which is when it can only have answered an
// earlier, milder shape. With no escalated-at, or an unreadable one, or one that is
// not after the proposal, it is kept: the safe direction is to leave a human's queue
// alone and let the gate defer.

func answering(t *testing.T, e *recurrenceEnv, name, incident, signal string, created time.Time) *dorguv1.RemediationAction {
	t.Helper()
	ra := raFor(name, incident, RemediationPhasePending)
	ra.CreationTimestamp = metav1.NewTime(created)
	ra.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	if signal != "" {
		ra.Annotations = map[string]string{gate.AnnotationAnswersSignal: signal}
	}
	require.NoError(t, e.c.Create(context.Background(), ra))
	require.NoError(t, e.c.Status().Update(context.Background(), ra))
	return ra
}

func TestSupersede_OnlyAStrictlyMilderProposalIsSuperseded(t *testing.T) {
	tests := []struct {
		answers    string
		superseded bool
	}{
		{"ReadinessNotReady", true},
		{"RestartLoop", true},
		{"LivenessRestarts", true},
		{"CrashLoopBackOff", true},
		{"OOMKilled", false},       // the shape the incident is escalating to: it still fits
		{"ImagePullBackOff", true}, // not in the progression: rank unknown, so it is not provably as severe
	}
	for _, tt := range tests {
		t.Run(tt.answers, func(t *testing.T) {
			e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
			e.setCrashLoop()
			e.cycle()
			ra := answering(t, e, "p", e.onlyOpen().Name, tt.answers, time.Now().Add(-time.Minute))

			e.setOOM()
			e.cycle()

			got := e.actionNamed(ra.Name).Status.Phase
			if tt.superseded {
				assert.Equal(t, RemediationPhaseExpired, got)
			} else {
				assert.Equal(t, RemediationPhasePending, got, "answers %s, as severe as the incident now is", tt.answers)
			}
		})
	}
}

// A proposal for a worse shape than the one the incident escalates to also stays.
func TestSupersede_AProposalForAWorseShapeIsKept(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	ra := answering(t, e, "worse", e.onlyOpen().Name, "OOMKilled", time.Now().Add(-time.Minute))

	e.setCrashLoop() // escalates, but not as far as the OOM the proposal answers
	e.cycle()

	assert.Equal(t, RemediationPhasePending, e.actionNamed(ra.Name).Status.Phase)
}

// Unannotated: the escalation time decides.
func TestSupersede_AnUnannotatedProposalIsSupersededOnlyIfTheEscalationCameAfterIt(t *testing.T) {
	t.Run("proposal older than the escalation: superseded", func(t *testing.T) {
		e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
		e.setReadiness()
		e.cycle()
		ra := answering(t, e, "old", e.onlyOpen().Name, "", time.Now().Add(-time.Hour))
		e.setOOM()
		e.cycle()
		assert.Equal(t, RemediationPhaseExpired, e.actionNamed(ra.Name).Status.Phase)
	})
	t.Run("proposal made after the escalation: kept", func(t *testing.T) {
		// The incident already escalated to CrashLoopBackOff; the proposal is created
		// after that, so it answers this shape or a later one, and the next escalation
		// finds the stamp older than the proposal... which it is not: the next
		// escalation stamps a NEW time. Model it directly: stamp an escalation in the
		// past and ask supersedePending with the incident as it stands.
		e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
		e.setCrashLoop()
		e.cycle()
		incident := e.onlyOpen()
		incident.Annotations = map[string]string{gate.AnnotationEscalatedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
		ra := answering(t, e, "newer", incident.Name, "", time.Now().Add(-time.Minute))

		require.NoError(t, e.r.supersedePending(context.Background(), &incident, "ReadinessNotReady"))

		assert.Equal(t, RemediationPhasePending, e.actionNamed(ra.Name).Status.Phase,
			"escalated an hour ago, proposed a minute ago: it cannot have answered the earlier shape")
	})
	t.Run("no escalated-at on the incident: kept", func(t *testing.T) {
		e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
		e.setCrashLoop()
		e.cycle()
		incident := e.onlyOpen()
		incident.Annotations = nil
		ra := answering(t, e, "unknown", incident.Name, "", time.Now().Add(-time.Hour))
		require.NoError(t, e.r.supersedePending(context.Background(), &incident, "ReadinessNotReady"))
		assert.Equal(t, RemediationPhasePending, e.actionNamed(ra.Name).Status.Phase, "nothing says it is milder")
	})
	t.Run("unreadable escalated-at: kept", func(t *testing.T) {
		e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
		e.setCrashLoop()
		e.cycle()
		incident := e.onlyOpen()
		incident.Annotations = map[string]string{gate.AnnotationEscalatedAt: "yesterday"}
		ra := answering(t, e, "garbled", incident.Name, "", time.Now().Add(-time.Hour))
		require.NoError(t, e.r.supersedePending(context.Background(), &incident, "ReadinessNotReady"))
		assert.Equal(t, RemediationPhasePending, e.actionNamed(ra.Name).Status.Phase)
	})
	t.Run("escalated in the same second: kept", func(t *testing.T) {
		e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
		e.setCrashLoop()
		e.cycle()
		incident := e.onlyOpen()
		at := time.Now().Truncate(time.Second)
		incident.Annotations = map[string]string{gate.AnnotationEscalatedAt: at.UTC().Format(time.RFC3339)}
		ra := answering(t, e, "same-second", incident.Name, "", at)
		require.NoError(t, e.r.supersedePending(context.Background(), &incident, "ReadinessNotReady"))
		assert.Equal(t, RemediationPhasePending, e.actionNamed(ra.Name).Status.Phase, "not provably older")
	})
}

// A flapping fault ends with a standing proposal for the worst shape, and a bounded
// number of RemediationActions: 12 OOM, Readiness alternations at the default rate limit.
func TestSupersede_AFlappingFaultKeepsAStandingProposalForTheWorstShape(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	rec := record.NewFakeRecorder(1000)
	e.r.Recorder = rec
	e.container = mainContainer

	for range 12 {
		e.setOOM()
		e.cycle()
		stampCreationNow(t, e)
		e.setReadiness()
		e.cycle()
		stampCreationNow(t, e)
	}
	e.setOOM()
	e.cycle()

	var pending, expired int
	for _, a := range e.actions() {
		switch a.Status.Phase {
		case RemediationPhasePending:
			pending++
			assert.Equal(t, "OOMKilled", a.Annotations[gate.AnnotationAnswersSignal], "the one standing is for the worst shape")
		case RemediationPhaseExpired:
			expired++
		}
	}
	assert.Equal(t, 1, pending, "a critical incident ends with one proposal for a human to answer")
	assert.LessOrEqual(t, len(e.actions()), 5, "bounded by the persona rate limit, not by the number of flaps")
	assert.Zero(t, expired, "nothing the worst shape's own proposal answered was expired")
	assert.Len(t, e.open(), 1)
	for _, ev := range drainEvents(rec) {
		assert.False(t, hasEvent([]string{ev}, "Normal", EventReasonSuperseded), "no churn: %s", ev)
	}
}

// R2-1 in its other direction: a Pending proposal of an earlier generation (whose incident
// resolved) is expired as stale, milder or not, before escalation is even judged. Since B10 (a)
// the sweep at the start of the cycle settles it, so no earlier generation's Pending proposal
// can reach the supersede rule, and none defers the new generation's proposals.
func TestSupersede_AnEarlierGenerationsPendingProposalsExpireWithTheirIncident(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	gen0 := e.onlyOpen()
	milder := answering(t, e, "gen0-milder", gen0.Name, "ReadinessNotReady", time.Now().Add(-time.Hour))
	sameAsNow := answering(t, e, "gen0-oom", gen0.Name, "OOMKilled", time.Now().Add(-time.Hour))
	e.resolve(gen0.Name)

	e.setReadiness()
	e.cycle() // generation 1
	require.NotEqual(t, gen0.Name, e.onlyOpen().Name)
	e.setOOM()
	e.cycle()

	assert.Equal(t, RemediationPhaseExpired, e.actionNamed(milder.Name).Status.Phase)
	assert.Equal(t, ReasonIncidentResolved, expiredReason(e.actionNamed(milder.Name)))
	assert.Equal(t, RemediationPhaseExpired, e.actionNamed(sameAsNow.Name).Status.Phase)
	assert.Equal(t, ReasonIncidentResolved, expiredReason(e.actionNamed(sameAsNow.Name)))
}
