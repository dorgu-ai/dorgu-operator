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

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B9 review F-1: one fault is one incident across cycles.
//
// Identity: an incident is a persona and a signal, so a fault that worsens (not
// Ready, then restarted by the kubelet, then CrashLoopBackOff, then OOM) used to
// open an incident per shape, each with its own diagnosis stream and proposal.
// Chosen: merge on escalation, within one ranked family of signals. Per-container
// identity was the alternative; it was rejected because an incident has never been
// per container (it is per application and signal, and lists the pods affected),
// so a per-container key would change what every other signal means, and a pod
// replaced by a rollback would mint a new identity for a fault that has not moved.
// The merge keeps the incident's name, firstSeen and count, and records the path.

func (e *recurrenceEnv) setSignal(typ detection.SignalType, category detection.Category, severity detection.Severity) {
	e.count.signals = []detection.Signal{e.signalFor(typ, category, severity, e.pod, e.container)}
}

func (e *recurrenceEnv) setReadiness() {
	e.setSignal(detection.SignalReadinessNotReady, detection.CategoryHealth, detection.SeverityWarning)
}
func (e *recurrenceEnv) setLoop() {
	e.setSignal(detection.SignalRestartLoop, detection.CategoryHealth, detection.SeverityWarning)
}
func (e *recurrenceEnv) setLiveness() {
	e.setSignal(detection.SignalLivenessRestarts, detection.CategoryHealth, detection.SeverityWarning)
}
func (e *recurrenceEnv) setCrashLoop() {
	e.setSignal(detection.SignalCrashLoopBackOff, detection.CategoryHealth, detection.SeverityCritical)
}
func (e *recurrenceEnv) setOOM() {
	e.setSignal(detection.SignalOOMKilled, detection.CategoryResource, detection.SeverityCritical)
}

func TestFamily_AFaultThatWorsensIsOneIncident(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	opened := recurrenceCount(t) // unrelated counter, read to prove nothing else moves

	e.setReadiness()
	e.cycle()
	first := e.onlyOpen()
	require.Equal(t, "ReadinessNotReady", first.Spec.Detection.Signal)

	e.setLoop()
	e.cycle()
	e.setLiveness()
	e.cycle()
	e.setCrashLoop()
	e.cycle()
	e.setOOM()
	e.cycle()

	im := e.onlyOpen()
	require.Len(t, e.incidents(), 1, "one fault, one incident: got %s", incidentSignals(e.incidents()))
	assert.Equal(t, first.Name, im.Name, "the incident keeps its identity")
	assert.Equal(t, "OOMKilled", im.Spec.Detection.Signal, "escalated to the signal that explains it")
	assert.Equal(t, "resource", im.Spec.Category)
	assert.Equal(t, "resource", im.Labels[LabelCategory], "the labels the incident is found by follow the spec")
	assert.Equal(t, "OOMKilled", im.Labels[LabelSignal])
	assert.Equal(t, string(detection.SeverityCritical), im.Spec.Severity)
	assert.True(t, im.Spec.Detection.FirstSeen.Equal(&first.Spec.Detection.FirstSeen), "history kept: first seen is the first sighting")
	assert.EqualValues(t, 5, im.Status.OccurrenceCount, "every cycle's sighting counts")
	assert.Equal(t, "ReadinessNotReady,RestartLoop,LivenessRestarts,CrashLoopBackOff", im.Annotations[AnnotationEscalatedFrom],
		"the path it took is recorded")
	assert.Equal(t, opened, recurrenceCount(t), "an escalation is not a recurrence")
}

