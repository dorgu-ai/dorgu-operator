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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func newTestDorguEvent(name string, eventTime time.Time, ttl *time.Duration) *dorguv1.DorguEvent {
	const namespace = "default"
	de := &dorguv1.DorguEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: dorguv1.DorguEventSpec{
			Severity:  "warning",
			Category:  "health",
			Source:    "test",
			Message:   "test event",
			EventTime: metav1.Time{Time: eventTime},
			InvolvedObject: dorguv1.ResourceReference{
				Kind:      "Pod",
				Name:      "test-pod",
				Namespace: namespace,
			},
		},
	}
	if ttl != nil {
		de.Spec.TTL = &metav1.Duration{Duration: *ttl}
	}
	return de
}

func TestCleaner_Cleanup_DeletesExpiredEvents(t *testing.T) {
	scheme := testScheme()

	expired := newTestDorguEvent("de-expired",
		time.Now().Add(-48*time.Hour), nil) // 48h old, default TTL is 24h
	fresh := newTestDorguEvent("de-fresh",
		time.Now().Add(-1*time.Hour), nil) // 1h old, still within 24h TTL

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(expired, fresh).
		Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger)

	err := cleaner.cleanup(context.Background())
	require.NoError(t, err)

	// Verify only expired event was deleted.
	var list dorguv1.DorguEventList
	err = fakeClient.List(context.Background(), &list)
	require.NoError(t, err)
	assert.Len(t, list.Items, 1)
	assert.Equal(t, "de-fresh", list.Items[0].Name)
}

func TestCleaner_Cleanup_RespectsCustomTTL(t *testing.T) {
	scheme := testScheme()

	shortTTL := 10 * time.Minute
	// 15 minutes old with 10-minute TTL — should be expired.
	expiredCustom := newTestDorguEvent("de-custom-expired",
		time.Now().Add(-15*time.Minute), &shortTTL)

	longTTL := 72 * time.Hour
	// 48 hours old with 72-hour TTL — should NOT be expired.
	freshCustom := newTestDorguEvent("de-custom-fresh",
		time.Now().Add(-48*time.Hour), &longTTL)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(expiredCustom, freshCustom).
		Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger)

	err := cleaner.cleanup(context.Background())
	require.NoError(t, err)

	var list dorguv1.DorguEventList
	err = fakeClient.List(context.Background(), &list)
	require.NoError(t, err)
	assert.Len(t, list.Items, 1)
	assert.Equal(t, "de-custom-fresh", list.Items[0].Name)
}

func TestCleaner_Cleanup_BatchSizeLimit(t *testing.T) {
	scheme := testScheme()

	objects := make([]dorguv1.DorguEvent, 0, 5)
	for i := range 5 {
		de := newTestDorguEvent(
			"de-expired-"+string(rune('a'+i)),
			time.Now().Add(-48*time.Hour),
			nil,
		)
		objects = append(objects, *de)
	}

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for i := range objects {
		builder = builder.WithObjects(&objects[i])
	}
	fakeClient := builder.Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger, WithBatchSize(3))

	err := cleaner.cleanup(context.Background())
	require.NoError(t, err)

	var list dorguv1.DorguEventList
	err = fakeClient.List(context.Background(), &list)
	require.NoError(t, err)
	assert.Len(t, list.Items, 2, "should only delete batchSize events per cycle")
}

func TestCleaner_Cleanup_NoExpiredEvents(t *testing.T) {
	scheme := testScheme()

	fresh := newTestDorguEvent("de-fresh", time.Now(), nil)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(fresh).
		Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger)

	err := cleaner.cleanup(context.Background())
	require.NoError(t, err)

	var list dorguv1.DorguEventList
	err = fakeClient.List(context.Background(), &list)
	require.NoError(t, err)
	assert.Len(t, list.Items, 1, "fresh event should not be deleted")
}

func TestCleaner_Cleanup_EmptyList(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger)

	err := cleaner.cleanup(context.Background())
	assert.NoError(t, err)
}

func TestCleaner_Start_StopsOnContextCancel(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger, WithCleanupInterval(100*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- cleaner.Start(ctx)
	}()

	// Let it run one cycle.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("cleaner did not stop after context cancellation")
	}
}

func TestIsExpired(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		de       *dorguv1.DorguEvent
		expected bool
	}{
		{
			name:     "expired with default TTL",
			de:       newTestDorguEvent("de-1", now.Add(-25*time.Hour), nil),
			expected: true,
		},
		{
			name:     "not expired with default TTL",
			de:       newTestDorguEvent("de-2", now.Add(-23*time.Hour), nil),
			expected: false,
		},
		{
			name: "expired with custom TTL",
			de: func() *dorguv1.DorguEvent {
				ttl := 1 * time.Hour
				return newTestDorguEvent("de-3", now.Add(-2*time.Hour), &ttl)
			}(),
			expected: true,
		},
		{
			name: "not expired with custom TTL",
			de: func() *dorguv1.DorguEvent {
				ttl := 48 * time.Hour
				return newTestDorguEvent("de-4", now.Add(-24*time.Hour), &ttl)
			}(),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isExpired(tt.de, now))
		})
	}
}

func TestNewCleaner_Defaults(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger)

	assert.Equal(t, DefaultCleanupInterval, cleaner.interval)
	assert.Equal(t, DefaultCleanupBatchSize, cleaner.batchSize)
}

func TestNewCleaner_WithOptions(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger,
		WithCleanupInterval(10*time.Minute),
		WithBatchSize(100),
	)

	assert.Equal(t, 10*time.Minute, cleaner.interval)
	assert.Equal(t, 100, cleaner.batchSize)
}
