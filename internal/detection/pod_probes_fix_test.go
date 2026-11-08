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
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// B9 fix round: F-4 (kubelet events decide what a restart loop is), F-5 (initial
// delay), F-7 (first sight), F-9 (the tracker and the event reads).

// killingEvent is the kubelet's record that it killed a container for failing its
// liveness probe.
func killingEvent(pod string, count int32, at metav1.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: pod + "-killing", Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Name: pod, Namespace: "shop", UID: k8stypes.UID("uid-" + pod), FieldPath: "spec.containers{main}",
		},
		Reason:         "Killing",
		Message:        "Container main failed liveness probe, will be restarted",
		Count:          count,
		FirstTimestamp: at,
		LastTimestamp:  at,
	}
}

func loopSignal(sigs []Signal) (Signal, bool) {
	for _, s := range sigs {
		if s.Type == SignalLivenessRestarts || s.Type == SignalRestartLoop {
			return s, true
		}
	}
	return Signal{}, false
}

// ---- F-4 -------------------------------------------------------------------

func TestLiveness_KubeletKillEventsMakeItALivenessFailure(t *testing.T) {
	c := newClock()
	evs := []corev1.Event{
		killingEvent("api-1", 2, ago(c, 20*time.Second)),
		probeEvent("api-1", "Liveness probe failed: connection refused", 4, ago(c, 20*time.Second)),
	}
	cycles := livenessCyclesWithEvents(t, c, nil, evs,
		restartedRunning(c, 3, "Error", 137, 10*time.Second),
		restartedRunning(c, 5, "Error", 137, 10*time.Second),
	)

	s, ok := loopSignal(cycles[1])
	require.True(t, ok)
	assert.Equal(t, SignalLivenessRestarts, s.Type)
	assert.Equal(t, CategoryHealth, s.Category)
	assert.Equal(t, "2", s.Metadata["livenessKillEvents"], "the primary evidence")
	assert.Equal(t, "4", s.Metadata["livenessProbeEvents"], "supporting")
	assert.Contains(t, s.Message, "2 kubelet liveness kill events")
}

func TestLiveness_KillEventsAloneAreEnoughAtFirstSight(t *testing.T) {
	c := newClock()
	// An operator that has just started has no restart history, but the kubelet's
	// events are cluster state, not operator memory.
	evs := []corev1.Event{killingEvent("api-1", 3, ago(c, 20*time.Second))}
	cycles := livenessCyclesWithEvents(t, c, nil, evs, restartedRunning(c, 6, "Completed", 0, 30*time.Second))

	s, ok := loopSignal(cycles[0])
	require.True(t, ok, "got %v", signalTypes(cycles[0]))
	assert.Equal(t, SignalLivenessRestarts, s.Type)
}

func TestLiveness_OneKillEventAtFirstSightIsNotALoop(t *testing.T) {
	c := newClock()
	evs := []corev1.Event{killingEvent("api-1", 1, ago(c, 20*time.Second))}
	cycles := livenessCyclesWithEvents(t, c, nil, evs, restartedRunning(c, 6, "Completed", 0, 30*time.Second))
	assert.False(t, hasLoop(cycles[0]))
}

func TestLiveness_KillEventsOutsideTheWindowDoNotCount(t *testing.T) {
	c := newClock()
	evs := []corev1.Event{killingEvent("api-1", 5, ago(c, time.Hour))}
	cycles := livenessCyclesWithEvents(t, c, nil, evs,
		restartedRunning(c, 3, "Error", 137, 10*time.Second),
		restartedRunning(c, 5, "Error", 137, 10*time.Second),
	)
	s, ok := loopSignal(cycles[1])
	require.True(t, ok)
	assert.Equal(t, SignalRestartLoop, s.Type, "an hour-old kill is not evidence about the last 10 minutes")
}

// An aggregated event reports a lifetime count: 50 kills over three days with the
// last one a minute ago says "at least one in the window", not fifty.
func TestLiveness_AggregatedEventsAreCountedOnlyInsideTheWindow(t *testing.T) {
	c := newClock()
	ev := killingEvent("api-1", 50, ago(c, time.Minute))
	ev.FirstTimestamp = ago(c, 72*time.Hour)
	cycles := livenessCyclesWithEvents(t, c, nil, []corev1.Event{ev},
		restartedRunning(c, 3, "Completed", 0, 10*time.Second),
		restartedRunning(c, 5, "Completed", 0, 10*time.Second),
	)
	s, ok := loopSignal(cycles[1])
	require.True(t, ok, "restart delta 2 plus a kill in the window")
	assert.Equal(t, "1", s.Metadata["livenessKillEvents"])
}

