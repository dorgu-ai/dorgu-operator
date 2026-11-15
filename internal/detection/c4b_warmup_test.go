package detection

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// C4b N-1: after the CPU source was blind, its silence is not evidence that a pod stopped
// being throttled until it has produced as many consecutive good cycles as the signal
// needs. For metrics.k8s.io that is CPUUsageConsecutiveCycles: with fewer, a pod pinned
// at its limit reads as quiet because the streak that would say otherwise has not had
// time to build. Blind (the verifier's question) is unchanged: it cannot afford three
// Unknown cycles. ResolutionBlind is resolution's.

func quietUsage() map[string]string { return map[string]string{"api-1 app": "100m"} }

func limitedPod() client.Object { return throttlePod("api-1", map[string]string{"app": "1000m"}) }

// cycle runs one Collect (the collectors in these tests have no spacing window).
func cycle(c *CPUThrottlingCollector) { _, _ = c.Collect(context.Background()) }

func TestCPUThrottling_ResolutionIsBlindForTheWarmUpCyclesOfMetricsServer(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(quietUsage())
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics))
	require.True(t, c.ResolutionBlind(), "before the first cycle")

	want := []bool{true, true, false, false} // cycles 1..4
	for i, wantBlind := range want {
		cycle(c)
		assert.False(t, c.Blind(), "cycle %d: the verifier's question is unchanged", i+1)
		assert.Equal(t, wantBlind, c.ResolutionBlind(), "cycle %d", i+1)
	}
}

func TestCPUThrottling_ResolutionWarmUpRestartsAfterAnyBreak(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(quietUsage())
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics))
	for range CPUUsageConsecutiveCycles {
		cycle(c)
	}
	require.False(t, c.ResolutionBlind())

	metrics.mu.Lock()
	metrics.listErr = errors.New("503")
	metrics.mu.Unlock()
	cycle(c)
	assert.True(t, c.ResolutionBlind(), "a cycle that could not look")

	metrics.mu.Lock()
	metrics.listErr = nil
	metrics.mu.Unlock()
	cycle(c)
	assert.True(t, c.ResolutionBlind(), "good cycle 1 of the new warm-up")
	cycle(c)
	assert.True(t, c.ResolutionBlind(), "good cycle 2")
	cycle(c)
	assert.False(t, c.ResolutionBlind(), "good cycle 3")

	// Source lost and back.
	metrics.mu.Lock()
	metrics.present = false
	metrics.mu.Unlock()
	_, _ = c.Collect(context.Background())
	metrics.mu.Lock()
	metrics.present = true
	metrics.mu.Unlock()
	cycle(c)
	assert.True(t, c.ResolutionBlind(), "the source came back: warm-up again")
}

func TestCPUThrottling_AnAnswerThatNamesNoLimitedContainerIsNotAGoodCycle(t *testing.T) {
	metrics := &fakeMetricsSource{present: true} // served, empty list: metrics-server just restarted
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics))
	for i := range 2 * CPUUsageConsecutiveCycles {
		cycle(c)
		assert.False(t, c.Blind(), "the source answered, so detection is on")
		assert.True(t, c.ResolutionBlind(), "cycle %d: an empty list for a pod with a CPU limit is no data, not quiet", i+1)
	}

	metrics.set(quietUsage())
	for range CPUUsageConsecutiveCycles {
		cycle(c)
	}
	assert.False(t, c.ResolutionBlind(), "once the pod is seen for three cycles")
}

func TestCPUThrottling_AnEmptyAnswerIsFineWhenNoPodHasACPULimit(t *testing.T) {
	unlimited := throttlePod("api-1", map[string]string{"app": ""})
	c := newThrottleCollector(t, []client.Object{unlimited}, WithMetricsSource(&fakeMetricsSource{present: true}))
	for range CPUUsageConsecutiveCycles {
		cycle(c)
	}
	assert.False(t, c.ResolutionBlind(), "there is nothing that could be throttled to be missing")
}

func TestCPUThrottling_ResolutionWarmUpFollowsTheSource(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(quietUsage())
	prom := &fakeThrottleSource{samples: []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.01}}}
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics), WithThrottleSource(prom))

	cycle(c)
	assert.False(t, c.ResolutionBlind(), "Prometheus's ratio is a five-minute rate: it needs no warm-up")

	prom.err = errors.New("refused") // falls back to metrics.k8s.io, whose streaks start from zero
	cycle(c)
	assert.True(t, c.ResolutionBlind(), "fallback cycle 1")
	cycle(c)
	assert.True(t, c.ResolutionBlind(), "fallback cycle 2")
	cycle(c)
	assert.False(t, c.ResolutionBlind(), "fallback cycle 3")

	prom.err = nil // and back to Prometheus
	cycle(c)
	assert.False(t, c.ResolutionBlind())
}

func TestCPUThrottling_ACachedCallInsideTheSpacingWindowIsNotAWarmUpCycle(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	metrics := &fakeMetricsSource{present: true}
	metrics.set(quietUsage())
	c := newThrottleCollector(t, []client.Object{limitedPod()}, WithMetricsSource(metrics),
		WithMinCycleSpacing(time.Minute), WithCPUClock(func() time.Time { return now }))
	for range 5 { // a verifier hammering Collect inside one window
		cycle(c)
		now = now.Add(time.Second)
	}
	assert.True(t, c.ResolutionBlind(), "five calls in one window are one cycle")
}

// ---------------------------------------------------------------------------
// the engine
// ---------------------------------------------------------------------------

type fakeResolutionCollector struct {
	fakeFamilyCollector
	resolutionBlind bool
}

func (f *fakeResolutionCollector) ResolutionBlind() bool { return f.resolutionBlind }

func TestEngine_ResolutionBlindAddsTheWarmUpToBlind(t *testing.T) {
	fc := &fakeResolutionCollector{fakeFamilyCollector: fakeFamilyCollector{
		name: "cpu", families: []Family{FamilyCPUThrottling}}, resolutionBlind: true}
	e := NewEngine(logr.Discard(), fc)
	_, err := e.CollectAll(context.Background())
	require.NoError(t, err)

	assert.False(t, e.SignalBlind(SignalCPUThrottling), "the verifier's answer ignores the warm-up")
	assert.True(t, e.ResolutionBlind(SignalCPUThrottling), "resolution's does not")
	assert.False(t, e.ResolutionBlind(SignalOOMKilled), "and no other family is held")
	assert.Equal(t, map[Family]bool{FamilyCPUThrottling: true}, e.ResolutionBlindFamilies())

	fc.resolutionBlind = false
	assert.False(t, e.ResolutionBlind(SignalCPUThrottling))
	assert.Empty(t, e.ResolutionBlindFamilies())
}

func TestEngine_ResolutionBlindIsAtLeastBlind(t *testing.T) {
	// A FamilyCollector with no warm-up of its own: resolution blindness is its Blind.
	fc := &fakeFamilyCollector{name: "cpu", families: []Family{FamilyCPUThrottling}, blind: true}
	e := NewEngine(logr.Discard(), fc)
	assert.True(t, e.ResolutionBlind(SignalCPUThrottling))
	assert.Equal(t, map[Family]bool{FamilyCPUThrottling: true}, e.ResolutionBlindFamilies())

	// And a collector that failed in the latest round is blind whatever it says.
	failing := &fakeFamilyCollector{name: "cpu", families: []Family{FamilyCPUThrottling}, err: errors.New("down")}
	e = NewEngine(logr.Discard(), failing)
	_, _ = e.CollectAll(context.Background())
	assert.True(t, e.ResolutionBlind(SignalCPUThrottling))
}
