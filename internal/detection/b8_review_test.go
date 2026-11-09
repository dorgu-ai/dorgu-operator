package detection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// B8 review (pr61) F-4 follow-up. restartsInYoungPod turns the pod's lifetime restart
// count into the delta only while the tracker has no history. The same cycle seeds
// the tracker with that very count, so on the NEXT cycle the delta is measured from
// the seed: count 2 -> 2 is delta 0, and 2 -> 3 is delta 1. The RestartLoop signal of
// a container that is still crash looping (state Terminated between runs, not yet
// Waiting/CrashLoopBackOff) therefore fires on the first sighting and goes away on the
// next cycle, then returns only after two further restarts have been observed.
//
// A signal that disappears while the fault continues lets the incident look settled
// and (see QA re-run N-1) churns the advisory RA.

func youngPodAcrossCycles(t *testing.T, counts []int32, step time.Duration) [][]Signal {
	t.Helper()
	c := newClock()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Name}
		}).Build()
	p := NewPodCollector(cl, zap.New(zap.UseDevMode(true)), withClock(c.now), WithEventReader(cl))

	created := ago(c, 45*time.Second) // pod is 45 s old at the first sighting
	out := make([][]Signal, 0, len(counts))
	for i, n := range counts {
		pod := runningPod("api-1", terminatedBetweenRuns(c, n))
		pod.CreationTimestamp = created
		if i == 0 {
			require.NoError(t, cl.Create(context.Background(), &pod))
		} else {
			var cur corev1.Pod
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(&pod), &cur))
			cur.Status = pod.Status
			require.NoError(t, cl.Update(context.Background(), &cur))
		}
		sigs, err := p.Collect(context.Background())
		require.NoError(t, err)
		out = append(out, sigs)
		c.advance(step)
	}
	return out
}

// Same crash loop, nothing changed, four cycles apart by 30 s (the chart default):
// the fault is continuous, so the signal must be.
func TestReview_YoungPodRestartLoopSignalDoesNotVanishWhileTheLoopContinues(t *testing.T) {
	cycles := youngPodAcrossCycles(t, []int32{2, 2, 3, 3}, 30*time.Second)

	require.True(t, hasType(cycles[0], SignalRestartLoop), "first sight of a 45 s old pod with 2 restarts fires")
	for i := 1; i < len(cycles); i++ {
		assert.True(t, hasType(cycles[i], SignalRestartLoop),
			"cycle %d: 3 restarts in a pod 2 minutes old is still a restart loop, got %v", i, signalTypes(cycles[i]))
	}
}

// Five cycles of a continuing crash loop on a young pod: the signal never drops.
func TestReview_YoungPodSignalHoldsForFiveCycles(t *testing.T) {
	cycles := youngPodAcrossCycles(t, []int32{2, 2, 3, 3, 4}, 30*time.Second)

	for i := range cycles {
		assert.Equal(t, []SignalType{SignalRestartLoop}, signalTypes(cycles[i]), "cycle %d", i)
	}
}

// Once the pod is older than the window the floor ends, and the tracker's own history
// decides: a loop that stopped (no restart for longer than the window) stops signalling.
func TestReview_YoungPodSignalEndsWhenTheRestartsLeaveTheWindow(t *testing.T) {
	// 45 s old, then every 3 minutes: 45 s, 3m45, 6m45, 9m45, 12m45 (past the 10 min window).
	cycles := youngPodAcrossCycles(t, []int32{2, 2, 2, 2, 2}, 3*time.Minute)

	assert.True(t, hasType(cycles[0], SignalRestartLoop))
	assert.True(t, hasType(cycles[3], SignalRestartLoop), "still inside the window at 9m45")
	assert.False(t, hasType(cycles[4], SignalRestartLoop), "the restarts are 12 minutes old: the loop ended")
}
