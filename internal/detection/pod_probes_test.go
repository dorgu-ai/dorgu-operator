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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// B9 (D0.8 findings 1 and 3): probe failures become health signals, and a stale
// terminated state stops being one.

// fakeClock is a settable clock for the collector.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)} }

func ago(c *fakeClock, d time.Duration) metav1.Time { return metav1.NewTime(c.t.Add(-d)) }

// runningPod is a Running pod with one container whose state the caller sets.
func runningPod(name string, cs corev1.ContainerStatus, conds ...corev1.PodCondition) corev1.Pod {
	cs.Name = "main"
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: k8stypes.UID("uid-" + name)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{cs},
			Conditions:        conds,
		},
	}
}

func containersReadyFalse(c *fakeClock, since time.Duration) corev1.PodCondition {
	return corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: ago(c, since)}
}

func runningNotReady(c *fakeClock, startedAgo time.Duration) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Ready: false,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(c, startedAgo)}},
	}
}

func collectWith(t *testing.T, c *fakeClock, pods []corev1.Pod, events []corev1.Event, opts ...PodCollectorOption) []Signal {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	b := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Name}
		})
	for i := range pods {
		b = b.WithObjects(&pods[i])
	}
	for i := range events {
		b = b.WithObjects(&events[i])
	}
	cl := b.Build()
	all := append([]PodCollectorOption{withClock(c.now), WithEventReader(cl)}, opts...)
	p := NewPodCollector(cl, zap.New(zap.UseDevMode(true)), all...)
	sigs, err := p.Collect(context.Background())
	require.NoError(t, err)
	return sigs
}

func signalTypes(sigs []Signal) []SignalType {
	out := make([]SignalType, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, s.Type)
	}
	return out
}

func probeEvent(pod, msg string, count int32, at metav1.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: pod + "-" + msg[:4], Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod, Namespace: "shop", UID: k8stypes.UID("uid-" + pod)},
		Reason:         "Unhealthy",
		Message:        msg,
		Count:          count,
		LastTimestamp:  at,
	}
}

// ---- Readiness -------------------------------------------------------------

func TestReadiness_NotReadyPastThresholdIsAHealthSignal(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, 10*time.Minute), containersReadyFalse(c, 3*time.Minute))

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)

	require.Len(t, sigs, 1, "got %v", signalTypes(sigs))
	s := sigs[0]
	assert.Equal(t, SignalReadinessNotReady, s.Type)
	assert.Equal(t, CategoryHealth, s.Category)
	assert.Equal(t, SeverityWarning, s.Severity)
	assert.Equal(t, "shop", s.Resource.Namespace)
	assert.Equal(t, "main", s.Metadata["container"])
}

func TestReadiness_Boundaries(t *testing.T) {
	c := newClock()
	started := func(d time.Duration) corev1.ContainerStatus { return runningNotReady(c, d) }
	startupPending := started(10 * time.Minute)
	f := false
	startupPending.Started = &f
	ready := started(10 * time.Minute)
	ready.Ready = true
	terminating := runningPod("term", started(10*time.Minute), containersReadyFalse(c, 5*time.Minute))
	now := metav1.NewTime(c.t)
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"test/hold"}
	pending := runningPod("pend", started(10*time.Minute), containersReadyFalse(c, 5*time.Minute))
	pending.Status.Phase = corev1.PodPending

	tests := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{"not ready 1 minute: still within the threshold", runningPod("a", started(10*time.Minute), containersReadyFalse(c, time.Minute)), false},
		{"not ready exactly at the threshold", runningPod("b", started(10*time.Minute), containersReadyFalse(c, 2*time.Minute)), true},
		{"ready", runningPod("c", ready), false},
		{"startup probe has not passed yet", runningPod("d", startupPending, containersReadyFalse(c, 5*time.Minute)), false},
		{"container only just (re)started: the threshold counts from its start", runningPod("e", started(30*time.Second), containersReadyFalse(c, 10*time.Minute)), false},
		{"pod is terminating", terminating, false},
		{"pod is pending", pending, false},
		{"no condition recorded: falls back to the container start", runningPod("f", started(5*time.Minute)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigs := collectWith(t, c, []corev1.Pod{tt.pod}, nil)
			got := false
			for _, s := range sigs {
				got = got || s.Type == SignalReadinessNotReady
			}
			assert.Equal(t, tt.want, got, "signals: %v", signalTypes(sigs))
		})
	}
}

