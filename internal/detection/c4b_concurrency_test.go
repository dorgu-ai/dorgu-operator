package detection

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// C4b N-6: the CPU collector's blindness, warm-up and streak state, and the engine's
// failed-collector set, are shared by the reconciler (which collects, then asks whether
// to resolve), the verifier (which collects, then asks whether it can look) and the
// operator's own cycle. They are guarded by three locks (the collector's, its view's, the
// engine's) and this test is what keeps them guarded: run it with -race.

func TestEngine_BlindnessAndStreakStateIsSafeUnderConcurrentReadersAndAWriter(t *testing.T) {
	const iterations = 150
	metrics := &fakeMetricsSource{present: true}
	hot := map[string]string{"api-1 app": "990m"}
	metrics.set(hot)
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics))
	engine := NewEngine(logr.Discard(), c)
	ctx := context.Background()

	var wg sync.WaitGroup
	run := func(f func(i int)) {
		wg.Go(func() {
			for i := range iterations {
				f(i)
			}
		})
	}

	// The collector's writer: the reconciler's cycle.
	run(func(int) { _, _ = engine.CollectAll(ctx) })
	// A verifier: runs detection too, then asks whether it can look.
	run(func(int) {
		_, _ = engine.CollectAll(ctx)
		_ = engine.SignalBlind(SignalCPUThrottling)
		_ = engine.SignalBlind(SignalOOMKilled)
	})
	// The reconciler's resolve step: asks resolution's question, live and as a snapshot.
	run(func(int) {
		_ = engine.ResolutionBlind(SignalCPUThrottling)
		_ = engine.ResolutionBlindFamilies()
		_ = engine.ResolutionSnapshot()
		_ = engine.FamilyBlind(FamilyCPUThrottling)
	})
	// The collector's own readers.
	run(func(int) {
		_ = c.Blind()
		_ = c.ResolutionBlind()
		_ = c.ResolutionPending()
	})
	// A flapping source: gone, unreadable, quiet and hot, so every state is rewritten.
	run(func(i int) {
		metrics.mu.Lock()
		defer metrics.mu.Unlock()
		switch i % 4 {
		case 0:
			metrics.present = false
		case 1:
			metrics.present, metrics.listErr = true, assert.AnError
		case 2:
			metrics.listErr = nil
			metrics.usage = quietUsage()
		default:
			metrics.usage = hot
		}
	})
	wg.Wait()

	// Settled, the state is exactly what a quiet run would have: nothing was lost or torn.
	// One failed cycle first, which starts every streak and the warm-up over whatever the
	// flapping left behind.
	metrics.mu.Lock()
	metrics.present, metrics.listErr, metrics.usage = true, assert.AnError, hot
	metrics.mu.Unlock()
	_, _ = engine.CollectAll(ctx)
	metrics.mu.Lock()
	metrics.listErr = nil
	metrics.mu.Unlock()
	for range CPUUsageConsecutiveCycles - 1 {
		signals, err := engine.CollectAll(ctx)
		require.NoError(t, err)
		assert.Empty(t, signals)
		assert.False(t, engine.SignalBlind(SignalCPUThrottling))
		assert.True(t, engine.ResolutionBlind(SignalCPUThrottling), "still warming up")
	}
	signals, err := engine.CollectAll(ctx)
	require.NoError(t, err)
	require.Len(t, signals, 1, "three consecutive hot cycles after the flapping stopped")
	assert.False(t, engine.ResolutionBlind(SignalCPUThrottling))
	assert.Empty(t, engine.ResolutionBlindFamilies())
}

// blockingMetrics is a metrics.k8s.io whose PodMetrics call waits for the test, so a
// Collect can be held in the middle of a read.
type blockingMetrics struct {
	entered chan struct{}
	release chan struct{}
}

func (*blockingMetrics) Available(context.Context) (bool, error) { return true, nil }
func (b *blockingMetrics) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	close(b.entered)
	<-b.release
	return nil, nil
}

// The readers of the collector's view (Blind, ResolutionBlind, ResolutionPending) are
// guarded by the view lock. The race detector only sees a missing lock when the two
// accesses happen to overlap, so this asks the question directly: a reader must wait for a
// writer that holds the lock, and must not wait for a Collect that does not.
func TestCollector_TheViewReadersWaitForTheViewLockAndNotForACollect(t *testing.T) {
	readers := map[string]func(c *CPUThrottlingCollector){
		"Blind":             func(c *CPUThrottlingCollector) { _ = c.Blind() },
		"ResolutionBlind":   func(c *CPUThrottlingCollector) { _ = c.ResolutionBlind() },
		"ResolutionPending": func(c *CPUThrottlingCollector) { _ = c.ResolutionPending() },
	}
	const settle = 50 * time.Millisecond
	const patience = 5 * time.Second

	for name, read := range readers {
		t.Run(name+" waits for a writer", func(t *testing.T) {
			c := newThrottleCollector(t, nil)
			c.viewMu.Lock()
			done := make(chan struct{})
			go func() { read(c); close(done) }()
			select {
			case <-done:
				c.viewMu.Unlock()
				t.Fatalf("%s returned while a writer held the view lock: it reads the view unlocked", name)
			case <-time.After(settle):
			}
			c.viewMu.Unlock()
			select {
			case <-done:
			case <-time.After(patience):
				t.Fatalf("%s never returned after the writer let go", name)
			}
		})

		t.Run(name+" does not wait for a Collect", func(t *testing.T) {
			metrics := &blockingMetrics{entered: make(chan struct{}), release: make(chan struct{})}
			c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics))
			collected := make(chan struct{})
			go func() { _, _ = c.Collect(context.Background()); close(collected) }()
			<-metrics.entered // Collect holds its own lock and is inside the read

			done := make(chan struct{})
			go func() { read(c); close(done) }()
			select {
			case <-done:
			case <-time.After(patience):
				close(metrics.release)
				<-collected
				t.Fatalf("%s waited behind a Collect that was in the middle of a read", name)
			}
			close(metrics.release)
			<-collected
		})
	}
}
