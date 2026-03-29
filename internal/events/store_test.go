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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func newTestInternalEvent(id, source, name, namespace string, severity Severity, category Category) *InternalEvent {
	return &InternalEvent{
		ID:       id,
		Severity: severity,
		Category: category,
		Source:   source,
		Message:  "test message for " + name,
		InvolvedObject: dorguv1.ResourceReference{
			Kind:      "Pod",
			Name:      name,
			Namespace: namespace,
		},
		EventTime:   time.Now(),
		K8sEventUID: "uid-" + id,
	}
}

func TestStore_StoreAndQueryFromCache(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger)

	ctx := context.Background()
	event := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical, CategoryHealth)

	err := store.Store(ctx, event)
	require.NoError(t, err)

	results, err := store.Query(ctx, EventFilter{})
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "test-pod", results[0].InvolvedObject.Name)
}

func TestStore_Deduplication(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger, WithDedupWindow(1*time.Hour))

	ctx := context.Background()

	event1 := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical, CategoryHealth)
	err := store.Store(ctx, event1)
	require.NoError(t, err)

	// Same source+involvedObject+category should be deduplicated.
	event2 := newTestInternalEvent("e2", "kubelet", "test-pod", "default", SeverityCritical, CategoryHealth)
	err = store.Store(ctx, event2)
	require.NoError(t, err)

	results, err := store.Query(ctx, EventFilter{})
	require.NoError(t, err)
	assert.Len(t, results, 1, "duplicate event should be suppressed")
}

func TestStore_DedupWindowExpiry(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger, WithDedupWindow(1*time.Millisecond))

	ctx := context.Background()

	event1 := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical, CategoryHealth)
	err := store.Store(ctx, event1)
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond) // Wait for dedup window to expire.

	event2 := newTestInternalEvent("e2", "kubelet", "test-pod", "default", SeverityCritical, CategoryHealth)
	err = store.Store(ctx, event2)
	require.NoError(t, err)

	results, err := store.Query(ctx, EventFilter{})
	require.NoError(t, err)
	assert.Len(t, results, 2, "event after dedup window should be stored")
}

func TestStore_CacheEviction(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger,
		WithCacheSize(3),
		WithDedupWindow(0), // Disable dedup for this test.
	)

	ctx := context.Background()

	for i := range 5 {
		event := newTestInternalEvent(
			fmt.Sprintf("e%d", i),
			fmt.Sprintf("source-%d", i),
			fmt.Sprintf("pod-%d", i),
			"default",
			SeverityInfo,
			CategoryHealth,
		)
		err := store.Store(ctx, event)
		require.NoError(t, err)
	}

	results, err := store.Query(ctx, EventFilter{})
	require.NoError(t, err)
	assert.Len(t, results, 3, "cache should not exceed max size")
	// Should contain the last 3 events.
	assert.Equal(t, "pod-2", results[0].InvolvedObject.Name)
	assert.Equal(t, "pod-3", results[1].InvolvedObject.Name)
	assert.Equal(t, "pod-4", results[2].InvolvedObject.Name)
}

func TestStore_QueryWithFilter(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger, WithDedupWindow(0))

	ctx := context.Background()

	events := []*InternalEvent{
		newTestInternalEvent("e1", "s1", "pod-1", "production", SeverityCritical, CategoryHealth),
		newTestInternalEvent("e2", "s2", "pod-2", "staging", SeverityWarning, CategoryResource),
		newTestInternalEvent("e3", "s3", "pod-3", "production", SeverityInfo, CategoryScaling),
	}
	for _, e := range events {
		require.NoError(t, store.Store(ctx, e))
	}

	t.Run("filter by namespace", func(t *testing.T) {
		results, err := store.Query(ctx, EventFilter{Namespace: "production"})
		require.NoError(t, err)
		assert.Len(t, results, 2)
	})

	t.Run("filter by severity", func(t *testing.T) {
		results, err := store.Query(ctx, EventFilter{Severity: SeverityCritical})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, SeverityCritical, results[0].Severity)
	})

	t.Run("filter by category", func(t *testing.T) {
		results, err := store.Query(ctx, EventFilter{Category: CategoryResource})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, CategoryResource, results[0].Category)
	})

	t.Run("filter with limit", func(t *testing.T) {
		results, err := store.Query(ctx, EventFilter{Limit: 2})
		require.NoError(t, err)
		assert.Len(t, results, 2)
	})

	t.Run("filter by since", func(t *testing.T) {
		future := time.Now().Add(1 * time.Hour)
		results, err := store.Query(ctx, EventFilter{Since: &future})
		require.NoError(t, err)
		assert.Len(t, results, 0)
	})
}

