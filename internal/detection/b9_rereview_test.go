package detection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// B9 PR #60 re-review (head c084586), judgement call (e): at most 25 pods get a
// kubelet-event read per cycle. For readiness the events are only supporting
// evidence, so running out costs nothing. For a liveness kill that exits 0 they are
// the ONLY evidence, so a pod past the budget is silently not reported.

func liveKillPod(c *fakeClock, name string) (corev1.Pod, []corev1.Event) {
	pod := runningPod(name, restartedRunning(c, 6, "Completed", 0, 30*time.Second))
	ev := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-killing", Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Name: name, Namespace: "shop", UID: k8stypes.UID("uid-" + name), FieldPath: "spec.containers{main}",
		},
		Reason:         "Killing",
		Message:        "Container main failed liveness probe, will be restarted",
		Count:          3,
		FirstTimestamp: ago(c, 20*time.Second),
		LastTimestamp:  ago(c, 20*time.Second),
	}
	return pod, []corev1.Event{ev}
}

// twoCycles collects twice, a minute apart, with the pods' restart counts raised
// by two in between so the tracker sees a loop (first sight only seeds it).
func twoCycles(t *testing.T, c *fakeClock, pods []corev1.Pod, evs []corev1.Event, live string, secondStatus func() corev1.ContainerStatus) []Signal {
	t.Helper()
	base := fakeWithEventIndex(t)
	for i := range pods {
		require.NoError(t, base.Create(ctxBG(), &pods[i]))
	}
	for i := range evs {
		require.NoError(t, base.Create(ctxBG(), &evs[i]))
	}
	p := NewPodCollector(base, testLogger(), withClock(c.now), WithEventReader(base))
	_, err := p.Collect(ctxBG())
	require.NoError(t, err)
	c.advance(time.Minute)

	var cur corev1.Pod
	require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: "shop", Name: live}, &cur))
	cs := secondStatus()
	cs.Name = "main"
	cur.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	require.NoError(t, base.Status().Update(context.Background(), &cur))
	sigs, err := p.Collect(ctxBG())
	require.NoError(t, err)
	var out []Signal
	for _, s := range sigs {
		if s.Resource.Name == live {
			out = append(out, s)
		}
	}
	return out
}

// Control: alone, the exit-0 liveness kill is reported from its events.
func TestRereview_ExitZeroLivenessKillIsReportedWhenTheBudgetIsFree(t *testing.T) {
	c := newClock()
	pod, evs := liveKillPod(c, "z-live")
	sigs := twoCycles(t, c, []corev1.Pod{pod}, evs, "z-live", func() corev1.ContainerStatus {
		return restartedRunning(c, 8, "Completed", 0, 30*time.Second)
	})
	assert.True(t, hasType(sigs, SignalLivenessRestarts), "control: got %v", signalTypes(sigs))
}

// A node loss leaves 25 or more pods not Ready (each spends one event read for
// supporting evidence it does not need); a pod sorted after them whose liveness
// kill exits 0 has no other evidence, so no liveness or restart-loop signal is
// raised for it. (Only the legacy ContainerHighRestarts signal, which produces no
// diagnosis, is.)
func TestRereview_ExitZeroLivenessKillPastTheEventBudgetIsMissed(t *testing.T) {
	c := newClock()
	pods := manyNotReadyPods(c, maxEventReadsPerCycle) // "api-000".."api-024" sort before "z-live"
	live, evs := liveKillPod(c, "z-live")
	pods = append(pods, live)
	sigs := twoCycles(t, c, pods, evs, "z-live", func() corev1.ContainerStatus {
		return restartedRunning(c, 8, "Completed", 0, 30*time.Second)
	})
	assert.True(t, hasLoop(sigs),
		"z-live was killed for failing its liveness probe (3 kubelet kill events exist) but %d readiness-only pods used the %d event reads first; signals for it: %v",
		maxEventReadsPerCycle, maxEventReadsPerCycle, signalTypes(sigs))
}

// Characterization: with an Error exit the same pod still raises a signal, but as
// the weaker RestartLoop whose rule says "no kubelet event blames a probe", which
// is false: an event does, it was just not read.
func TestRereview_ErrorExitPastTheBudgetIsCalledARestartLoopWhileTheKubeletSaidLiveness_Characterization(t *testing.T) {
	c := newClock()
	pods := manyNotReadyPods(c, maxEventReadsPerCycle)
	live, evs := liveKillPod(c, "z-live")
	pods = append(pods, live)
	sigs := twoCycles(t, c, pods, evs, "z-live", func() corev1.ContainerStatus {
		return restartedRunning(c, 8, "Error", 137, 30*time.Second)
	})
	s, ok := loopSignal(sigs)
	require.True(t, ok)
	assert.Equal(t, SignalRestartLoop, s.Type)
	assert.Equal(t, evidenceUnavailable, s.Metadata["livenessKillEvents"])
}

// F-5 variant. A container with a slow startupProbe: the kubelet runs no readiness
// probe until the startup probe has passed, and initialDelaySeconds is counted from
// the container start. When the startup probe passes at minute 5, the container is
// not Ready for up to one probe period, but its readiness clock (start + initial
// delay) is already 5 minutes old, so ReadinessNotReady is raised for a container
// that has had no readiness probe yet. The pod status does not say when the startup
// probe passed, so this is a limit of the evidence; recorded as PLAUSIBLE, low.
func TestRereview_F5_ContainerThatJustPassedASlowStartupProbeIsReportedNotReady_Characterization(t *testing.T) {
	c := newClock()
	started := true
	cs := runningNotReady(c, 5*time.Minute)
	cs.Started = &started
	pod := runningPod("api-1", cs, containersReadyFalse(c, 5*time.Minute))
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{FailureThreshold: 60, PeriodSeconds: 5}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{PeriodSeconds: 10}

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)

	// Not a bug the code can fix: "passed a moment ago" and "passed four minutes ago
	// and readiness has failed since" look the same in the pod status.
	assert.True(t, hasType(sigs, SignalReadinessNotReady), "got %v", signalTypes(sigs))
}
