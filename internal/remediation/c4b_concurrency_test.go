package remediation

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

// C4b N-6: the verifier is one of the readers of the CPU collector's blindness state,
// which the reconciler's cycle writes. Run with -race.

// flappingMetrics is a metrics.k8s.io that is served, unserved or unreadable depending
// on a counter the test bumps.
type flappingMetrics struct{ mode atomic.Int32 }

func (m *flappingMetrics) Available(context.Context) (bool, error) { return m.mode.Load()%3 != 0, nil }
func (m *flappingMetrics) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	if m.mode.Load()%3 == 1 {
		return nil, assert.AnError
	}
	return nil, nil
}

func TestVerifier_ConcurrentWithTheCollectionCycleOverAFlappingCPUSource(t *testing.T) {
	const iterations = 100
	metrics := &flappingMetrics{}
	collector := cpuCollectorWith(t, detection.WithMetricsSource(metrics))
	engine := detection.NewEngine(logr.Discard(), collector)
	v := verifierOver(t, string(detection.SignalCPUThrottling), collector)
	// verifierOver built its own engine over the same collector; the cycle below writes
	// through a second engine, so the verifier's reads race the collector, not one lock.
	ctx := context.Background()

	var wg sync.WaitGroup
	run := func(f func(int)) {
		wg.Go(func() {
			for i := range iterations {
				f(i)
			}
		})
	}
	run(func(int) { _, _ = engine.CollectAll(ctx) }) // the reconciler's cycle: the writer
	run(func(int) {                                  // verifications
		result, _ := v.Verify(ctx, testRemediationAction())
		assert.Contains(t, []VerificationResult{VerificationHealthy, VerificationUnknown}, result)
	})
	run(func(int) { _ = engine.ResolutionBlind(detection.SignalCPUThrottling) }) // the resolve step
	run(func(i int) { metrics.mode.Store(int32(i)) })                            // the source flaps
	wg.Wait()

	// Settled on a served, readable source the verifier can look again.
	metrics.mode.Store(2)
	result, err := v.Verify(ctx, testRemediationAction())
	require.NoError(t, err)
	assert.Equal(t, VerificationHealthy, result, "the verifier does not wait for the resolution warm-up")
}
