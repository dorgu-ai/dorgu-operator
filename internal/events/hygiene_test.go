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

// Event hygiene: clean-room run #3 findings F-06 and F-10.
//
// F-06 — 171 of the operator's 188 ERROR lines in a 20-minute window came from a
// duplicate DorguEvent key. The write those lines reported had already
// succeeded, the condition was idempotent, and the noise buried the
// DorguDiagnosisDiscarded records the docs tell users to alert on.
//
// F-10 — 187 DorguEvent records after 100 minutes on a five-app cluster, growing
// without bound: a 24-hour TTL caps nothing when the arrival rate scales with
// the cluster, and the operator had no delete permission to enforce even that.
//
// Every test below fails against the code as it shipped in v0.9.0.
package events

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// TestStore_ReDeliveredEventIsIdempotentNotAnError reproduces the source of the
// 171 ERROR lines. DorguEvent names are content-addressed, the informer re-lists
// every event on each resync, and the resync period was the same 5 minutes as
// the dedup window, so a settled event came back round just as its dedup entry
// expired. The Create then returned AlreadyExists for a record that was already
// on the API server, which is not a failure and must not be reported as one.
func TestStore_ReDeliveredEventIsIdempotentNotAnError(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	// Dedup off, so the second delivery reaches the API server exactly as it did
	// in the cluster once the window had expired.
	store := NewEventStore(fakeClient, logger, WithDedupWindow(0))

	ctx := context.Background()
	event := newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityCritical, CategoryHealth)

	require.NoError(t, store.Store(ctx, event))
	require.NoError(t, store.Store(ctx, event),
		"re-delivering an already recorded event is idempotent and must not surface as an error")

	var list dorguv1.DorguEventList
	require.NoError(t, fakeClient.List(ctx, &list))
	assert.Len(t, list.Items, 1, "the same event must not be recorded twice")
}

// TestStore_DistinctReasonsAreBothRecorded pins the identity of a record. The
// dedup key and the CRD name both ignored the Kubernetes Event reason, so two
// unrelated things happening to one object in one category collapsed into a
// single record and the second was dropped. Applied to an IncidentMemory that
// meant a DorguDiagnosisDiscarded could be suppressed by an ordinary health
// event, silently removing the record users are told to alert on.
func TestStore_DistinctReasonsAreBothRecorded(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger, WithDedupWindow(1*time.Hour))

	ctx := context.Background()
	at := time.Now()

	detected := newTestInternalEvent("e1", "healthcheck-reconciler", "im-checkout", "apps",
		SeverityCritical, CategoryHealth)
	detected.Reason = ReasonDorguDetected
	detected.EventTime = at

	discarded := newTestInternalEvent("e2", "healthcheck-reconciler", "im-checkout", "apps",
		SeverityCritical, CategoryHealth)
	discarded.Reason = "DorguDiagnosisDiscarded"
	discarded.EventTime = at

	require.NoError(t, store.Store(ctx, detected))
	require.NoError(t, store.Store(ctx, discarded))

	var list dorguv1.DorguEventList
	require.NoError(t, fakeClient.List(ctx, &list))
	assert.Len(t, list.Items, 2,
		"a discarded diagnosis must not be suppressed by an unrelated event on the same object")
}

// TestClassifier_CarriesTheEventReason makes the reason available to everything
// downstream. Without it the store cannot tell one kind of event apart from
// another on the same object, and the emitter relabels every re-ingested event
// as a generic detection.
func TestClassifier_CarriesTheEventReason(t *testing.T) {
	classifier := NewClassifier()

	internal := classifier.Classify(newTestEvent("OOMKilling", "Container killed due to OOM",
		corev1.EventTypeWarning))

	require.NotNil(t, internal)
	assert.Equal(t, "OOMKilling", internal.Reason)
}

// TestClassifier_DiscardsDorgusOwnEvents stops the pipeline observing its own
// output. The emitter writes a Kubernetes Event for every record it stores, the
// watcher then saw that Event and stored a second DorguEvent saying dorgu had
// said something. DorguDetected and DorguDiagnosisDiscarded accounted for 24 of
// the 171 duplicate-key errors, and every one of those records was an echo.
func TestClassifier_DiscardsDorgusOwnEvents(t *testing.T) {
	classifier := NewClassifier()

	tests := []struct {
		name   string
		mutate func(*corev1.Event)
	}{
		{
			name:   "recorded via Source.Component",
			mutate: func(e *corev1.Event) { e.Source = corev1.EventSource{Component: OperatorEventSource} },
		},
		{
			name: "recorded via ReportingController",
			mutate: func(e *corev1.Event) {
				e.Source = corev1.EventSource{}
				e.ReportingController = OperatorEventSource
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := newTestEvent("DorguDiagnosisDiscarded",
				"[dorgu] critical: discarded the claude diagnosis", corev1.EventTypeWarning)
			tt.mutate(event)

			assert.Nil(t, classifier.Classify(event),
				"dorgu must not re-ingest an event it recorded itself")
		})
	}

	// The same event from any other component still flows through, so dropping
	// the echo costs no real observation.
	fromKubelet := newTestEvent("Unhealthy", "Readiness probe failed", corev1.EventTypeWarning)
	assert.NotNil(t, classifier.Classify(fromKubelet))
}

