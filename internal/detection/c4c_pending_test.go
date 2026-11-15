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

package detection

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// C4c (review of PR 69, F-2): the resolution warm-up is per container as well as per
// source. A source that is warm as a whole can still be new to one container (a replaced
// pod, metrics-server lag for a new pod, a partial answer), and until that container has
// been read for as many cycles as the signal needs, a pod pinned at its limit reads as
// quiet. ResolutionPending names those containers.

var apiApp = ContainerRef{Namespace: throttleNS, Pod: "api-1", Container: "app"}

func hotUsage() map[string]string { return map[string]string{"api-1 app": "990m"} }

// two limited pods: api-1 (the one under test) and api-2 (keeps the source answering).
func twoPods() []client.Object {
	return []client.Object{
		throttlePod("api-1", map[string]string{"app": "1000m"}),
		throttlePod("api-2", map[string]string{"app": "1000m"}),
	}
}

func withOther(usage map[string]string) map[string]string {
	out := map[string]string{"api-2 app": "50m"}
	maps.Copy(out, usage)
	return out
}

func TestC4cF2_AHotContainerIsPendingUntilItsSignalIsRaised(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(hotUsage())
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics))

	assert.Empty(t, c.ResolutionPending(), "before the first cycle")
	cycle(c)
	assert.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending(), "hot for 1 cycle: no signal yet")
	cycle(c)
	assert.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending(), "hot for 2 cycles: no signal yet")
	signals, err := c.Collect(context.Background())
	require.NoError(t, err)
	require.Len(t, signals, 1)
	assert.Empty(t, c.ResolutionPending(), "the third cycle raises the signal, which is its own hold")
}

func TestC4cF2_AQuietContainerIsNeverPending(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(quietUsage())
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics))
	for range 2 * CPUUsageConsecutiveCycles {
		cycle(c)
		assert.Empty(t, c.ResolutionPending())
	}
}

// A burst that ends is not a hold: the container reads below the line, so silence about it
// is the truth.
func TestC4cF2_ABurstThatEndsIsNotPending(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(hotUsage())
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics))
	cycle(c)
	require.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending())
	metrics.set(quietUsage())
	cycle(c)
	assert.Empty(t, c.ResolutionPending(), "one read below the line: it is not throttled and was never throttled")
}

// The container that was throttled (it had a signal) and then drops out of the source is
// unknown, not quiet. It is held until it has been read for CPUUsageConsecutiveCycles
// cycles again.
func TestC4cF2_AThrottledContainerThatLeavesTheSourceIsPendingUntilItIsReadAgain(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(withOther(hotUsage()))
	c := newThrottleCollector(t, twoPods(), WithMetricsSource(metrics))
	for range CPUUsageConsecutiveCycles {
		cycle(c)
	}
	require.Empty(t, c.ResolutionPending(), "it is a signal now")

	metrics.set(withOther(nil)) // api-1 is gone from the answer; api-2 still answers
	for i := range 4 {
		cycle(c)
		assert.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending(), "absent cycle %d", i+1)
	}

	metrics.set(withOther(map[string]string{"api-1 app": "40m"})) // back, and quiet
	for i := 1; i < CPUUsageConsecutiveCycles; i++ {
		cycle(c)
		assert.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending(), "read %d of 3 since it came back", i)
	}
	cycle(c)
	assert.Empty(t, c.ResolutionPending(), "three reads since it came back: its silence is evidence again")
}

func TestC4cF2_AThrottledContainerThatComesBackHotIsPendingThenASignal(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(withOther(hotUsage()))
	c := newThrottleCollector(t, twoPods(), WithMetricsSource(metrics))
	for range CPUUsageConsecutiveCycles {
		cycle(c)
	}
	metrics.set(withOther(nil))
	cycle(c)
	require.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending())

	metrics.set(withOther(hotUsage()))
	cycle(c)
	assert.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending(), "hot, streak 1")
	cycle(c)
	assert.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending(), "hot, streak 2")
	signals, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.Len(t, signals, 1, "streak 3")
	assert.Empty(t, c.ResolutionPending())
}

// A pod that is no longer in the cluster is not unknown, it is gone: nothing is held for it.
func TestC4cF2_APodThatIsGoneReleasesItsHold(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(twoPods()...).Build()
	metrics := &fakeMetricsSource{present: true}
	metrics.set(withOther(hotUsage()))
	c := NewCPUThrottlingCollector(cl, logr.Discard(), WithMinCycleSpacing(0), WithMetricsSource(metrics))
	for range CPUUsageConsecutiveCycles {
		cycle(c)
	}
	metrics.set(withOther(nil))
	cycle(c)
	require.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending())

	require.NoError(t, cl.Delete(context.Background(), throttlePod("api-1", map[string]string{"app": "1000m"})))
	cycle(c)
	assert.Empty(t, c.ResolutionPending(), "the pod was deleted: there is nothing left to read")
}

