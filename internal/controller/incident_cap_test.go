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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// B9 review F-3: a fault that flaps is bounded. After RecurrenceCap recurrences
// inside RecurrenceWindow, the next one reopens the latest incident (count,
// lastSeen, and open again, so it is never a silent fold into history) and one
// Warning Event says so.

func cappedCount(t *testing.T) float64 {
	t.Helper()
	return metricValue(t, "dorgu_incident_recurrences_capped_total", map[string]string{"class": "resource", "scope": "persona"})
}

func cappedEnv(t *testing.T, cap int) (*recurrenceEnv, *record.FakeRecorder) {
	t.Helper()
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	rec := record.NewFakeRecorder(100)
	e.r.Recorder = rec
	e.r.RecurrenceCap = cap
	e.r.RecurrenceWindow = 24 * time.Hour
	return e, rec
}

func TestCap_AFlappingFaultStopsOpeningIncidents(t *testing.T) {
	const flaps = 40
	e, rec := cappedEnv(t, 3)
	before := cappedCount(t)

	e.cycle()
	for range flaps {
		e.resolve(e.onlyOpen().Name)
		e.cycle()
	}

	all := e.incidents()
	assert.Len(t, all, 1+3, "the first incident and three recurrences, however long it flaps (was %d)", flaps+1)
	latest := e.onlyOpen()
	assert.Equal(t, 3, gateGeneration(latest.Name), "the latest generation is the last one the cap allowed")
	assert.Equal(t, PhaseDetected, latest.Status.Phase, "reopened, not silently left Resolved")
	assert.Equal(t, PhaseDetected, latest.Labels[LabelPhase])
	assert.Nil(t, latest.Spec.Resolution, "an open incident carries no resolution")
	assert.EqualValues(t, flaps-3, cappedCountDelta(t, before), "every capped recurrence is counted")
	assert.EqualValues(t, flaps-3, mustAtoi(t, latest.Annotations[AnnotationRecurrencesFolded]))
	assert.Greater(t, latest.Status.OccurrenceCount, int32(1), "the folded sightings are counted")

	var flapping []string
	for _, ev := range drainEvents(rec) {
		if strings.Contains(ev, EventReasonIncidentFlapping) {
			flapping = append(flapping, ev)
		}
	}
	require.Len(t, flapping, 1, "one Event when the cap is hit, not one per flap: %v", flapping)
	assert.Contains(t, flapping[0], "Warning")
}

func TestCap_BelowTheCapEveryRecurrenceIsItsOwnIncident(t *testing.T) {
	e, _ := cappedEnv(t, 3)
	e.cycle()
	for range 3 {
		e.resolve(e.onlyOpen().Name)
		e.cycle()
	}
	assert.Len(t, e.incidents(), 4)
	assert.Empty(t, e.onlyOpen().Annotations[AnnotationRecurrencesFolded])
}

func TestCap_RecurrencesOutsideTheWindowDoNotCount(t *testing.T) {
	e, _ := cappedEnv(t, 3)
	e.cycle()
	for range 3 {
		e.resolve(e.onlyOpen().Name)
		e.cycle()
	}
	// A day and an hour on, the three recurrences are old news.
	old := metav1.NewTime(time.Now().Add(-25 * time.Hour))
	for _, im := range e.incidents() {
		fresh := e.get(im.Name)
		fresh.Spec.Detection.FirstSeen = old
		require.NoError(t, e.c.Update(context.Background(), &fresh))
	}
	e.resolve(e.onlyOpen().Name)
	e.cycle()

	assert.Len(t, e.incidents(), 5, "outside the window a recurrence is a new incident again")
	assert.Equal(t, 4, gateGeneration(e.onlyOpen().Name))
}

func TestCap_ZeroDisablesIt(t *testing.T) {
	e, _ := cappedEnv(t, 0)
	e.cycle()
	for range 8 {
		e.resolve(e.onlyOpen().Name)
		e.cycle()
	}
	assert.Len(t, e.incidents(), 9)
}

func TestCap_TheReopenedIncidentStillGetsItsProposalThroughTheGate(t *testing.T) {
	e, _ := cappedEnv(t, 1)
	e.cycle() // r0 + its proposal
	first := e.onlyOpen()
	e.resolve(first.Name)
	e.cycle() // r1 (cap not yet reached: zero recurrences so far); r0's proposal is outstanding, so it defers
	r1 := e.onlyOpen()
	require.Equal(t, 1, gateGeneration(r1.Name))
	require.Len(t, e.actions(), 1)

	// Finish every proposal, so only the cap decides what happens next.
	for _, ra := range e.actions() {
		ra.Status.Phase = RemediationPhaseCompleted
		require.NoError(t, e.c.Status().Update(context.Background(), &ra))
	}
	e.resolve(r1.Name)
	e.cycle() // capped: r1 reopens

	reopened := e.onlyOpen()
	assert.Equal(t, r1.Name, reopened.Name)
	var named int
	for _, ra := range e.actions() {
		if ra.Spec.IncidentRef.Name == reopened.Name {
			named++
		}
	}
	assert.GreaterOrEqual(t, named, 1, "the reopened incident is proposed for like any open one")
	assert.Len(t, e.actions(), 2, "a finished fix that did not hold gets a new proposal, capped or not")
}