// When the dominant signal clears and a milder one in the same container persists,
// the incident follows the current state down (re-review N-1). It stays one incident
// and its history is kept.
func TestFamily_DeEscalatesWhenTheDominantSignalClears(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.container = "main"
	e.setOOM()
	e.cycle()
	first := e.onlyOpen()
	require.Equal(t, "OOMKilled", first.Spec.Detection.Signal)

	e.setReadiness() // the OOM signal is gone; the container is just not Ready
	e.cycle()

	im := e.onlyOpen()
	require.Len(t, e.incidents(), 1, "got %s", incidentSignals(e.incidents()))
	assert.Equal(t, first.Name, im.Name)
	assert.Equal(t, "ReadinessNotReady", im.Spec.Detection.Signal, "the signal follows the current state")
	assert.Equal(t, "health", im.Spec.Category)
	assert.Equal(t, string(detection.SeverityWarning), im.Spec.Severity, "and so does the severity")
	assert.Equal(t, "ReadinessNotReady", im.Labels[LabelSignal])
	assert.Equal(t, "OOMKilled", im.Annotations[AnnotationEscalatedFrom], "history is kept")
	assert.True(t, im.Spec.Detection.FirstSeen.Equal(&first.Spec.Detection.FirstSeen))
	assert.EqualValues(t, 2, im.Status.OccurrenceCount)

	e.setOOM() // and it comes back
	e.cycle()
	back := e.onlyOpen()
	assert.Equal(t, "OOMKilled", back.Spec.Detection.Signal)
	assert.Equal(t, "OOMKilled,ReadinessNotReady", back.Annotations[AnnotationEscalatedFrom])
	assert.Len(t, e.incidents(), 1)
}

// While the worse signal is still being seen for the container, a milder one is a
// rung of the same fault and is folded in: the incident does not flip.
func TestFamily_AMilderSignalIsFoldedWhileTheWorseOneIsLive(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.count.signals = []detection.Signal{
		e.signalFor(detection.SignalReadinessNotReady, detection.CategoryHealth, detection.SeverityWarning, e.pod, "main"),
		e.signalFor(detection.SignalOOMKilled, detection.CategoryResource, detection.SeverityCritical, e.pod, "main"),
	}
	for range 3 {
		e.cycle()
	}
	im := e.onlyOpen()
	assert.Len(t, e.incidents(), 1, "got %s", incidentSignals(e.incidents()))
	assert.Equal(t, "OOMKilled", im.Spec.Detection.Signal)
	assert.Equal(t, string(detection.SeverityCritical), im.Spec.Severity)
}

func TestFamily_TheTrailOfAFlickeringFaultIsBounded(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.container = "main"
	for range 40 {
		e.setOOM()
		e.cycle()
		e.setReadiness()
		e.cycle()
	}
	im := e.onlyOpen()
	trail := strings.Split(im.Annotations[AnnotationEscalatedFrom], ",")
	assert.LessOrEqual(t, len(trail), maxEscalationTrail, "the annotation must stay small however long it flickers")
	assert.Len(t, e.incidents(), 1)
}

// While any signal of the family is being seen, the incident is still active: a
// lower rung folded into it must keep it from resolving.
func TestFamily_AFoldedSightingKeepsTheIncidentActive(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setCrashLoop()
	e.cycle()
	im := e.onlyOpen()
	// The last sighting of its own signal is long ago; the grace period has passed.
	im.Spec.Detection.LastSeen = metav1.NewTime(time.Now().Add(-time.Hour))
	require.NoError(t, e.c.Update(context.Background(), &im))

	e.setReadiness()
	e.cycle()

	assert.Equal(t, PhaseDetected, e.onlyOpen().Status.Phase)
	assert.WithinDuration(t, time.Now(), e.onlyOpen().Spec.Detection.LastSeen.Time, time.Minute,
		"the fold refreshes lastSeen, which is what the resolution grace period is measured from")
}

func TestFamily_OtherSignalsAndOtherPersonasAreNotMerged(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()

	// ImagePullBackOff is a different fault, not a rung of this one.
	e.setSignal(detection.SignalImagePullBackOff, detection.CategoryHealth, detection.SeverityWarning)
	e.cycle()
	assert.Len(t, e.incidents(), 2, "got %s", incidentSignals(e.incidents()))
}

