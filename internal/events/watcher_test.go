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

package events

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// mockClassifier implements Classifier for testing.
type mockClassifier struct {
	result *InternalEvent
}

func (m *mockClassifier) Classify(event *corev1.Event) *InternalEvent {
	if m.result != nil {
		// Copy and fill in event-specific data.
		r := *m.result
		r.K8sEventUID = string(event.UID)
		r.Message = event.Message
		return &r
	}
	return nil
}

// mockCorrelator implements Correlator for testing.
type mockCorrelator struct {
	personaRef *dorguv1.PersonaReference
	err        error
}

func (m *mockCorrelator) Correlate(_ context.Context, event *InternalEvent) error {
	if m.err != nil {
		return m.err
	}
	if m.personaRef != nil {
		event.PersonaRef = m.personaRef
	}
	return nil
}

// mockStore implements EventStore for testing.
type mockStore struct {
	mu     sync.Mutex
	events []InternalEvent
	err    error
}

func (m *mockStore) Store(_ context.Context, event *InternalEvent) error {
	if m.err != nil {
		return m.err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, *event)
	return nil
}

func (m *mockStore) Query(_ context.Context, _ EventFilter) ([]InternalEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events, nil
}

func (m *mockStore) Count(_ context.Context, _ EventFilter) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events), nil
}

func (m *mockStore) stored() []InternalEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]InternalEvent, len(m.events))
	copy(result, m.events)
	return result
}

// mockEmitter implements Emitter for testing.
type mockEmitter struct {
	mu     sync.Mutex
	events []InternalEvent
	err    error
}

func (m *mockEmitter) Emit(_ context.Context, event *InternalEvent) error {
	if m.err != nil {
		return m.err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, *event)
	return nil
}

func (m *mockEmitter) emitted() []InternalEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]InternalEvent, len(m.events))
	copy(result, m.events)
	return result
}

func TestWatcher_ProcessEvent(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	classifier := &mockClassifier{
		result: &InternalEvent{
			ID:       "test-id",
			Severity: SeverityCritical,
			Category: CategoryHealth,
			Source:   "kubelet",
		},
	}
	correlator := &mockCorrelator{
		personaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona",
			Name: "api-server",
		},
	}
	store := &mockStore{}
	emitter := &mockEmitter{}

	w := &Watcher{
		classifier:  classifier,
		correlator:  correlator,
		store:       store,
		emitter:     emitter,
		logger:      logger,
		startupTime: time.Now(),
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-event",
			UID:  types.UID("uid-123"),
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "api-server-xyz",
			Namespace: "production",
		},
		Reason:        "OOMKilling",
		Message:       "Container killed due to OOM",
		Type:          corev1.EventTypeWarning,
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	w.processEvent(context.Background(), event)

	// Verify store received the event.
	stored := store.stored()
	require.Len(t, stored, 1)
	assert.Equal(t, SeverityCritical, stored[0].Severity)
	assert.Equal(t, "Container killed due to OOM", stored[0].Message)

	// Verify persona was correlated.
	require.NotNil(t, stored[0].PersonaRef)
	assert.Equal(t, "api-server", stored[0].PersonaRef.Name)

	// Verify emitter received the event.
	emitted := emitter.emitted()
	require.Len(t, emitted, 1)
	assert.Equal(t, SeverityCritical, emitted[0].Severity)
}

func TestWatcher_ProcessEvent_DiscardedByClassifier(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	classifier := &mockClassifier{result: nil} // Classifier returns nil.
	store := &mockStore{}
	emitter := &mockEmitter{}

	w := &Watcher{
		classifier:  classifier,
		correlator:  &mockCorrelator{},
		store:       store,
		emitter:     emitter,
		logger:      logger,
		startupTime: time.Now(),
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: "normal-event",
			UID:  types.UID("uid-456"),
		},
		Reason:        "Scheduled",
		Message:       "Successfully assigned pod",
		Type:          corev1.EventTypeNormal,
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	w.processEvent(context.Background(), event)

	assert.Empty(t, store.stored(), "discarded event should not be stored")
	assert.Empty(t, emitter.emitted(), "discarded event should not be emitted")
}