func TestCap_FlappingConditionOnTheReopenedIncident(t *testing.T) {
	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: "im-x-r3", Annotations: map[string]string{
			AnnotationRecurrenceOf: "im-x-r2", AnnotationRecurrenceCount: "3", AnnotationRecurrencesFolded: "5",
		}},
		Status: dorguv1.IncidentMemoryStatus{Phase: PhaseDetected, OccurrenceCount: 6},
	}
	(&IncidentController{}).updateConditions(im)

	var cond *metav1.Condition
	for i := range im.Status.Conditions {
		if im.Status.Conditions[i].Type == ConditionFlapping {
			cond = &im.Status.Conditions[i]
		}
	}
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "RecurrenceCapReached", cond.Reason)
	assert.Contains(t, cond.Message, "5")

	plain := &dorguv1.IncidentMemory{Status: dorguv1.IncidentMemoryStatus{Phase: PhaseDetected, OccurrenceCount: 1}}
	(&IncidentController{}).updateConditions(plain)
	for _, c := range plain.Status.Conditions {
		assert.NotEqual(t, ConditionFlapping, c.Type, "an ordinary incident does not carry the condition")
	}
}

func TestCap_TheWindowDefaultsWhenOnlyTheCapIsSet(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.r.RecurrenceCap = 1 // RecurrenceWindow left zero
	e.cycle()
	for range 3 {
		e.resolve(e.onlyOpen().Name)
		e.cycle()
	}
	assert.Len(t, e.incidents(), 2, "a cap with no window uses the default 24h window")
}

var _ client.Object = (*dorguv1.IncidentMemory)(nil)

// Re-review N-4: the cap counts per fault, and a fault is the application, the
// container and the family, not one signal. A flapper that changes shape each time
// it comes back has one budget.
func TestCap_ShapesOfOneFaultShareOneBudgetAndOneLineage(t *testing.T) {
	e, _ := cappedEnv(t, 3)
	e.container = mainContainer
	shapes := []func(){e.setReadiness, e.setLiveness, e.setCrashLoop, e.setOOM}

	shapes[0]()
	e.cycle()
	first := e.onlyOpen()
	for i := 1; i <= 30; i++ {
		e.resolve(e.onlyOpen().Name)
		shapes[i%len(shapes)]()
		e.cycle()
	}

	all := e.incidents()
	assert.LessOrEqual(t, len(all), 1+3, "one fault, one cap: %v", incidentSignals(all))
	for _, im := range all {
		assert.True(t, gateSameLineage(first.Name, im.Name), "%s left the lineage of %s", im.Name, first.Name)
	}
}

// The same signal on another container is the same incident, as it always was (one
// signal's sightings are one diagnosis), but the incident names both containers.
func TestFamily_TheSameSignalOnAnotherContainerIsNamedOnTheIncident(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.container = "app"
	e.setCrashLoop()
	e.cycle()
	e.container = "sidecar"
	e.setCrashLoop()
	e.cycle()

	im := e.onlyOpen()
	assert.Equal(t, "app,sidecar", im.Annotations[AnnotationContainers], "nothing hides: the incident names both")
	assert.Len(t, e.incidents(), 1)
}

// At the cap the reopened incident takes the shape the fault has now, and keeps
// the record of the resolution it replaces.
func TestCap_TheReopenedIncidentFollowsTheCurrentShapeAndKeepsItsHistory(t *testing.T) {
	e, _ := cappedEnv(t, 1)
	e.container = mainContainer
	e.setCrashLoop()
	e.cycle()
	e.resolve(e.onlyOpen().Name)
	e.setCrashLoop()
	e.cycle() // the one recurrence the cap allows
	latest := e.onlyOpen()
	e.resolve(latest.Name)

	e.setReadiness() // capped, and a milder shape
	e.cycle()

	im := e.onlyOpen()
	assert.Equal(t, latest.Name, im.Name)
	assert.Equal(t, "ReadinessNotReady", im.Spec.Detection.Signal)
	assert.Equal(t, "ReadinessNotReady", im.Labels[LabelSignal])
	assert.Contains(t, im.Annotations[AnnotationEscalatedFrom], "CrashLoopBackOff")
	assert.Contains(t, im.Annotations[AnnotationLastResolution], "resolved", "the fix that did not hold is not lost with the resolution field")
}
