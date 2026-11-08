package detection

// Adversarial review tests for PR #60 (B9). *_Bug tests are expected to FAIL on
// head 09894e7; *_Characterization tests PASS and pin behaviour the review flags.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// R-5. A readiness probe with initialDelaySeconds longer than the threshold has
// not even started probing yet; the container is not "failing readiness". The
// collector reads only cs.Started (startupProbe), never the probe's own delay, so
// it reports the container as not Ready "for 3m" before the first probe runs.
func TestReview_ReadinessInitialDelayIsNotAFailure_Bug(t *testing.T) {
	c := newClock()
	pod := runningPod("api-1", runningNotReady(c, 3*time.Minute), containersReadyFalse(c, 3*time.Minute))
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		InitialDelaySeconds: 300,
		ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready"}},
	}

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)

	assert.False(t, hasType(sigs, SignalReadinessNotReady),
		"the readiness probe's initialDelaySeconds (300s) has not elapsed; nothing has failed yet: %v", signalTypes(sigs))
}

// R-6. A pod held NotReady by a readiness gate (e.g. an ALB / NEG target not yet
// registered) has Ready containers: cs.Ready is true, so nothing is raised. This
// pins the exclusion the review brief asked about (passes).
func TestReview_ReadinessGateHeldPodIsExcluded_Characterization(t *testing.T) {
	c := newClock()
	cs := corev1.ContainerStatus{
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(c, 30*time.Minute)}},
	}
	pod := runningPod("api-1", cs, corev1.PodCondition{
		Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: ago(c, 20*time.Minute),
	})
	pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: "target-health.elbv2.k8s.aws/x"}}

	assert.Empty(t, collectWith(t, c, []corev1.Pod{pod}, nil))
}

// R-7. First sight after an operator restart: the lifetime RestartCount stands in
// for "restarts in the window". One kill 40s ago plus one a week ago reads as
// "restarted 2 times in 10m0s". Passes; documents the false claim in the message.
func TestReview_FirstSightUsesLifetimeCount_Characterization(t *testing.T) {
	c := newClock()
	cs := restartedRunning(c, 2, "Error", 137, 40*time.Second) // second restart ever; first was last week
	sigs := collectWith(t, c, []corev1.Pod{runningPod("api-1", cs)}, nil)

	require.True(t, hasType(sigs, SignalLivenessRestarts))
	for _, s := range sigs {
		if s.Type == SignalLivenessRestarts {
			assert.Equal(t, "2", s.Metadata["restartsInWindow"], "only ONE of those restarts is inside the window")
			assert.Contains(t, s.Message, "restarted 2 times in 10m0s")
		}
	}
}

// R-8. Staleness trusts the node's clock against the operator's. A node whose
// clock is 5 minutes behind makes a container that started 10s ago look like it
// has run for 5m10s, so a fresh OOM kill of an app with no readiness probe is
// declared history at once and never reported. Passes; documents the assumption.
func TestReview_StalenessAssumesNodeAndOperatorClocksAgree_Characterization(t *testing.T) {
	c := newClock()
	skew := 5 * time.Minute
	cs := staleOOM(c, 10*time.Second+skew, true) // apparent ages as the skewed kubelet wrote them
	sigs := collectWith(t, c, []corev1.Pod{runningPod("api-1", cs)}, nil)

	assert.False(t, hasType(sigs, SignalOOMKilled), "a 10s-old OOM kill is erased by a 5m clock skew")
}

// R-9. OOM, restart, Ready for the grace, OOM again: the second kill must be live
// again and the first must not re-fire. (Brief item 4.) Passes: the design holds.
func TestReview_RepeatedOOMAfterTheGraceIsDetectedAgain_Characterization(t *testing.T) {
	c := newClock()
	scheme := runtimeSchemeForTest(t)
	cl := newFakeForTest(scheme)
	p := NewPodCollector(cl, testLogger(), withClock(c.now))

	apply := func(cs corev1.ContainerStatus) []Signal {
		pod := runningPod("api-1", cs)
		var cur corev1.Pod
		if err := cl.Get(ctxBG(), keyOf(&pod), &cur); err != nil {
			require.NoError(t, cl.Create(ctxBG(), &pod))
		} else {
			cur.Status = pod.Status
			require.NoError(t, cl.Status().Update(ctxBG(), &cur))
		}
		sigs, err := p.Collect(ctxBG())
		require.NoError(t, err)
		return sigs
	}

	// OOM #1 five seconds before the restart.
	assert.True(t, hasType(apply(staleOOM(c, 5*time.Second, true)), SignalOOMKilled), "fresh kill is live")
	c.advance(3 * time.Minute)
	// 3m later, same status aged: stale.
	assert.False(t, hasType(apply(staleOOM(c, 3*time.Minute+5*time.Second, true)), SignalOOMKilled), "old kill is history")
	// OOM #2 now: the pod reports a new lastState and a new start.
	c.advance(time.Minute)
	assert.True(t, hasType(apply(staleOOM(c, 5*time.Second, true)), SignalOOMKilled), "second kill is live again")
}