func TestReadiness_ThresholdIsConfigurable(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, 10*time.Minute), containersReadyFalse(c, 3*time.Minute))

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil, WithReadinessThreshold(5*time.Minute))
	assert.Empty(t, sigs, "3 minutes is under a 5 minute threshold")

	sigs = collectWith(t, c, []corev1.Pod{pod}, nil, WithReadinessThreshold(time.Minute))
	assert.Len(t, sigs, 1)
}

func TestReadiness_InitContainersAreNotReadinessSignals(t *testing.T) {
	c := newClock()
	pod := runningPod("init", corev1.ContainerStatus{})
	pod.Status.ContainerStatuses = nil
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{runningNotReady(c, 10*time.Minute)}
	pod.Status.Conditions = []corev1.PodCondition{containersReadyFalse(c, 10*time.Minute)}

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)
	assert.Empty(t, sigs)
}

func TestReadiness_KubeletUnhealthyEventsAreSupportingEvidence(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, 10*time.Minute), containersReadyFalse(c, 3*time.Minute))
	events := []corev1.Event{
		probeEvent("api-1", "Readiness probe failed: HTTP probe failed with statuscode: 503", 7, ago(c, 20*time.Second)),
		probeEvent("api-1", "Liveness probe failed: ignored here", 2, ago(c, 20*time.Second)),
		probeEvent("other", "Readiness probe failed: another pod", 9, ago(c, 20*time.Second)),
	}

	sigs := collectWith(t, c, []corev1.Pod{pod}, events)

	require.Len(t, sigs, 1)
	assert.Equal(t, "7", sigs[0].Metadata["readinessProbeEvents"], "only this pod's Readiness events count")
	assert.Contains(t, sigs[0].Message, "7 kubelet readiness probe failure events")
}

func TestReadiness_NoEventsStillRaisesTheSignal(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, 10*time.Minute), containersReadyFalse(c, 3*time.Minute))

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)

	require.Len(t, sigs, 1)
	assert.Equal(t, "0", sigs[0].Metadata["readinessProbeEvents"])
	assert.NotContains(t, sigs[0].Message, "kubelet readiness probe failure events")
}

// ---- Liveness restarts -----------------------------------------------------

func restartedRunning(c *fakeClock, restarts int32, reason string, exit int32, finishedAgo time.Duration) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Ready:        true,
		RestartCount: restarts,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(c, 5*time.Second)}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: reason, ExitCode: exit, FinishedAt: ago(c, finishedAgo), StartedAt: ago(c, finishedAgo+time.Minute),
		}},
	}
}

// livenessCycles runs one collection cycle per container status over one
// collector, a minute apart, as the operator does.
func livenessCycles(t *testing.T, c *fakeClock, opts []PodCollectorOption, cycles ...corev1.ContainerStatus) [][]Signal {
	t.Helper()
	return livenessCyclesWithEvents(t, c, opts, nil, cycles...)
}

// livenessCyclesWithEvents is livenessCycles with kubelet events in the cluster
// and the collector reading them.
func livenessCyclesWithEvents(
	t *testing.T, c *fakeClock, opts []PodCollectorOption, events []corev1.Event, cycles ...corev1.ContainerStatus,
) [][]Signal {
	t.Helper()
	return livenessCyclesFn(t, c, opts, events, len(cycles), func(i int) corev1.ContainerStatus { return cycles[i] })
}

// livenessCyclesFn builds each cycle's status when the cycle runs, so timestamps
// are relative to that cycle's clock.
func livenessCyclesFn(
	t *testing.T, c *fakeClock, opts []PodCollectorOption, events []corev1.Event, n int, status func(i int) corev1.ContainerStatus,
) [][]Signal {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	b := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Name}
		})
	for i := range events {
		b = b.WithObjects(&events[i])
	}
	cl := b.Build()
	all := append([]PodCollectorOption{withClock(c.now), WithEventReader(cl)}, opts...)
	p := NewPodCollector(cl, zap.New(zap.UseDevMode(true)), all...)
	out := make([][]Signal, 0, n)
	for i := range n {
		pod := runningPod("api-1", status(i))
		if i == 0 {
			require.NoError(t, cl.Create(context.Background(), &pod))
		} else {
			var cur corev1.Pod
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(&pod), &cur))
			cur.Status = pod.Status
			require.NoError(t, cl.Status().Update(context.Background(), &cur))
		}
		sigs, err := p.Collect(context.Background())
		require.NoError(t, err)
		out = append(out, sigs)
		c.advance(time.Minute)
	}
	return out
}