func TestWatcher_ProcessEvent_NilEmitter(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	classifier := &mockClassifier{
		result: &InternalEvent{
			ID:       "test-id",
			Severity: SeverityWarning,
			Category: CategoryNode,
			Source:   "kubelet",
		},
	}
	store := &mockStore{}

	w := &Watcher{
		classifier:  classifier,
		correlator:  &mockCorrelator{},
		store:       store,
		emitter:     nil, // No emitter configured.
		logger:      logger,
		startupTime: time.Now(),
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-event",
			UID:  types.UID("uid-789"),
		},
		Reason:        "MemoryPressure",
		Message:       "Node has memory pressure",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	w.processEvent(context.Background(), event)

	// Store should still receive the event.
	stored := store.stored()
	require.Len(t, stored, 1)
}

func TestWatcher_IsOldEvent(t *testing.T) {
	w := &Watcher{
		startupTime: time.Now(),
	}

	tests := []struct {
		name     string
		event    *corev1.Event
		expected bool
	}{
		{
			name: "recent event is not old",
			event: &corev1.Event{
				LastTimestamp: metav1.Time{Time: time.Now().Add(-1 * time.Minute)},
			},
			expected: false,
		},
		{
			name: "old event is filtered",
			event: &corev1.Event{
				LastTimestamp: metav1.Time{Time: time.Now().Add(-30 * time.Minute)},
			},
			expected: true,
		},
		{
			name: "event at startup window boundary is not old",
			event: &corev1.Event{
				LastTimestamp: metav1.Time{Time: time.Now().Add(-9 * time.Minute)},
			},
			expected: false,
		},
		{
			name: "zero timestamp event is not old",
			event: &corev1.Event{
				LastTimestamp: metav1.Time{},
			},
			expected: false,
		},
		{
			name: "uses CreationTimestamp as fallback",
			event: &corev1.Event{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Time{Time: time.Now().Add(-30 * time.Minute)},
				},
				LastTimestamp: metav1.Time{},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, w.isOldEvent(tt.event))
		})
	}
}

func TestWatcher_ProcessEvent_CorrelationError(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	classifier := &mockClassifier{
		result: &InternalEvent{
			ID:       "test-id",
			Severity: SeverityCritical,
			Category: CategoryHealth,
			Source:   "kubelet",
		},
	}
	correlator := &mockCorrelator{err: assert.AnError}
	store := &mockStore{}
	emitter := &mockEmitter{}

	w := &Watcher{
		classifier:  classifier,
		correlator:  correlator,
		store:       store,
		emitter:     emitter,
		logger:      logger,
		startupTime: time.Now(),
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-event",
			UID:  types.UID("uid-err"),
		},
		Reason:        "OOMKilling",
		Message:       "OOM",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	// Should continue processing even if correlation fails.
	w.processEvent(context.Background(), event)

	stored := store.stored()
	require.Len(t, stored, 1, "event should be stored even if correlation fails")
}

func TestWatcher_ProcessEvent_StoreError(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	classifier := &mockClassifier{
		result: &InternalEvent{
			ID:       "test-id",
			Severity: SeverityCritical,
			Category: CategoryHealth,
			Source:   "kubelet",
		},
	}
	store := &mockStore{err: assert.AnError}
	emitter := &mockEmitter{}

	w := &Watcher{
		classifier:  classifier,
		correlator:  &mockCorrelator{},
		store:       store,
		emitter:     emitter,
		logger:      logger,
		startupTime: time.Now(),
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-event",
			UID:  types.UID("uid-err2"),
		},
		Reason:        "OOMKilling",
		Message:       "OOM",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	// Should not emit if store fails.
	w.processEvent(context.Background(), event)
	assert.Empty(t, emitter.emitted(), "should not emit when store fails")
}

func TestNewWatcher(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	w := NewWatcher(nil, &mockClassifier{}, &mockCorrelator{}, &mockStore{}, &mockEmitter{}, logger)

	assert.NotNil(t, w)
	assert.Equal(t, DefaultResyncPeriod, w.resyncPeriod)
}

func TestNewWatcher_WithOptions(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	w := NewWatcher(nil, &mockClassifier{}, &mockCorrelator{}, &mockStore{}, &mockEmitter{}, logger,
		WithResyncPeriod(1*time.Minute),
	)

	assert.Equal(t, 1*time.Minute, w.resyncPeriod)
}
