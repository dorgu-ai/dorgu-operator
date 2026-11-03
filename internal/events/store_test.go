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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func newTestInternalEvent(id, source, name, namespace string, severity Severity) *InternalEvent {
	return &InternalEvent{
		ID:       id,
		Severity: severity,
		Category: CategoryHealth,
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

func TestStore_Deduplication(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger, WithDedupWindow(1*time.Hour))

	ctx := context.Background()

	event1 := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical)
	err := store.Store(ctx, event1)
	require.NoError(t, err)

	// Same source+involvedObject+category should be deduplicated.
	event2 := newTestInternalEvent("e2", "kubelet", "test-pod", "default", SeverityCritical)
	err = store.Store(ctx, event2)
	require.NoError(t, err)

	assert.Len(t, storedEvents(t, fakeClient), 1, "duplicate event should be suppressed")
}

func TestStore_DedupWindowExpiry(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger, WithDedupWindow(1*time.Millisecond))

	ctx := context.Background()

	event1 := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical)
	err := store.Store(ctx, event1)
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond) // Wait for dedup window to expire.

	event2 := newTestInternalEvent("e2", "kubelet", "test-pod", "default", SeverityCritical)
	event2.EventTime = event1.EventTime.Add(time.Second) // a later occurrence, so a distinct record
	err = store.Store(ctx, event2)
	require.NoError(t, err)

	assert.Len(t, storedEvents(t, fakeClient), 2, "event after dedup window should be stored")
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
	event := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical)

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

func TestGenerateCRDName(t *testing.T) {
	event := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical)

	name := generateCRDName(event)
	assert.True(t, len(name) <= MaxK8sNameLength, "name should not exceed K8s limit")
	assert.Contains(t, name, "de-default-pod-test-pod-")
}

func TestGenerateCRDName_Deterministic(t *testing.T) {
	event := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical)

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

// storedEvents lists the DorguEvents the store persisted.
func storedEvents(t *testing.T, c client.Client) []dorguv1.DorguEvent {
	t.Helper()
	var list dorguv1.DorguEventList
	require.NoError(t, c.List(context.Background(), &list))
	return list.Items
}