func TestC4cF2_AFinishedPodReleasesItsHold(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	done := throttlePod("api-1", map[string]string{"app": "1000m"})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(done, throttlePod("api-2", map[string]string{"app": "1000m"})).Build()
	metrics := &fakeMetricsSource{present: true}
	metrics.set(withOther(hotUsage()))
	c := NewCPUThrottlingCollector(cl, logr.Discard(), WithMinCycleSpacing(0), WithMetricsSource(metrics))
	for range CPUUsageConsecutiveCycles {
		cycle(c)
	}
	metrics.set(withOther(nil))
	cycle(c)
	require.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending())

	var live corev1.Pod
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(done), &live))
	live.Status.Phase = corev1.PodFailed
	require.NoError(t, cl.Status().Update(context.Background(), &live))
	cycle(c)
	assert.Empty(t, c.ResolutionPending(), "a finished pod never reports usage again")
}

// A cycle that could not look breaks every reading streak, and a container that was
// throttled stays held through it.
func TestC4cF2_ACycleThatCouldNotLookKeepsAThrottledContainerHeld(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(withOther(hotUsage()))
	c := newThrottleCollector(t, twoPods(), WithMetricsSource(metrics))
	for range CPUUsageConsecutiveCycles {
		cycle(c)
	}
	metrics.set(withOther(nil))
	cycle(c) // out of the source for a cycle: its reading streak is gone
	metrics.set(withOther(map[string]string{"api-1 app": "40m"}))
	cycle(c)
	cycle(c) // read twice, quiet: 2 of 3
	require.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending())

	metrics.mu.Lock()
	metrics.listErr = assert.AnError
	metrics.mu.Unlock()
	cycle(c)
	metrics.mu.Lock()
	metrics.listErr = nil
	metrics.mu.Unlock()
	for i := 1; i < CPUUsageConsecutiveCycles; i++ {
		cycle(c)
		assert.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending(), "read %d after the break", i)
	}
	cycle(c)
	assert.Empty(t, c.ResolutionPending())
}

// Prometheus measures throttling directly, over five minutes; it has no per-container
// reading streak to wait for.
func TestC4cF2_PrometheusHasNothingPending(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(hotUsage())
	prom := &fakeThrottleSource{samples: []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.1}}}
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics), WithThrottleSource(prom))
	cycle(c)
	assert.Empty(t, c.ResolutionPending())
}

func TestC4cF2_ANamespaceTheGateRefusesIsNeverPending(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(hotUsage())
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics),
		WithCPUNamespaceFilter(excludeNS{throttleNS: true}))
	cycle(c)
	assert.Empty(t, c.ResolutionPending())
}

func TestC4cF2_AnAnswerFromTheCacheDoesNotMovePending(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(hotUsage())
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics),
		WithMinCycleSpacing(30*time.Second), WithCPUClock(func() time.Time { return now }))
	cycle(c)
	require.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending())
	metrics.set(quietUsage())
	cycle(c) // inside the spacing window: answered from cache
	assert.Equal(t, []ContainerRef{apiApp}, c.ResolutionPending(), "the cached call is not a cycle")
}

func TestC4cF2_TheEngineSnapshotNamesTheBlindFamiliesAndThePendingContainers(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(hotUsage())
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics))
	e := NewEngine(logr.Discard(), c)

	_, err := e.CollectAll(context.Background())
	require.NoError(t, err)
	snap := e.ResolutionSnapshot()
	assert.True(t, snap.Blind(FamilyCPUThrottling), "cycle 1 of the warm-up")
	assert.True(t, snap.Pending(FamilyCPUThrottling, apiApp))
	assert.False(t, snap.Pending(FamilyCPUThrottling, ContainerRef{Namespace: throttleNS, Pod: "other", Container: "app"}))
	assert.Equal(t, e.ResolutionBlindFamilies(), snap.BlindFamilies)

	for range CPUUsageConsecutiveCycles {
		_, err = e.CollectAll(context.Background())
		require.NoError(t, err)
	}
	snap = e.ResolutionSnapshot()
	assert.False(t, snap.Blind(FamilyCPUThrottling))
	assert.False(t, snap.Pending(FamilyCPUThrottling, apiApp), "a signal is raised: that is its own hold")
}

// A zero snapshot (no engine, an engine with no collectors) holds nothing.
func TestC4cF2_AZeroSnapshotHoldsNothing(t *testing.T) {
	var snap ResolutionSnapshot
	assert.False(t, snap.Blind(FamilyCPUThrottling))
	assert.False(t, snap.Pending(FamilyCPUThrottling, apiApp))
	assert.Empty(t, NewEngine(logr.Discard()).ResolutionSnapshot().PendingContainers)
}