// hasLoop reports whether a container is reported as restarting repeatedly, as a
// liveness failure or as a restart loop of unknown cause.
func hasLoop(sigs []Signal) bool {
	return hasType(sigs, SignalLivenessRestarts) || hasType(sigs, SignalRestartLoop)
}

func hasType(sigs []Signal, t SignalType) bool {
	for _, s := range sigs {
		if s.Type == t {
			return true
		}
	}
	return false
}

func TestLiveness_RestartDeltaWithinWindowIsAHealthSignal(t *testing.T) {
	c := newClock()
	// Restarts: 3 at first sight (a baseline), then 5 a minute later: +2 in the window.
	cycles := livenessCycles(t, c, nil,
		restartedRunning(c, 3, "Error", 137, 3*time.Hour),
		restartedRunning(c, 5, "Error", 137, 10*time.Second),
	)

	assert.False(t, hasLoop(cycles[0]), "first sight only seeds the history")
	require.True(t, hasLoop(cycles[1]), "got %v", signalTypes(cycles[1]))
	var s Signal
	for _, x := range cycles[1] {
		if x.Type == SignalRestartLoop {
			s = x
		}
	}
	require.Equal(t, SignalRestartLoop, s.Type, "no kubelet event says a probe did it, so it is a restart loop")
	assert.Equal(t, CategoryHealth, s.Category)
	assert.Equal(t, SeverityWarning, s.Severity)
	assert.Equal(t, "2", s.Metadata["restartsInWindow"])
}

func TestLiveness_OneRestartIsNotASignal(t *testing.T) {
	c := newClock()
	cycles := livenessCycles(t, c, nil,
		restartedRunning(c, 3, "Error", 137, 3*time.Hour),
		restartedRunning(c, 4, "Error", 137, 10*time.Second),
	)
	assert.False(t, hasLoop(cycles[1]))
}

func TestLiveness_RestartsOutsideTheWindowAreNotASignal(t *testing.T) {
	c := newClock()
	cs := func(n int32) corev1.ContainerStatus { return restartedRunning(c, n, "Error", 137, time.Minute) }
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	p := NewPodCollector(cl, zap.New(zap.UseDevMode(true)), withClock(c.now))
	pod := runningPod("api-1", cs(3))
	require.NoError(t, cl.Create(context.Background(), &pod))
	_, err := p.Collect(context.Background())
	require.NoError(t, err)

	// 15 minutes later (window is 10), the count is up by two.
	c.advance(15 * time.Minute)
	var cur corev1.Pod
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(&pod), &cur))
	cur.Status = runningPod("api-1", cs(5)).Status
	require.NoError(t, cl.Status().Update(context.Background(), &cur))
	sigs, err := p.Collect(context.Background())
	require.NoError(t, err)
	assert.False(t, hasLoop(sigs), "+2 restarts over 15 minutes is not 2 in 10")
}

func TestLiveness_LastTerminationMustBeAnErrorOrASigkill(t *testing.T) {
	// Without a kubelet event that blames a probe, only an Error or a SIGKILL that
	// is not an OOM kill counts, and it is reported as a restart loop.
	tests := []struct {
		name   string
		reason string
		exit   int32
		want   bool
	}{
		{"reason Error", "Error", 1, true},
		{"exit 137 with no reason", "", 137, true},
		{"Error and 137", "Error", 137, true},
		{"OOMKilled is the resource signal's", "OOMKilled", 137, false},
		{"clean exit", "Completed", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClock()
			cycles := livenessCycles(t, c, nil,
				restartedRunning(c, 3, tt.reason, tt.exit, 3*time.Hour),
				restartedRunning(c, 5, tt.reason, tt.exit, 10*time.Second),
			)
			assert.Equal(t, tt.want, hasType(cycles[1], SignalRestartLoop), "got %v", signalTypes(cycles[1]))
			assert.False(t, hasType(cycles[1], SignalLivenessRestarts), "no event blames a probe: never a liveness failure")
		})
	}
}