func TestFamily_AnEscalationAfterResolveRecursInTheSameLineage(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	first := e.onlyOpen()
	e.setCrashLoop()
	e.cycle()
	escalated := e.onlyOpen()
	require.Equal(t, first.Name, escalated.Name)
	e.resolve(escalated.Name)

	// It comes back in its final shape. The recurrence belongs to the lineage of
	// the incident it recurs, whatever the incident's current signal, so a human's
	// rejection of the earlier fix still holds.
	e.cycle()
	recurrence := e.onlyOpen()
	assert.Equal(t, escalated.Name, recurrence.Annotations[AnnotationRecurrenceOf])
	assert.True(t, gate.SameIncidentLineage(first.Name, recurrence.Name),
		"%s must share a lineage with %s", recurrence.Name, first.Name)
	assert.Equal(t, 1, gate.IncidentGeneration(recurrence.Name))
}

func TestFamily_PreviousOccurrencesIncludesTheEscalatedIncident(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	e.cycle()

	diag := aiDiagnosis()
	diag.PersonaRef = &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	diag.Category = "health"
	diag.Contributing[0].Signal.Type = detection.SignalCrashLoopBackOff
	diag.AffectedResources = []dorguv1.ResourceReference{e.pod}
	got := e.r.previousOccurrences(personaSubject(*diag.PersonaRef))(context.Background(), *diag)
	assert.EqualValues(t, 2, got, "the prompt must not call an escalating fault a first sighting")
}

// An application's pods are not one fault. Two pods failing in different ways are
// two incidents; the merge is for ONE pod's fault as it worsens.
func TestFamily_DifferentPodsAtTheSameTimeAreDifferentFaults(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	other := dorguv1.ResourceReference{Kind: "Pod", Name: "api-other", Namespace: recurrenceNS}
	e.count.signals = []detection.Signal{
		e.signalFor(detection.SignalCrashLoopBackOff, detection.CategoryHealth, detection.SeverityCritical, e.pod, "main"),
		e.signalFor(detection.SignalReadinessNotReady, detection.CategoryHealth, detection.SeverityWarning, other, "main"),
	}
	for range 3 {
		e.cycle()
	}
	assert.Len(t, e.open(), 2, "got %s", incidentSignals(e.incidents()))
}

// Different containers are different faults, sidecars included, whichever pod
// they share (re-review N-1).
func TestFamily_DifferentContainersAreDifferentFaults(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.count.signals = []detection.Signal{
		e.signalFor(detection.SignalCrashLoopBackOff, detection.CategoryHealth, detection.SeverityCritical, e.pod, "app"),
		e.signalFor(detection.SignalRestartLoop, detection.CategoryHealth, detection.SeverityWarning, e.pod, "sidecar"),
	}
	for range 3 {
		e.cycle()
	}
	open := e.open()
	require.Len(t, open, 2, "got %s", incidentSignals(e.incidents()))
	containers := map[string]string{}
	for _, im := range open {
		containers[im.Annotations[AnnotationContainers]] = im.Spec.Detection.Signal
	}
	assert.Equal(t, map[string]string{"app": "CrashLoopBackOff", "sidecar": "RestartLoop"}, containers,
		"each incident records the container it is about")
}

// The same container's fault changes shape on a pod that was rolled: no pod in
// common, but the old pod's fault has ended, so it is one incident.
func TestFamily_ARolledPodMergesWhenTheOldOnesFaultHasEnded(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.container = "main"
	e.setCrashLoop()
	e.cycle()
	e.pod = dorguv1.ResourceReference{Kind: "Pod", Name: "api-def", Namespace: recurrenceNS}
	e.setReadiness()
	e.cycle()
	assert.Len(t, e.open(), 1, "got %s", incidentSignals(e.incidents()))
}

// The container a fault is about is written on the incident, and survives a merge
// of a fault from another container only as its own incident.
func TestFamily_TheContainerIsRecordedOnTheIncident(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.container = "main"
	e.setReadiness()
	e.cycle()
	assert.Equal(t, "main", e.onlyOpen().Annotations[AnnotationContainers])
}

func TestFamily_ThePodSurvivesAcrossTheShapes(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	e.setCrashLoop()
	e.cycle()
	im := e.onlyOpen()
	require.NotEmpty(t, im.Spec.Detection.AffectedResources)
	assert.Equal(t, e.pod.Name, im.Spec.Detection.AffectedResources[0].Name)
}