// R-10. Readiness/liveness signals ignore a PodDisruptionBudget-style planned
// restart pattern: two Error/137 exits in ten minutes during a node drain are
// evictions (pod deleted, new UID, tracker key changes) so they never accumulate.
// Pins that a replacement pod does not inherit the old pod's history (passes).
func TestReview_ReplacementPodDoesNotInheritRestartHistory_Characterization(t *testing.T) {
	tr := newRestartTracker(10 * time.Minute)
	now := time.Now()
	tr.observe("shop/api-1/uid-a/main", now, 5)
	delta, history := tr.observe("shop/api-1/uid-b/main", now.Add(time.Minute), 1)
	assert.Equal(t, restartHistory(historyFirstSight), history)
	assert.Zero(t, delta)
}

// R-11. The tracker keeps EVERY sample inside the window, not just the points
// where the count changed: memory is containers x window/interval for the whole
// cluster, user-tunable through --liveness-restart-window. Passes; documents it.
func TestReview_TrackerKeepsOneSamplePerObservation_Characterization(t *testing.T) {
	tr := newRestartTracker(24 * time.Hour)
	now := time.Now()
	for i := range 1440 { // a day at 60s
		tr.observe("shop/api-1/uid/main", now.Add(time.Duration(i)*time.Minute), 0)
	}
	tr.mu.Lock()
	n := len(tr.samples["shop/api-1/uid/main"])
	tr.mu.Unlock()
	assert.Equal(t, 1440, n, "one sample per cycle, although the count never moved")
	_ = resource.Quantity{}
	_ = metav1.Time{}
}

// R-12. --liveness-restart-window shorter than the health-check interval makes
// every sample fall out of the window before the next cycle, so history is always
// "none" and the liveness signal can only ever fire at first sight. Nothing
// validates the flag against --health-check-interval. Expected to FAIL: a
// container restarted 3 -> 5 -> 7 times over three cycles raises nothing.
func TestReview_WindowShorterThanTheCycleSilentlyDisablesLiveness_Bug(t *testing.T) {
	c := newClock()
	opts := []PodCollectorOption{WithLivenessRestarts(2, 30*time.Second)}
	cycles := livenessCycles(t, c, opts,
		restartedRunning(c, 3, "Error", 137, 10*time.Second),
		restartedRunning(c, 5, "Error", 137, 10*time.Second),
		restartedRunning(c, 7, "Error", 137, 10*time.Second),
	)
	assert.True(t, hasType(cycles[2], SignalLivenessRestarts),
		"a container restarted twice a minute, every minute, for three minutes must raise the signal (cycle 3 saw: %v)",
		signalTypes(cycles[2]))
}

// R-14. A liveness kill is SIGTERM first. An app that traps SIGTERM and exits 0
// leaves reason "Completed", exit 0: killedByProbeOrError rejects it, so a
// liveness-probe fault against a gracefully-exiting app is still invisible (the
// D0.8 hole B9 exists to close). Passes; documents the recall gap.
func TestReview_LivenessKillThatExitsZeroOnSigtermIsMissed_Characterization(t *testing.T) {
	c := newClock()
	cycles := livenessCycles(t, c, nil,
		restartedRunning(c, 3, "Completed", 0, 10*time.Second),
		restartedRunning(c, 5, "Completed", 0, 10*time.Second),
	)
	assert.False(t, hasType(cycles[1], SignalLivenessRestarts), "5 restarts in 2 minutes, exit 0: not reported")
}
