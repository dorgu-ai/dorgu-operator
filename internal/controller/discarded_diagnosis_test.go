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
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
)

// updateConflicter fails the first n Update calls for a named object with a
// Conflict, then delegates. n < 0 means "always conflict".
type updateConflicter struct {
	name  string
	fail  int
	calls int
}

func (uc *updateConflicter) fn() func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
	return func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() != uc.name {
			return c.Update(ctx, obj, opts...)
		}
		uc.calls++
		if uc.fail < 0 || uc.calls <= uc.fail {
			return newConflict(uc.name)
		}
		return c.Update(ctx, obj, opts...)
	}
}

// capturingEventStore records everything handed to it so a test can assert that
// a failure was surfaced and not swallowed.
type capturingEventStore struct {
	mu     sync.Mutex
	stored []events.InternalEvent
}

func (s *capturingEventStore) Store(_ context.Context, e *events.InternalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stored = append(s.stored, *e)
	return nil
}

func (s *capturingEventStore) Query(_ context.Context, _ events.EventFilter) ([]events.InternalEvent, error) {
	return nil, nil
}

func (s *capturingEventStore) Count(_ context.Context, _ events.EventFilter) (int, error) {
	return 0, nil
}

// capturingEmitter records the K8s Events the reconciler emits.
type capturingEmitter struct {
	mu       sync.Mutex
	emitted  []events.InternalEvent
	emitFail error
}

func (e *capturingEmitter) Emit(_ context.Context, ev *events.InternalEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emitted = append(e.emitted, *ev)
	return e.emitFail
}

// logEntry is one line a recordingSink captured.
type logEntry struct {
	isError bool
	msg     string
}

// recordingSink is a logr.LogSink that keeps every line, so a test can assert
// that a failure was reported at ERROR rather than whispered at V(1).
type recordingSink struct {
	mu      *sync.Mutex
	entries *[]logEntry
}

func newRecordingLogger() (logr.Logger, *recordingSink) {
	sink := &recordingSink{mu: &sync.Mutex{}, entries: &[]logEntry{}}
	return logr.New(sink), sink
}

func (s *recordingSink) Init(logr.RuntimeInfo)        {}
func (s *recordingSink) Enabled(int) bool             { return true }
func (s *recordingSink) WithName(string) logr.LogSink { return s }

func (s *recordingSink) WithValues(...any) logr.LogSink { return s }

func (s *recordingSink) Info(_ int, msg string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.entries = append(*s.entries, logEntry{msg: msg})
}

func (s *recordingSink) Error(_ error, msg string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.entries = append(*s.entries, logEntry{isError: true, msg: msg})
}

// hasError reports whether an ERROR line containing substr was recorded.
func (s *recordingSink) hasError(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range *s.entries {
		if e.isError && strings.Contains(e.msg, substr) {
			return true
		}
	}
	return false
}

// messages returns every recorded line, for assertion failure output.
func (s *recordingSink) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(*s.entries))
	for _, e := range *s.entries {
		prefix := "INFO "
		if e.isError {
			prefix = "ERROR "
		}
		out = append(out, prefix+e.msg)
	}
	return out
}

// aiDiagnosis is the diagnosis the tests treat as the paid-for, AI-enhanced one
// that must survive a conflicting write.
func aiDiagnosis() *diagnosis.Diagnosis {
	return &diagnosis.Diagnosis{
		PersonaRef: &dorguv1.PersonaReference{
			Kind:      "ApplicationPersona",
			Name:      "api",
			Namespace: "default",
		},
		Category:        "health",
		Severity:        detection.SeverityCritical,
		Confidence:      0.92,
		Provider:        "ai-anthropic",
		Summary:         "container exceeds its 32Mi memory limit during startup cache warm",
		SuggestedAction: "resource-adjustment",
		Contributing: []diagnosis.ContributingSignal{
			{
				Signal: detection.Signal{
					Type:     detection.SignalOOMKilled,
					Severity: detection.SeverityCritical,
					Category: detection.CategoryResource,
					Source:   "pod-collector",
					Message:  "container api was OOMKilled",
					Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "api-0", Namespace: "default"},
				},
				Detail: "container memory limit: 32Mi",
			},
		},
	}
}

// activeIncident is a Detected IncidentMemory carrying a weaker, rule-based root
// cause, i.e. exactly the state where the AI diagnosis is the better one.
func activeIncident() *dorguv1.IncidentMemory {
	return &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "im-default-api-oomkilled-cf4",
			Namespace: "default",
			Labels: map[string]string{
				LabelPersonaKind:      "ApplicationPersona",
				LabelPersonaName:      "api",
				LabelPersonaNamespace: "default",
				LabelCategory:         "health",
				LabelSeverity:         string(detection.SeverityCritical),
				LabelSignal:           reasonOOMKilled,
				LabelPhase:            PhaseDetected,
			},
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      "api",
				Namespace: "default",
			},
			Category: "health",
			Severity: string(detection.SeverityCritical),
			Detection: dorguv1.DetectionInfo{
				Signal:    reasonOOMKilled,
				Source:    "pod-collector",
				FirstSeen: metav1.Now(),
				LastSeen:  metav1.Now(),
			},
			RootCause: &dorguv1.RootCauseInfo{
				Summary:    "pod restarted",
				Confidence: "0.60",
				Provider:   "rule-based",
			},
		},
		Status: dorguv1.IncidentMemoryStatus{
			Phase:           PhaseDetected,
			OccurrenceCount: 1,
		},
	}
}

