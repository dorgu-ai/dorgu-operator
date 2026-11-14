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
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// C4 (P2-D11): CPU throttling is a signal family of its own and takes the incident
// paths the restart/OOM family takes: a fault is the application, the CONTAINER and the
// family; a recurrence after Resolved is a new record of the same fault; a human's no
// holds across it.

const (
	cpuNS      = "shop"
	cpuPersona = "api"
)

type cpuEnv struct {
	t         *testing.T
	c         client.Client
	r         *HealthCheckReconciler
	collector *stubCollector
}

func throttleSignalFor(pod, container string) detection.Signal {
	pct, threshold := 98.0, 95.0
	return detection.Signal{
		Type: detection.SignalCPUThrottling, Severity: detection.SeverityWarning, Category: detection.CategoryResource,
		Source:     "cpu-throttling-checker",
		Message:    "container " + container + " is at its CPU limit",
		Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: pod, Namespace: cpuNS},
		PersonaRef: &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: cpuPersona, Namespace: cpuNS},
		Value:      &pct, Threshold: &threshold,
		DetectedAt: time.Now(),
		Metadata:   map[string]string{"container": container, "source": "metrics.k8s.io"},
	}
}

func newCPUEnv(t *testing.T, signals ...detection.Signal) *cpuEnv {
	t.Helper()
	c := noiseClient(t, recurrencePersonaObject())
	collector := &stubCollector{name: "stub", signals: signals}
	r := newNoiseReconciler(c, collector)
	r.Proposer = remediation.NewProposer(c, gate.New(c, noiseLogger()), noiseLogger())
	return &cpuEnv{t: t, c: c, r: r, collector: collector}
}

func (e *cpuEnv) cycle() { e.r.reconcile(context.Background()) }

func (e *cpuEnv) incidents() []dorguv1.IncidentMemory { return listIncidents(e.t, e.c) }

func (e *cpuEnv) actions() []dorguv1.RemediationAction {
	var list dorguv1.RemediationActionList
	require.NoError(e.t, e.c.List(context.Background(), &list))
	return list.Items
}

func (e *cpuEnv) resolve(name string) {
	e.t.Helper()
	key := client.ObjectKey{Namespace: cpuNS, Name: name}
	var im dorguv1.IncidentMemory
	require.NoError(e.t, e.c.Get(context.Background(), key, &im))
	im.Labels[LabelPhase] = PhaseResolved
	im.Spec.Resolution = &dorguv1.ResolutionInfo{Action: "none", Outcome: "resolved"}
	require.NoError(e.t, e.c.Update(context.Background(), &im))
	require.NoError(e.t, e.c.Get(context.Background(), key, &im))
	im.Status.Phase = PhaseResolved
	require.NoError(e.t, e.c.Status().Update(context.Background(), &im))
}

func TestCPUThrottling_OneContainerIsOneIncidentAndOneProposal(t *testing.T) {
	e := newCPUEnv(t, throttleSignalFor("api-abc", "app"))
	e.cycle()
	e.cycle()
	e.cycle()

	incidents := e.incidents()
	require.Len(t, incidents, 1, "the same throttled container across cycles is one incident: %s", incidentSignals(incidents))
	im := incidents[0]
	assert.Equal(t, string(detection.SignalCPUThrottling), im.Spec.Detection.Signal)
	assert.Equal(t, "resource", im.Spec.Category)
	assert.Equal(t, "app", im.Annotations[AnnotationContainers], "the incident records the container it is about")

	actions := e.actions()
	require.Len(t, actions, 1, "one fix proposed, not one per cycle")
	ra := actions[0]
	assert.Equal(t, RemediationPhasePending, ra.Status.Phase)
	assert.Equal(t, string(detection.SignalCPUThrottling), ra.Annotations[gate.AnnotationAnswersSignal])
	require.Len(t, ra.Spec.Steps, 1)
	assert.Contains(t, string(ra.Spec.Steps[0].Patch.Raw), `"cpu":"625m"`, "500m persona limit, +25 percent")
}