func TestLiveness_NoKillEventsMakeAnErrorExitARestartLoop(t *testing.T) {
	c := newClock()
	cycles := livenessCyclesWithEvents(t, c, nil, nil,
		restartedRunning(c, 3, "Error", 1, 10*time.Second),
		restartedRunning(c, 5, "Error", 1, 10*time.Second),
	)
	s, ok := loopSignal(cycles[1])
	require.True(t, ok)
	assert.Equal(t, SignalRestartLoop, s.Type, "an ordinary crash looks exactly like this: do not call it a probe failure")
	assert.Equal(t, CategoryHealth, s.Category)
	assert.NotContains(t, s.Message, "liveness")
	assert.Equal(t, "0", s.Metadata["livenessKillEvents"])
}

func TestLiveness_UnreadableEventsDegradeToARestartLoop(t *testing.T) {
	c := newClock()
	opts := []PodCollectorOption{WithEventReader(failingEventReader{})}
	cycles := livenessCyclesWithEvents(t, c, opts, nil,
		restartedRunning(c, 3, "Error", 137, 10*time.Second),
		restartedRunning(c, 5, "Error", 137, 10*time.Second),
	)
	s, ok := loopSignal(cycles[1])
	require.True(t, ok)
	assert.Equal(t, SignalRestartLoop, s.Type)
	assert.Equal(t, evidenceUnavailable, s.Metadata["livenessKillEvents"])
}

func TestLiveness_KillEventsDoNotOverrideAnOOMKill(t *testing.T) {
	c := newClock()
	evs := []corev1.Event{killingEvent("api-1", 3, ago(c, 20*time.Second))}
	cycles := livenessCyclesWithEvents(t, c, nil, evs,
		restartedRunning(c, 3, "OOMKilled", 137, 3*time.Hour),
		restartedRunning(c, 5, "OOMKilled", 137, 10*time.Second),
	)
	assert.True(t, hasType(cycles[1], SignalOOMKilled))
	assert.False(t, hasLoop(cycles[1]), "the OOM owns the container")
}

// ---- F-5 -------------------------------------------------------------------

func TestReadiness_InitialDelayIsCountedFromTheContainerStart(t *testing.T) {
	c := newClock()
	probe := &corev1.Probe{InitialDelaySeconds: 300}
	mk := func(startedAgo time.Duration) corev1.Pod {
		pod := runningPod("api-1", runningNotReady(c, startedAgo), containersReadyFalse(c, startedAgo))
		pod.Spec.Containers[0].ReadinessProbe = probe
		return pod
	}

	// Started 6 minutes ago, first probe at 5 minutes: 1 minute of failing, under 2.
	assert.False(t, hasType(collectWith(t, c, []corev1.Pod{mk(6 * time.Minute)}, nil), SignalReadinessNotReady))
	// Started 8 minutes ago: probing for 3 minutes, over the threshold.
	assert.True(t, hasType(collectWith(t, c, []corev1.Pod{mk(8 * time.Minute)}, nil), SignalReadinessNotReady))
}

func TestReadiness_NoInitialDelayChangesNothing(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, 3*time.Minute), containersReadyFalse(c, 3*time.Minute))
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{PeriodSeconds: 10}
	assert.True(t, hasType(collectWith(t, c, []corev1.Pod{pod}, nil), SignalReadinessNotReady))
}

// ---- F-9 -------------------------------------------------------------------

