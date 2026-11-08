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
	e.count.signals = []detection.Signal{{
		Type: typ, Severity: severity, Category: category, Source: "stub",
		Message:  "container api: " + string(typ),
		Resource: e.pod,
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS,
		},
		DetectedAt: time.Now(),
	}}
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

func TestFamily_EscalationNeverLowersSeverityOrOpensASecondIncident(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setCrashLoop()
	e.cycle()
	require.Equal(t, string(detection.SeverityCritical), e.onlyOpen().Spec.Severity)

	// The container recovers to Running and not Ready: a lower rung of the same
	// fault. It does not open an incident and does not demote the open one.
	e.setReadiness()
	e.cycle()

	im := e.onlyOpen()
	require.Len(t, e.incidents(), 1, "got %s", incidentSignals(e.incidents()))
	assert.Equal(t, "CrashLoopBackOff", im.Spec.Detection.Signal)
	assert.Equal(t, string(detection.SeverityCritical), im.Spec.Severity)
	assert.EqualValues(t, 2, im.Status.OccurrenceCount, "the sighting still counts")
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
	got := e.r.previousOccurrences(personaSubject(*diag.PersonaRef))(context.Background(), *diag)
	assert.EqualValues(t, 2, got, "the prompt must not call an escalating fault a first sighting")
}
