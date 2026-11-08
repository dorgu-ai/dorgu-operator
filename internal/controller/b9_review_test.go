package controller

// Adversarial review tests for PR #60 (B9). Tests named TestReview_*_Bug are
// expected to FAIL on head 09894e7; *_Characterization tests PASS and document
// behaviour the review flags.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

func limitOneClusterPersona() *dorguv1.ClusterPersona {
	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "dorgu-cluster"},
		Spec: dorguv1.ClusterPersonaSpec{
			Name: "dorgu-cluster",
			Policies: &dorguv1.ClusterPolicies{
				SelfHealing: &dorguv1.SelfHealingPolicy{MaxRemediationsPerHour: 1},
			},
		},
	}
}

// R-1. B6 decided: an outstanding proposal for the same fault is a DEFERRAL (no
// ProposalRefused, out of Q6). B9 taught dedup about lineage but not the
// rate-limit stage (guardrails.ownActiveProposal compares exact names, and the
// guardrails stage runs BEFORE dedup). With a limit of 1 the recurrence's own
// outstanding proposal is counted as "another remediation", so the recurrence is
// REFUSED at Guardrails/rate-limit every cycle instead of deferred.
func TestReview_RecurrenceWithOutstandingProposalIsDeferredNotRefusedByRateLimit_Bug(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject(), limitOneClusterPersona()))

	e.cycle()
	require.Len(t, e.actions(), 1, "first incident gets its proposal")
	first := e.onlyOpen()
	e.resolve(first.Name)  // proposal stays Pending (nobody answered)
	stampCreationNow(t, e) // the fake API server does not set creationTimestamp; a real one does

	e.cycle()

	recurrence := e.onlyOpen()
	require.Len(t, e.actions(), 1, "outstanding proposal covers the fault: no second proposal")
	for _, cond := range recurrence.Status.Conditions {
		if cond.Type == dorguv1.ConditionProposalRefused {
			assert.NotEqual(t, metav1.ConditionTrue, cond.Status,
				"an outstanding proposal is a deferral, not a refusal; got ProposalRefused=%s reason=%s msg=%q",
				cond.Status, cond.Reason, cond.Message)
		}
	}
}

// stampCreationNow recreates every RemediationAction with creationTimestamp=now,
// which the real API server always sets and the fake client does not, so the
// hourly rate limit can see it.
func stampCreationNow(t *testing.T, e *recurrenceEnv) {
	t.Helper()
	for _, ra := range e.actions() {
		status := ra.Status
		fresh := ra.DeepCopy()
		require.NoError(t, e.c.Delete(context.Background(), &ra))
		fresh.ResourceVersion = ""
		fresh.CreationTimestamp = metav1.NewTime(time.Now())
		require.NoError(t, e.c.Create(context.Background(), fresh))
		fresh.Status = status
		require.NoError(t, e.c.Status().Update(context.Background(), fresh))
	}
}

// ---- R-2: one fault, three incidents as the signal changes shape ------------

func notReadyPodStatus(now time.Time, startedAgo time.Duration) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{
			Type: corev1.ContainersReady, Status: corev1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(now.Add(-startedAgo)),
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "main",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-startedAgo))}},
		}},
	}
}

func livenessPodStatus(now time.Time) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "main", RestartCount: 3,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-20 * time.Second))}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Error", ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-25 * time.Second)),
			}},
		}},
	}
}

func crashLoopPodStatus(now time.Time) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "main", RestartCount: 6,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Error", ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-10 * time.Second)),
			}},
		}},
	}
}

// R-2. Precedence is decided per cycle, but a liveness fault walks through three
// shapes as it worsens: not Ready for a while, then restarts, then
// CrashLoopBackOff. Incident identity is (persona, signal), so every shape opens
// its own incident, its own diagnosis stream and its own proposal while the
// earlier ones sit open waiting out the 6 minute recovery window. The PR's
// "no double fire" claim holds within one cycle only.
func TestReview_OneProbeFaultAcrossCyclesIsOneOpenIncident_Bug(t *testing.T) {
	now := time.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "shop"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		Status:     notReadyPodStatus(now, 5*time.Minute),
	}
	c := noiseClient(t, probePersona("shop"), pod)
	g := gate.New(c, noiseLogger())
	stub := &stubLLMClient{}
	r := aiReconciler(t, c, g, stub)

	step := func(status corev1.PodStatus) {
		var cur corev1.Pod
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pod), &cur))
		cur.Status = status
		require.NoError(t, c.Status().Update(context.Background(), &cur))
		r.reconcile(context.Background())
	}

	step(notReadyPodStatus(now, 5*time.Minute)) // cycle 1: ReadinessNotReady
	step(livenessPodStatus(now))                // cycle 2: kubelet starts killing it: LivenessRestarts
	step(crashLoopPodStatus(now))               // cycle 3: back-off: CrashLoopBackOff

	var open []dorguv1.IncidentMemory
	for _, im := range listIncidents(t, c) {
		if im.Status.Phase != PhaseResolved {
			open = append(open, im)
		}
	}
	var actions dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &actions))
	t.Logf("open incidents: %s; RemediationActions: %d; model calls: %d", incidentSignals(open), len(actions.Items), stub.calls)
	assert.Len(t, open, 1, "one container, one fault: expected one open incident, got %s", incidentSignals(open))
}

// ---- R-3: flapping fault, no cap --------------------------------------------

// R-3. Characterization (passes). A fault that resolves and returns N times
// leaves N+1 IncidentMemories, N+1 model-diagnosed incidents and an unbounded
// lineage. There is no cap, no Flapping fold and nothing prunes Resolved objects.
func TestReview_FlappingFaultIsUncapped_Characterization(t *testing.T) {
	const flaps = 40
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.cycle()
	for range flaps {
		e.resolve(e.onlyOpen().Name)
		e.cycle()
	}
	all := e.incidents()
	assert.Len(t, all, flaps+1, "no cap: every recurrence is a new object")
	last := e.onlyOpen()
	assert.Equal(t, gate.IncidentLineage(all[0].Name), gate.IncidentLineage(last.Name))
	assert.Equal(t, flaps, gate.IncidentGeneration(last.Name))
	t.Logf("after %d flaps: %d IncidentMemories, last generation r%d", flaps, len(all), gate.IncidentGeneration(last.Name))
}

var _ = detection.SignalOOMKilled