func cf4Scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))
	return scheme
}

// TestUpdateExistingIncident_RetriesConflictedSpecWrite reproduces F-01. The
// IncidentMemory handed to updateExistingIncident comes from a List, so a
// concurrent write invalidates its ResourceVersion and the spec Update fails
// with "the object has been modified". Before the fix that error was returned
// and the AI diagnosis was thrown away; 176 of them vanished in 4h20m.
func TestUpdateExistingIncident_RetriesConflictedSpecWrite(t *testing.T) {
	im := activeIncident()
	base := fake.NewClientBuilder().
		WithScheme(cf4Scheme(t)).
		WithObjects(im).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	conflicter := &updateConflicter{name: im.Name, fail: 1}
	c := interceptor.NewClient(base, interceptor.Funcs{Update: conflicter.fn()})

	logger, _ := newRecordingLogger()
	r := &HealthCheckReconciler{Client: c, Logger: logger}

	diag := aiDiagnosis()
	require.NoError(t, r.updateExistingIncident(context.Background(), im, diag, metav1.Now()),
		"a conflicted spec write must be retried, not returned")

	var got dorguv1.IncidentMemory
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
	require.NotNil(t, got.Spec.RootCause)
	assert.Equal(t, diag.Summary, got.Spec.RootCause.Summary,
		"the AI diagnosis must survive the conflict")
	assert.Equal(t, "ai-anthropic", got.Spec.RootCause.Provider)
	assert.Equal(t, int32(2), got.Status.OccurrenceCount)
	assert.GreaterOrEqual(t, conflicter.calls, 2, "the spec update should have been retried")
}

// TestUpdateExistingIncident_SurfacesUnrecoverableDiscard covers the other half
// of F-01: when the retries are exhausted the diagnosis really is lost, and that
// loss must be loud. It costs an AI call, so it may never disappear into a V(1)
// line.
func TestUpdateExistingIncident_SurfacesUnrecoverableDiscard(t *testing.T) {
	im := activeIncident()
	base := fake.NewClientBuilder().
		WithScheme(cf4Scheme(t)).
		WithObjects(im).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	conflicter := &updateConflicter{name: im.Name, fail: -1}
	c := interceptor.NewClient(base, interceptor.Funcs{Update: conflicter.fn()})

	store := &capturingEventStore{}
	emitter := &capturingEmitter{}
	logger, sink := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		EventStore:   store,
		EventEmitter: emitter,
	}

	diag := aiDiagnosis()
	err := r.updateExistingIncident(context.Background(), im, diag, metav1.Now())
	require.Error(t, err, "an unrecoverable conflict must still be reported to the caller")

	assert.True(t, sink.hasError("discard"),
		"the discarded diagnosis must be logged at ERROR, got: %v", sink.messages())

	require.Len(t, store.stored, 1, "the discard must be recorded as a DorguEvent")
	assert.Equal(t, events.SeverityCritical, store.stored[0].Severity)
	assert.Contains(t, store.stored[0].Message, im.Name)
	assert.Equal(t, "IncidentMemory", store.stored[0].InvolvedObject.Kind)

	require.Len(t, emitter.emitted, 1, "the discard must reach kubectl as a K8s Event")
	assert.Equal(t, ReasonDiagnosisDiscarded, emitter.emitted[0].Reason)
}

// TestCreateIncident_RetriesConflictedStatusWrite covers the same bug class on
// the create path: the status write straight after Create used to fail hard,
// leaving a statusless incident and dropping the diagnosis with it.
func TestCreateIncident_RetriesConflictedStatusWrite(t *testing.T) {
	base := fake.NewClientBuilder().
		WithScheme(cf4Scheme(t)).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	counter := &conflictCounter{}
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: counter.interceptor(""),
	})

	logger, _ := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
	}

	diag := aiDiagnosis()
	require.NoError(t, r.createIncident(context.Background(), diag, metav1.Now()),
		"a conflicted initial status write must be retried")
	assert.GreaterOrEqual(t, counter.calls, 2)

	var list dorguv1.IncidentMemoryList
	require.NoError(t, base.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, PhaseDetected, list.Items[0].Status.Phase)
	assert.Equal(t, int32(1), list.Items[0].Status.OccurrenceCount)
}

// TestProcessDiagnosis_ReportsDiscardToCycleTally proves a failed persist
// reaches the caller and that a cycle which lost work says so at ERROR. The
// clean-room run only spotted the 176 losses by grepping raw logs, because
// nothing ever counted them.
func TestProcessDiagnosis_ReportsDiscardToCycleTally(t *testing.T) {
	im := activeIncident()
	base := fake.NewClientBuilder().
		WithScheme(cf4Scheme(t)).
		WithObjects(im).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	conflicter := &updateConflicter{name: im.Name, fail: -1}
	c := interceptor.NewClient(base, interceptor.Funcs{Update: conflicter.fn()})

	logger, sink := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
	}

	require.Error(t, r.processDiagnosis(context.Background(), aiDiagnosis(), map[string]bool{}),
		"a diagnosis that cannot be persisted must be reported to the cycle")

	r.logCycleSummary(1, 1)
	assert.True(t, sink.hasError("discarded"),
		"a cycle that lost diagnoses must say so at ERROR, got: %v", sink.messages())
}