// F-7: after an operator restart the status holds only a lifetime count. It is
// not evidence of anything inside the window, so first sight seeds the history
// and the next cycle's delta decides.
func TestLiveness_FirstSightOnlySeedsTheHistory(t *testing.T) {
	c := newClock()
	cycles := livenessCycles(t, c, nil,
		restartedRunning(c, 6, "Error", 137, 30*time.Second),
		restartedRunning(c, 8, "Error", 137, 10*time.Second),
	)
	assert.False(t, hasLoop(cycles[0]), "a lifetime count of 6 with a fresh kill is not 6 restarts in 10 minutes")
	require.True(t, hasLoop(cycles[1]), "the delta over the next cycle is: %v", signalTypes(cycles[1]))
	for _, s := range cycles[1] {
		if s.Type == SignalRestartLoop {
			assert.Equal(t, "2", s.Metadata["restartsInWindow"], "what was observed, not the lifetime count")
			assert.NotContains(t, s.Message, "8 times")
		}
	}
}

func TestLiveness_ThresholdAndWindowAreConfigurable(t *testing.T) {
	c := newClock()
	cycles := livenessCycles(t, c, []PodCollectorOption{WithLivenessRestarts(3, 10*time.Minute)},
		restartedRunning(c, 3, "Error", 137, 3*time.Hour),
		restartedRunning(c, 5, "Error", 137, 10*time.Second),
	)
	assert.False(t, hasLoop(cycles[1]), "+2 is under a threshold of 3")
}

func TestRestartTracker_ForgetsContainersThatAreGone(t *testing.T) {
	c := newClock()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	p := NewPodCollector(cl, zap.New(zap.UseDevMode(true)), withClock(c.now))
	pod := runningPod("api-1", restartedRunning(c, 3, "Error", 137, 3*time.Hour))
	require.NoError(t, cl.Create(context.Background(), &pod))
	_, err := p.Collect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, p.restarts.size())

	require.NoError(t, cl.Delete(context.Background(), &pod))
	_, err = p.Collect(context.Background())
	require.NoError(t, err)
	assert.Zero(t, p.restarts.size(), "a deleted pod's history must not accumulate")
}

// ---- Precedence: one container, one signal family --------------------------

func TestPrecedence_CrashLoopBackOffOwnsTheContainer(t *testing.T) {
	c := newClock()
	cs := restartedRunning(c, 6, "Error", 1, 10*time.Second)
	cs.Ready = false
	cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	pod := runningPod("api-1", cs, containersReadyFalse(c, 20*time.Minute))

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)

	assert.True(t, hasType(sigs, SignalCrashLoopBackOff))
	assert.False(t, hasLoop(sigs), "CrashLoopBackOff already reports this container")
	assert.False(t, hasType(sigs, SignalReadinessNotReady))
}

func TestPrecedence_CrashLoopBackOffOwnsTheContainerAcrossCycles(t *testing.T) {
	c := newClock()
	waiting := func(n int32) corev1.ContainerStatus {
		cs := restartedRunning(c, n, "Error", 1, 10*time.Second)
		cs.Ready = false
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
		return cs
	}
	cycles := livenessCycles(t, c, nil, waiting(3), waiting(6))
	assert.False(t, hasLoop(cycles[1]))
}

func TestPrecedence_LiveOOMKilledOwnsTheContainer(t *testing.T) {
	c := newClock()
	cycles := livenessCycles(t, c, nil,
		restartedRunning(c, 3, "OOMKilled", 137, 3*time.Hour),
		restartedRunning(c, 5, "OOMKilled", 137, 10*time.Second),
	)
	assert.True(t, hasType(cycles[1], SignalOOMKilled))
	assert.False(t, hasLoop(cycles[1]))
}

func TestPrecedence_LivenessOwnsAContainerThatIsAlsoNotReady(t *testing.T) {
	c := newClock()
	mk := func(n int32) corev1.ContainerStatus {
		cs := restartedRunning(c, n, "Error", 137, 10*time.Second)
		cs.Ready = false
		cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(c, 10*time.Minute)}}
		return cs
	}
	cycles := livenessCycles(t, c, nil, mk(3), mk(5))
	assert.True(t, hasLoop(cycles[1]))
	assert.False(t, hasType(cycles[1], SignalReadinessNotReady), "a restart loop explains not-ready: one finding, not two")
}

// ---- Stale terminated state (D0.8 finding 3) -------------------------------

func staleOOM(c *fakeClock, runningFor time.Duration, ready bool) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Ready:        ready,
		RestartCount: 1,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(c, runningFor)}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "OOMKilled", ExitCode: 137, FinishedAt: ago(c, runningFor+time.Second), StartedAt: ago(c, runningFor+time.Hour),
		}},
	}
}