func TestStore_Count(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger, WithDedupWindow(0))

	ctx := context.Background()

	for i := range 3 {
		event := newTestInternalEvent(
			fmt.Sprintf("e%d", i),
			fmt.Sprintf("source-%d", i),
			fmt.Sprintf("pod-%d", i),
			"default",
			SeverityCritical,
			CategoryHealth,
		)
		require.NoError(t, store.Store(ctx, event))
	}

	count, err := store.Count(ctx, EventFilter{Severity: SeverityCritical})
	require.NoError(t, err)
	assert.Equal(t, 3, count)

	count, err = store.Count(ctx, EventFilter{Severity: SeverityWarning})
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestStore_NilEvent(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger)

	err := store.Store(context.Background(), nil)
	assert.NoError(t, err)
}

func TestStore_CRDPersistence(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger)

	ctx := context.Background()
	event := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical, CategoryHealth)

	err := store.Store(ctx, event)
	require.NoError(t, err)

	// Verify the DorguEvent CRD was created.
	var list dorguv1.DorguEventList
	err = fakeClient.List(ctx, &list)
	require.NoError(t, err)
	assert.Len(t, list.Items, 1)
	assert.Equal(t, "critical", list.Items[0].Spec.Severity)
	assert.Equal(t, "health", list.Items[0].Spec.Category)
	assert.Equal(t, "kubelet", list.Items[0].Spec.Source)
}

func TestStore_QueryFallbackToAPI(t *testing.T) {
	scheme := testScheme()

	// Pre-populate a DorguEvent CRD directly (simulating existing data).
	existingEvent := &dorguv1.DorguEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "de-existing-event",
			Namespace: "default",
		},
		Spec: dorguv1.DorguEventSpec{
			Severity:   "warning",
			Category:   "resource",
			Source:     "kubelet",
			Message:    "existing event from CRD",
			InvolvedObject: dorguv1.ResourceReference{
				Kind:      "Pod",
				Name:      "old-pod",
				Namespace: "default",
			},
			EventTime: metav1.Time{Time: time.Now().Add(-2 * time.Hour)},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingEvent).Build()
	logger := zap.New(zap.UseDevMode(true))
	// Empty cache — should fall back to API.
	store := NewEventStore(fakeClient, logger)

	ctx := context.Background()
	past := time.Now().Add(-3 * time.Hour)
	results, err := store.Query(ctx, EventFilter{Since: &past})
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "old-pod", results[0].InvolvedObject.Name)
}

func TestGenerateCRDName(t *testing.T) {
	event := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical, CategoryHealth)

	name := generateCRDName(event)
	assert.True(t, len(name) <= MaxK8sNameLength, "name should not exceed K8s limit")
	assert.Contains(t, name, "de-default-pod-test-pod-")
}

func TestGenerateCRDName_Deterministic(t *testing.T) {
	event := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical, CategoryHealth)

	name1 := generateCRDName(event)
	name2 := generateCRDName(event)
	assert.Equal(t, name1, name2)
}

func TestSanitizeK8sName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"UpperCase", "uppercase"},
		{"with.dots", "with-dots"},
		{"with/slashes", "with-slashes"},
		{"with_underscore", "with-underscore"},
		{"already-valid-123", "already-valid-123"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, sanitizeK8sName(tt.input))
		})
	}
}

func TestMatchesFilter(t *testing.T) {
	now := time.Now()
	pastHour := now.Add(-1 * time.Hour)
	event := InternalEvent{
		Severity: SeverityCritical,
		Category: CategoryHealth,
		InvolvedObject: dorguv1.ResourceReference{
			Namespace: "production",
		},
		EventTime: now,
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona",
			Name: "api-server",
		},
	}

	assert.True(t, matchesFilter(event, EventFilter{}), "empty filter matches all")
	assert.True(t, matchesFilter(event, EventFilter{Namespace: "production"}))
	assert.False(t, matchesFilter(event, EventFilter{Namespace: "staging"}))
	assert.True(t, matchesFilter(event, EventFilter{Severity: SeverityCritical}))
	assert.False(t, matchesFilter(event, EventFilter{Severity: SeverityInfo}))
	assert.True(t, matchesFilter(event, EventFilter{Category: CategoryHealth}))
	assert.False(t, matchesFilter(event, EventFilter{Category: CategoryNode}))
	assert.True(t, matchesFilter(event, EventFilter{Since: &pastHour}))

	futureTime := now.Add(1 * time.Hour)
	assert.False(t, matchesFilter(event, EventFilter{Since: &futureTime}))
}

func TestApplyLimit(t *testing.T) {
	events := []InternalEvent{{ID: "1"}, {ID: "2"}, {ID: "3"}}

	assert.Len(t, applyLimit(events, 0), 3, "0 limit returns all")
	assert.Len(t, applyLimit(events, 2), 2)
	assert.Len(t, applyLimit(events, 5), 3, "limit > len returns all")
	assert.Len(t, applyLimit(events, 3), 3, "limit == len returns all")
}