// TestWatcher_ReDeliveredEventStillReachesTheEmitter is the "crowded out" half
// of F-06. Store returned the benign AlreadyExists as an error and processEvent
// returned on it, so a re-delivered event never reached step 4. The pipeline
// aborted on a condition that meant the write had already succeeded.
func TestWatcher_ReDeliveredEventStillReachesTheEmitter(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))

	// A fixed EventTime is what the informer actually re-delivers: LastTimestamp
	// has second granularity and does not move for a settled event.
	classified := newTestInternalEvent("e1", "kubelet", "test-pod", "default",
		SeverityCritical, CategoryHealth)
	classified.EventTime = time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)

	emitter := &mockEmitter{}
	watcher := NewWatcher(
		nil,
		&mockClassifier{result: classified},
		&mockCorrelator{},
		NewEventStore(fakeClient, logger, WithDedupWindow(0)),
		emitter,
		logger,
	)

	ctx := context.Background()
	raw := newTestEvent("BackOff", "Back-off restarting failed container", corev1.EventTypeWarning)
	raw.LastTimestamp = metav1.Time{Time: time.Now()} // Inside the startup window.

	watcher.processEvent(ctx, raw)
	watcher.processEvent(ctx, raw)

	assert.Len(t, emitter.emitted(), 2,
		"a duplicate record must not abort the pipeline before the emit step")
}

// TestCleaner_Cleanup_EnforcesRecordCap is F-10. A TTL bounds age, not count:
// 187 records in 100 minutes on five apps extrapolates to roughly 2,700 a day,
// all of them inside the 24-hour window and all of them in etcd. The cap is the
// bound that holds regardless of cluster size, and it prunes oldest first.
func TestCleaner_Cleanup_EnforcesRecordCap(t *testing.T) {
	scheme := testScheme()

	const total = 10
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for i := range total {
		// All fresh, so nothing here is expired: only the cap can prune them.
		de := newTestDorguEvent(
			fmt.Sprintf("de-fresh-%02d", i),
			time.Now().Add(-time.Duration(total-i)*time.Minute),
			nil,
		)
		builder = builder.WithObjects(de)
	}
	fakeClient := builder.Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger, WithMaxRecords(4))

	require.NoError(t, cleaner.cleanup(context.Background()))

	var list dorguv1.DorguEventList
	require.NoError(t, fakeClient.List(context.Background(), &list))
	require.Len(t, list.Items, 4, "unexpired records over the cap must still be pruned")

	survivors := make([]string, 0, len(list.Items))
	for _, de := range list.Items {
		survivors = append(survivors, de.Name)
	}
	assert.ElementsMatch(t, []string{"de-fresh-06", "de-fresh-07", "de-fresh-08", "de-fresh-09"},
		survivors, "the cap must keep the newest records and drop the oldest")
}

// TestCleaner_Cleanup_HonoursConfiguredRetention proves the retention knob
// reaches records already on the API server. A record's own spec.TTL still wins,
// because a per-record TTL is a deliberate override.
func TestCleaner_Cleanup_HonoursConfiguredRetention(t *testing.T) {
	scheme := testScheme()

	// Two hours old with no TTL of its own: inside the 24-hour default, outside a
	// configured one-hour retention.
	noTTL := newTestDorguEvent("de-no-ttl", time.Now().Add(-2*time.Hour), nil)

	ownTTL := 72 * time.Hour
	explicit := newTestDorguEvent("de-own-ttl", time.Now().Add(-2*time.Hour), &ownTTL)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(noTTL, explicit).Build()
	logger := zap.New(zap.UseDevMode(true))
	cleaner := NewCleaner(fakeClient, logger, WithRetention(1*time.Hour))

	require.NoError(t, cleaner.cleanup(context.Background()))

	var list dorguv1.DorguEventList
	require.NoError(t, fakeClient.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, "de-own-ttl", list.Items[0].Name,
		"a record's own TTL overrides the configured retention")
}

// TestStore_StampsTheConfiguredRetention keeps the two halves in step: whatever
// retention the operator is running with is what new records carry, so
// `kubectl get dorguevent -o yaml` states the real answer.
func TestStore_StampsTheConfiguredRetention(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := zap.New(zap.UseDevMode(true))
	store := NewEventStore(fakeClient, logger, WithTTL(6*time.Hour))

	ctx := context.Background()
	require.NoError(t, store.Store(ctx,
		newTestInternalEvent("e1", "kubelet", "test-pod", "default", SeverityInfo, CategoryHealth)))

	var list dorguv1.DorguEventList
	require.NoError(t, fakeClient.List(ctx, &list))
	require.Len(t, list.Items, 1)
	require.NotNil(t, list.Items[0].Spec.TTL)
	assert.Equal(t, metav1.Duration{Duration: 6 * time.Hour}, *list.Items[0].Spec.TTL)
}