func TestStaleTermination_OOMKilledBeforeTheCurrentStartIsHistory(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", staleOOM(c, 15*time.Minute, true))

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)

	assert.Empty(t, sigs, "a container Running and Ready for 15 minutes since its OOM kill raises nothing: %v", signalTypes(sigs))
}

func TestStaleTermination_RecentRestartIsStillLive(t *testing.T) {
	c := newClock()
	// 20 seconds after the restart. A health check every minute must still see the
	// kill, or an app with no readiness probe (Ready at once) would never be detected.
	pod := runningPod("api-1", staleOOM(c, 20*time.Second, true))

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)

	assert.True(t, hasType(sigs, SignalOOMKilled), "got %v", signalTypes(sigs))
}

func TestStaleTermination_NotReadyIsNeverStale(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", staleOOM(c, 15*time.Minute, false))

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)

	assert.True(t, hasType(sigs, SignalOOMKilled))
}

func TestStaleTermination_CurrentTerminatedStateIsNeverStale(t *testing.T) {
	c := newClock()
	cs := corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: "OOMKilled", FinishedAt: ago(c, time.Hour),
	}}}
	sigs := collectWith(t, c, []corev1.Pod{runningPod("api-1", cs)}, nil)

	assert.True(t, hasType(sigs, SignalOOMKilled), "a container that is terminated right now is the live fact")
}

func TestStaleTermination_GraceIsConfigurable(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", staleOOM(c, 5*time.Minute, true))

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil, WithTerminatedStaleAfter(10*time.Minute))
	assert.True(t, hasType(sigs, SignalOOMKilled), "5 minutes is inside a 10 minute grace")

	sigs = collectWith(t, c, []corev1.Pod{pod}, nil, WithTerminatedStaleAfter(time.Minute))
	assert.False(t, hasType(sigs, SignalOOMKilled))
}

func TestStaleTermination_StaleKillIsNotALivenessSignalEither(t *testing.T) {
	c := newClock()
	mk := func(n int32) corev1.ContainerStatus {
		cs := restartedRunning(c, n, "Error", 137, 15*time.Minute)
		cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(c, 14*time.Minute)}}
		return cs
	}
	cycles := livenessCycles(t, c, nil, mk(3), mk(5))
	assert.False(t, hasLoop(cycles[1]),
		"restarts that ended 14 minutes ago with the container Ready since are not a live loop")
}

// ---- Event evidence details ------------------------------------------------

func TestProbeEvents_Matching(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, time.Minute))
	cs := pod.Status.ContainerStatuses[0]
	since := c.t.Add(-time.Minute)
	base := func() corev1.Event {
		return probeEvent("api-1", "Readiness probe failed: x", 1, metav1.NewTime(c.t))
	}
	tests := []struct {
		name   string
		mutate func(*corev1.Event)
		want   bool
	}{
		{"plain match", func(*corev1.Event) {}, true},
		{"wrong reason", func(e *corev1.Event) { e.Reason = "Killing" }, false},
		{"wrong message", func(e *corev1.Event) { e.Message = "Liveness probe failed: x" }, false},
		{"not a pod", func(e *corev1.Event) { e.InvolvedObject.Kind = "Node" }, false},
		{"a previous incarnation of the pod (UID differs)", func(e *corev1.Event) { e.InvolvedObject.UID = "someone-else" }, false},
		{"this container, by field path", func(e *corev1.Event) { e.InvolvedObject.FieldPath = "spec.containers{main}" }, true},
		{"another container, by field path", func(e *corev1.Event) { e.InvolvedObject.FieldPath = "spec.containers{sidecar}" }, false},
		{"older than the evidence window", func(e *corev1.Event) { e.LastTimestamp = metav1.NewTime(since.Add(-time.Second)) }, false},
		{"timed by the events API series", func(e *corev1.Event) {
			e.LastTimestamp = metav1.Time{}
			e.Series = &corev1.EventSeries{Count: 5, LastObservedTime: metav1.NewMicroTime(c.t)}
		}, true},
		{"timed by eventTime", func(e *corev1.Event) {
			e.LastTimestamp = metav1.Time{}
			e.EventTime = metav1.NewMicroTime(c.t)
		}, true},
		{"timed by creation when nothing else is set", func(e *corev1.Event) {
			e.LastTimestamp = metav1.Time{}
			e.CreationTimestamp = metav1.NewTime(c.t)
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := base()
			tt.mutate(&ev)
			assert.Equal(t, tt.want, probeEventMatches(&ev, pod, cs, readinessEventPrefix, since))
		})
	}
}

