package controller

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// C4b N-6: the reconciler's cycle writes the CPU collector's blindness, warm-up and
// streak state while the verifier and the operator's other readers ask about it. Run
// with -race.

type flappingPodMetrics struct{ mode atomic.Int32 }

func (m *flappingPodMetrics) Available(context.Context) (bool, error) {
	return m.mode.Load()%3 != 0, nil
}
func (m *flappingPodMetrics) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	if m.mode.Load()%3 == 1 {
		return nil, assert.AnError
	}
	return nil, nil
}

func TestC4bN6_TheReconcilerCycleIsSafeAgainstConcurrentBlindnessReaders(t *testing.T) {
	const iterations = 60
	ctx := context.Background()
	crash := openIncident("platform", "checkout", "im-platform-checkout-crash")
	cpu := cpuIncidentFor()
	r, c := resolverFor(t, crash, cpu, deploymentFor("platform", "checkout", 1), limitedCheckoutPod())

	metrics := &flappingPodMetrics{}
	collector := detection.NewCPUThrottlingCollector(c, logr.Discard(),
		detection.WithMinCycleSpacing(0), detection.WithMetricsSource(metrics))
	engine := detection.NewEngine(logr.Discard(), collector)
	r.Detection = engine

	var wg sync.WaitGroup
	run := func(f func(int)) {
		wg.Go(func() {
			for i := range iterations {
				f(i)
			}
		})
	}
	run(func(int) { r.runCycle(ctx) })                                                        // the reconciler: collects, then resolves
	run(func(int) { _, _ = engine.CollectAll(ctx); _ = engine.SignalBlind("CPUThrottling") }) // a verifier
	run(func(int) { _ = r.resolutionBlind(detection.SignalCPUThrottling, engine.ResolutionBlindFamilies()) })
	run(func(i int) { metrics.mode.Store(int32(i)) })
	wg.Wait()

	// The crash-loop incident was never held by CPU blindness; the CPU incident never
	// resolved on a source that mostly could not look and never named the pod.
	assert.NotEqual(t, PhaseDetected, phaseOf(t, c, crash), "a crash loop resolves whatever the CPU source does")
	require.Equal(t, PhaseDetected, phaseOf(t, c, cpu), "the limited pod was never seen by the CPU source")
}