func TestRestartTracker_ChangePointsAndBaseline(t *testing.T) {
	c := newClock()
	tr := newRestartTracker(10 * time.Minute)

	_, h := tr.observe("k", c.t, 3)
	assert.Equal(t, historyFirstSight, h)
	for range 5 { // the count holds steady
		c.advance(time.Minute)
		delta, h := tr.observe("k", c.t, 3)
		assert.Equal(t, historyInWindow, h)
		assert.Zero(t, delta)
	}
	c.advance(time.Minute)
	delta, h := tr.observe("k", c.t, 5)
	assert.Equal(t, historyInWindow, h)
	assert.EqualValues(t, 2, delta)

	// Eleven minutes after the first sight the 3 has aged out of the window but is
	// still the count at the window's edge, so the delta is still exact.
	c.advance(5 * time.Minute)
	delta, _ = tr.observe("k", c.t, 5)
	assert.EqualValues(t, 2, delta, "the change was 5 minutes ago")
	c.advance(5 * time.Minute)
	delta, _ = tr.observe("k", c.t, 5)
	assert.Zero(t, delta, "and now it is 10 minutes ago, out of the window")

	tr.mu.Lock()
	defer tr.mu.Unlock()
	assert.LessOrEqual(t, len(tr.samples["k"]), 2, "a baseline and the latest change, not one point per cycle")
}

func TestRestartTracker_ALongGapProvesNothing(t *testing.T) {
	c := newClock()
	tr := newRestartTracker(10 * time.Minute)
	tr.observe("k", c.t, 3)
	c.advance(30 * time.Minute) // cycles were skipped
	delta, h := tr.observe("k", c.t, 5)
	assert.Equal(t, historyNone, h, "+2 over 30 minutes could have all been in the first one")
	assert.Zero(t, delta)
	c.advance(time.Minute)
	delta, h = tr.observe("k", c.t, 6)
	assert.Equal(t, historyInWindow, h, "history resumes from the reseed")
	assert.EqualValues(t, 1, delta)
}

// countingReader counts List calls.
type countingReader struct {
	client.Reader
	lists atomic.Int32
}

func (r *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists.Add(1)
	return r.Reader.List(ctx, list, opts...)
}

// fakeWithEventIndex is a fake client that answers the collector's field-selected
// event lists, as the API server does.
func fakeWithEventIndex(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(runtimeSchemeForTest(t)).
		WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Name}
		}).Build()
}

func manyNotReadyPods(c *fakeClock, n int) []corev1.Pod {
	pods := make([]corev1.Pod, 0, n)
	for i := range n {
		pods = append(pods, runningPod(fmt.Sprintf("api-%03d", i), runningNotReady(c, 10*time.Minute), containersReadyFalse(c, 5*time.Minute)))
	}
	return pods
}

// A node loss leaves hundreds of pods not Ready. Each one is a signal, but the
// event lookups are bounded per cycle, so the API server is not asked hundreds of
// times a minute.
func TestEventReads_AreBoundedPerCycle(t *testing.T) {
	c := newClock()
	pods := manyNotReadyPods(c, maxEventReadsPerCycle+15)
	base := fakeWithEventIndex(t)
	for i := range pods {
		require.NoError(t, base.Create(ctxBG(), &pods[i]))
	}
	reader := &countingReader{Reader: base}
	p := NewPodCollector(base, testLogger(), withClock(c.now), WithEventReader(reader))

	sigs, err := p.Collect(ctxBG())
	require.NoError(t, err)

	assert.Len(t, sigs, len(pods), "every pod still raises its signal")
	assert.LessOrEqual(t, int(reader.lists.Load()), maxEventReadsPerCycle)
	unavailable := 0
	for _, s := range sigs {
		if s.Metadata["readinessProbeEvents"] == evidenceUnavailable {
			unavailable++
		}
	}
	assert.Equal(t, 15, unavailable, "the pods past the budget say their evidence was not read")
}

// Two containers of one pod share one read.
func TestEventReads_AreSharedAcrossAPodsContainers(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, 10*time.Minute), containersReadyFalse(c, 5*time.Minute))
	second := runningNotReady(c, 10*time.Minute)
	second.Name = "sidecar"
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, second)
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "sidecar"})

	base := fakeWithEventIndex(t)
	require.NoError(t, base.Create(ctxBG(), &pod))
	reader := &countingReader{Reader: base}
	p := NewPodCollector(base, testLogger(), withClock(c.now), WithEventReader(reader))

	sigs, err := p.Collect(ctxBG())
	require.NoError(t, err)
	assert.Len(t, sigs, 2)
	assert.EqualValues(t, 1, reader.lists.Load())
}