func TestEventCount(t *testing.T) {
	assert.EqualValues(t, 5, eventCount(&corev1.Event{Count: 2, Series: &corev1.EventSeries{Count: 5}}), "a series wins")
	assert.EqualValues(t, 2, eventCount(&corev1.Event{Count: 2}))
	assert.EqualValues(t, 1, eventCount(&corev1.Event{}), "an event with no count is one event")
}

type failingEventReader struct{ client.Reader }

func (failingEventReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return assert.AnError
}

func TestProbeEvents_ReadFailureLeavesTheSignalStanding(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, 10*time.Minute), containersReadyFalse(c, 3*time.Minute))

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil, WithEventReader(failingEventReader{}))

	require.Len(t, sigs, 1, "evidence is supporting only: no events must never hide the signal")
	assert.Equal(t, evidenceUnavailable, sigs[0].Metadata["readinessProbeEvents"], "an unreadable count is marked, not zero")
}

func TestProbeEvents_NoReaderMarksTheEvidenceUnavailable(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, 10*time.Minute), containersReadyFalse(c, 3*time.Minute))
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&pod).Build()
	p := NewPodCollector(cl, zap.New(zap.UseDevMode(true)), withClock(c.now))

	sigs, err := p.Collect(context.Background())
	require.NoError(t, err)
	require.Len(t, sigs, 1)
	assert.Equal(t, evidenceUnavailable, sigs[0].Metadata["readinessProbeEvents"])
}

func TestKilledByProbeOrError(t *testing.T) {
	tests := []struct {
		reason string
		exit   int32
		want   bool
	}{
		{"OOMKilled", 137, false},
		{"Error", 2, true},
		{"", 137, true},
		{"Completed", 0, false},
		{"ContainerCannotRun", 128, false},
	}
	for _, tt := range tests {
		got := killedByProbeOrError(&corev1.ContainerStateTerminated{Reason: tt.reason, ExitCode: tt.exit})
		assert.Equal(t, tt.want, got, "%s/%d", tt.reason, tt.exit)
	}
}

func TestStaleTermination_MissingTimesAreNeverStale(t *testing.T) {
	c := newClock()
	p := NewPodCollector(nil, zap.New(zap.UseDevMode(true)))
	running := corev1.ContainerStatus{Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(c, time.Hour)}}}
	assert.False(t, p.staleTermination(running, &corev1.ContainerStateTerminated{}, c.t), "no finishedAt: cannot show it is old")

	noStart := corev1.ContainerStatus{Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	assert.False(t, p.staleTermination(noStart, &corev1.ContainerStateTerminated{FinishedAt: ago(c, time.Hour)}, c.t), "no startedAt")

	after := corev1.ContainerStatus{Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(c, time.Hour)}}}
	assert.False(t, p.staleTermination(after, &corev1.ContainerStateTerminated{FinishedAt: ago(c, time.Minute)}, c.t),
		"a termination that finished after the current start is not history")
}

func TestOptions_NonPositiveValuesKeepTheDefaults(t *testing.T) {
	p := NewPodCollector(nil, zap.New(zap.UseDevMode(true)),
		WithReadinessThreshold(0), WithLivenessRestarts(0, 0), WithTerminatedStaleAfter(-time.Second))
	assert.Equal(t, DefaultReadinessThreshold, p.readinessThreshold)
	assert.Equal(t, DefaultLivenessRestarts, p.livenessRestarts)
	assert.Equal(t, DefaultLivenessWindow, p.livenessWindow)
	assert.Equal(t, DefaultTerminatedStaleAfter, p.staleAfter)
}

func TestRestartTracker_CounterResetStartsOver(t *testing.T) {
	c := newClock()
	tr := newRestartTracker(10 * time.Minute)
	_, h := tr.observe("k", c.t, 5)
	assert.Equal(t, historyFirstSight, h)
	c.advance(time.Minute)
	delta, h := tr.observe("k", c.t, 7)
	assert.Equal(t, historyInWindow, h)
	assert.EqualValues(t, 2, delta)
	c.advance(time.Minute)
	delta, h = tr.observe("k", c.t, 1) // a different container under the same key
	assert.Equal(t, historyNone, h, "a count that went down is not a delta")
	assert.Zero(t, delta)
}