func TestCPUThrottling_IsNotMergedWithTheFaultLadder(t *testing.T) {
	pod := dorguv1.ResourceReference{Kind: "Pod", Name: "api-abc", Namespace: cpuNS}
	ref := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: cpuPersona, Namespace: cpuNS}
	oom := detection.Signal{
		Type: detection.SignalOOMKilled, Severity: detection.SeverityCritical, Category: detection.CategoryResource,
		Source: "stub", Message: "app was OOMKilled", Resource: pod, PersonaRef: ref, DetectedAt: time.Now(),
		Metadata: map[string]string{"container": "app"},
	}
	e := newCPUEnv(t, throttleSignalFor("api-abc", "app"), oom)
	e.cycle()
	e.cycle()

	signals := map[string]bool{}
	for _, im := range e.incidents() {
		signals[im.Spec.Detection.Signal] = true
	}
	assert.Equal(t, map[string]bool{"CPUThrottling": true, "OOMKilled": true}, signals,
		"a container that is throttled and one that is dying are two faults, with two fixes; "+
			"neither escalates into the other: %s", incidentSignals(e.incidents()))
	for _, im := range e.incidents() {
		assert.Empty(t, im.Annotations[AnnotationEscalatedFrom], "no escalation trail for %s", im.Spec.Detection.Signal)
	}
}

// A fault is application + container + family. CPU throttling in container "app"
// that resolved and came back is a recurrence; CPU throttling in a DIFFERENT container
// of the application is a first occurrence of a different fault, with its own
// budget, generation and cooldown.
func TestCPUThrottling_IdentityIncludesTheContainer(t *testing.T) {
	e := newCPUEnv(t, throttleSignalFor("api-abc", "app"))
	e.cycle()
	require.Len(t, e.incidents(), 1)
	first := e.incidents()[0]
	e.resolve(first.Name)

	// Same container again: a recurrence of the same fault.
	e.cycle()
	var recurrence *dorguv1.IncidentMemory
	for _, im := range e.incidents() {
		if im.Status.Phase != PhaseResolved {
			recurrence = &im
		}
	}
	require.NotNil(t, recurrence, "the recurrence opens a new record: %s", incidentSignals(e.incidents()))
	assert.Equal(t, 1, gate.IncidentGeneration(recurrence.Name), "same container: generation 1")
	assert.True(t, gate.SameIncidentLineage(first.Name, recurrence.Name))
	e.resolve(recurrence.Name)

	// A different container: not a recurrence of "app".
	e.collector.signals = []detection.Signal{throttleSignalFor("api-abc", "sidecar")}
	e.cycle()
	var other *dorguv1.IncidentMemory
	for _, im := range e.incidents() {
		if im.Status.Phase != PhaseResolved {
			other = &im
		}
	}
	require.NotNil(t, other)
	assert.Equal(t, 0, gate.IncidentGeneration(other.Name),
		"a different container is a different fault: its first occurrence, not a recurrence of app's")
	assert.Equal(t, "sidecar", other.Annotations[AnnotationContainers])
}

func TestCPUThrottling_RejectionHoldsAcrossARecurrence(t *testing.T) {
	e := newCPUEnv(t, throttleSignalFor("api-abc", "app"))
	e.cycle()
	require.Len(t, e.actions(), 1)
	ra := e.actions()[0]

	// A human says no.
	var live dorguv1.RemediationAction
	require.NoError(t, e.c.Get(context.Background(), client.ObjectKeyFromObject(&ra), &live))
	live.Status.Phase = RemediationPhaseRejected
	live.Status.Conditions = append(live.Status.Conditions, metav1.Condition{
		Type: ConditionRejected, Status: metav1.ConditionTrue, Reason: ReasonUserRejected,
		Message: "declined by a human", LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, e.c.Status().Update(context.Background(), &live))

	e.resolve(e.incidents()[0].Name)
	e.cycle() // the fault comes back
	e.cycle()

	assert.Len(t, e.actions(), 1, "the human's no still covers the same fault when it recurs")
}

func TestCPUThrottling_ClearsWhenTheSignalGoes(t *testing.T) {
	e := newCPUEnv(t, throttleSignalFor("api-abc", "app"))
	e.cycle()
	require.Len(t, e.incidents(), 1)

	e.collector.signals = nil
	e.cycle()
	im := e.incidents()[0]
	assert.Equal(t, PhaseResolved, im.Status.Phase, "a throttled container that stops being throttled resolves its incident")
}

func TestCPUThrottling_BlindCycleDoesNotResolve(t *testing.T) {
	e := newCPUEnv(t, throttleSignalFor("api-abc", "app"))
	e.cycle()
	require.Len(t, e.incidents(), 1)

	// The metrics source is down: the collector fails. Silence is not evidence.
	e.collector.signals = nil
	e.collector.err = assertErr("metrics API unavailable")
	e.cycle()
	assert.NotEqual(t, PhaseResolved, e.incidents()[0].Status.Phase,
		"B2b: a collector that could not look must never read as healthy")
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
