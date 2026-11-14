package detection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// C4 fix round (review F-1..F-11 on PR 64). Blindness is scoped to the families a
// collector owns: a CPU metrics source that is absent, failing or lost blinds
// CPUThrottling and nothing else.

// fakeFamilyCollector is a collector that owns a family.
type fakeFamilyCollector struct {
	name     string
	families []Family
	signals  []Signal
	err      error
	blind    bool
}

func (f *fakeFamilyCollector) Name() string                              { return f.name }
func (f *fakeFamilyCollector) Families() []Family                        { return f.families }
func (f *fakeFamilyCollector) Blind() bool                               { return f.blind }
func (f *fakeFamilyCollector) Collect(context.Context) ([]Signal, error) { return f.signals, f.err }

var _ FamilyCollector = (*fakeFamilyCollector)(nil)

// ---------------------------------------------------------------------------
// F-1 / F-2: the in-process freshness predicate
// ---------------------------------------------------------------------------

func TestCPUThrottling_BlindBeforeTheFirstCollect(t *testing.T) {
	c := newThrottleCollector(t, nil, WithMetricsSource(&fakeMetricsSource{present: true}))
	assert.True(t, c.Blind(), "a collector that has not looked yet cannot vouch for silence (the restart case)")
	assert.Equal(t, []Family{FamilyCPUThrottling}, c.Families())
}

func TestCPUThrottling_BlindFollowsTheLastObservedStatus(t *testing.T) {
	pod := throttlePod("api-1", map[string]string{"app": "1000m"})
	tests := []struct {
		name      string
		opts      []CPUThrottlingOption
		wantBlind bool
	}{
		{"metrics.k8s.io read", []CPUThrottlingOption{WithMetricsSource(&fakeMetricsSource{present: true})}, false},
		{"Prometheus read", []CPUThrottlingOption{WithThrottleSource(&fakeThrottleSource{samples: []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.01}}})}, false},
		{"no source at all", nil, true},
		{"no metrics-server installed", []CPUThrottlingOption{WithMetricsSource(&fakeMetricsSource{present: false})}, true},
		{"metrics-server disabled by flag", []CPUThrottlingOption{WithMetricsServerDisabled()}, true},
		{"metrics listing fails", []CPUThrottlingOption{WithMetricsSource(&fakeMetricsSource{present: true, listErr: errors.New("503")})}, true},
		{"Prometheus down, no fallback", []CPUThrottlingOption{WithThrottleSource(&fakeThrottleSource{err: errors.New("refused")})}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newThrottleCollector(t, []client.Object{pod}, tt.opts...)
			_, _ = c.Collect(context.Background())
			assert.Equal(t, tt.wantBlind, c.Blind())
		})
	}
}

func TestCPUThrottling_BlindIsRememberedInsideTheSpacingWindow(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	metrics := &fakeMetricsSource{present: true}
	c := newThrottleCollector(t, nil, WithMetricsSource(metrics),
		WithMinCycleSpacing(time.Minute), WithCPUClock(func() time.Time { return now }))
	_, err := c.Collect(context.Background())
	require.NoError(t, err)
	require.False(t, c.Blind())

	metrics.mu.Lock()
	metrics.present = false
	metrics.mu.Unlock()
	now = now.Add(10 * time.Second) // a verifier read: served from the cycle that already ran
	_, _ = c.Collect(context.Background())
	assert.False(t, c.Blind(), "a cached answer keeps the status of the cycle that produced it")

	now = now.Add(time.Minute)
	_, _ = c.Collect(context.Background())
	assert.True(t, c.Blind())
}

// ---------------------------------------------------------------------------
// F-5: the engine attributes a failure to the families the collector owns
// ---------------------------------------------------------------------------

func TestEngine_AFamilyCollectorFailureIsNotAWholeCycleFailure(t *testing.T) {
	cpu := &fakeFamilyCollector{name: "cpu", families: []Family{FamilyCPUThrottling}, err: errors.New("503")}
	pods := &mockSignalCollector{name: "pods", signals: []Signal{{Type: SignalOOMKilled, Severity: SeverityCritical}}}
	e := NewEngine(logr.Discard(), cpu, pods)

	signals, err := e.CollectAll(context.Background())
	require.NoError(t, err, "the CPU source failing must not make every other family's silence unprovable")
	assert.False(t, IsPartial(err))
	require.Len(t, signals, 1)
	assert.True(t, e.FamilyBlind(FamilyCPUThrottling))
	assert.True(t, e.SignalBlind(SignalCPUThrottling))
	assert.False(t, e.SignalBlind(SignalOOMKilled), "the fault family is not blind")
	assert.False(t, e.SignalBlind(SignalCPUSaturationHigh), "a signal in no family is never blind")
}

func TestEngine_ACollectorWithNoFamilyStillFailsTheWholeCycle(t *testing.T) {
	// B2b for the collectors that look at pods and workloads: if one cannot look,
	// silence about anything it might have seen proves nothing.
	cpu := &fakeFamilyCollector{name: "cpu", families: []Family{FamilyCPUThrottling}}
	pods := &mockSignalCollector{name: "pods", err: errors.New("pod list failed")}
	e := NewEngine(logr.Discard(), cpu, pods)

	_, err := e.CollectAll(context.Background())
	require.Error(t, err)
	assert.True(t, IsPartial(err))
	var ce *CollectionError
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, []string{"pods"}, ce.Collectors())
}

func TestEngine_BlindComesFromTheCollectorWithNoError(t *testing.T) {
	cpu := &fakeFamilyCollector{name: "cpu", families: []Family{FamilyCPUThrottling}, blind: true}
	e := NewEngine(logr.Discard(), cpu)

	_, err := e.CollectAll(context.Background())
	require.NoError(t, err, "an absent source is a missing capability, not an error")
	assert.True(t, e.SignalBlind(SignalCPUThrottling), "but it cannot vouch for silence")

	cpu.blind = false
	_, err = e.CollectAll(context.Background())
	require.NoError(t, err)
	assert.False(t, e.SignalBlind(SignalCPUThrottling))
}

func TestEngine_BlindBeforeAnyCollectionRound(t *testing.T) {
	cpu := &fakeFamilyCollector{name: "cpu", families: []Family{FamilyCPUThrottling}, blind: true}
	e := NewEngine(logr.Discard(), cpu)
	assert.True(t, e.SignalBlind(SignalCPUThrottling))
	assert.False(t, NewEngine(logr.Discard()).SignalBlind(SignalCPUThrottling),
		"no collector owns the family, so nothing can be blind to it")
}

func TestEngine_FamilyFailureRecoversWhenTheCollectorDoes(t *testing.T) {
	cpu := &fakeFamilyCollector{name: "cpu", families: []Family{FamilyCPUThrottling}, err: errors.New("503")}
	e := NewEngine(logr.Discard(), cpu)
	_, _ = e.CollectAll(context.Background())
	require.True(t, e.SignalBlind(SignalCPUThrottling))
	cpu.err = nil
	_, _ = e.CollectAll(context.Background())
	assert.False(t, e.SignalBlind(SignalCPUThrottling))
}

// mockSignalCollector is a plain collector that owns no family.
type mockSignalCollector struct {
	name    string
	signals []Signal
	err     error
}

func (m *mockSignalCollector) Name() string { return m.name }
func (m *mockSignalCollector) Collect(context.Context) ([]Signal, error) {
	return m.signals, m.err
}

// The CPU collector reaches the engine as a family collector: its failure blinds
// CPUThrottling only.
func TestCPUThrottling_FailureBlindsItsFamilyOnly(t *testing.T) {
	metrics := &fakeMetricsSource{present: true, listErr: errors.New("503")}
	c := newThrottleCollector(t, nil, WithMetricsSource(metrics))
	_, err := c.Collect(context.Background())
	require.Error(t, err, "the collector itself still says it could not look")

	e := NewEngine(logr.Discard(), c)
	_, err = e.CollectAll(context.Background())
	require.NoError(t, err)
	assert.True(t, e.SignalBlind(SignalCPUThrottling))
	assert.False(t, e.SignalBlind(SignalOOMKilled))
}

// ---------------------------------------------------------------------------
// F-4: an empty Prometheus answer is not an active source
// ---------------------------------------------------------------------------

func TestCPUThrottling_EmptyPrometheusAnswerIsNotASource(t *testing.T) {
	pod := throttlePod("api-1", map[string]string{"app": "1000m"})
	limitedAndHot := func() *fakeMetricsSource {
		m := &fakeMetricsSource{present: true}
		m.set(map[string]string{"api-1 app": "999m"})
		return m
	}

	t.Run("falls back to metrics.k8s.io and says why", func(t *testing.T) {
		metrics := limitedAndHot()
		prom := &fakeThrottleSource{samples: []ThrottleSample{}}
		sink := &statusRecorder{}
		c := newThrottleCollector(t, []client.Object{pod}, WithMetricsSource(metrics), WithThrottleSource(prom), WithCPUStatusSink(sink))
		for range 2 {
			collectTypes(t, c)
		}
		signals := collectTypes(t, c)
		require.Len(t, signals, 1, "metrics.k8s.io saw the pod at 99.9% of its limit for three cycles")
		assert.Equal(t, CPUThrottlingSourceMetricsServer, signals[0].Metadata["source"])
		st := sink.latest(t)
		assert.True(t, st.Active)
		assert.Equal(t, CPUThrottlingSourceMetricsServer, st.Source)
		assert.Equal(t, ReasonPrometheusNoData, st.Reason)
		assert.Contains(t, st.Message, "no CPU throttling series")
		assert.False(t, c.Blind())
	})

	t.Run("with no fallback the collector could not look", func(t *testing.T) {
		sink := &statusRecorder{}
		prom := &fakeThrottleSource{samples: []ThrottleSample{}}
		c := newThrottleCollector(t, []client.Object{pod}, WithThrottleSource(prom), WithCPUStatusSink(sink))
		_, err := c.Collect(context.Background())
		require.Error(t, err)
		st := sink.latest(t)
		assert.False(t, st.Active)
		assert.Equal(t, ReasonPrometheusNoData, st.Reason)
		assert.True(t, c.Blind())
	})

	t.Run("series for containers that are not in the cluster are no data either", func(t *testing.T) {
		prom := &fakeThrottleSource{samples: []ThrottleSample{{Namespace: throttleNS, Pod: "ghost", Container: "app", Ratio: 0.0}}}
		c := newThrottleCollector(t, []client.Object{pod}, WithThrottleSource(prom))
		_, err := c.Collect(context.Background())
		require.Error(t, err)
	})

	t.Run("a series for a known container, even a quiet one, is data", func(t *testing.T) {
		sink := &statusRecorder{}
		prom := &fakeThrottleSource{samples: []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.0}}}
		c := newThrottleCollector(t, []client.Object{pod}, WithThrottleSource(prom), WithCPUStatusSink(sink))
		signals, err := c.Collect(context.Background())
		require.NoError(t, err)
		assert.Empty(t, signals)
		assert.Equal(t, ReasonPrometheusActive, sink.latest(t).Reason)
	})

	t.Run("an empty answer is fine when no container has a CPU limit", func(t *testing.T) {
		sink := &statusRecorder{}
		unlimited := throttlePod("api-1", map[string]string{"app": ""})
		c := newThrottleCollector(t, []client.Object{unlimited}, WithThrottleSource(&fakeThrottleSource{}), WithCPUStatusSink(sink))
		_, err := c.Collect(context.Background())
		require.NoError(t, err, "nothing could be throttled, so an empty answer contradicts nothing")
		assert.Equal(t, ReasonPrometheusActive, sink.latest(t).Reason)
	})
}

// ---------------------------------------------------------------------------
// F-12: an invalid URL is not "unreachable"
// ---------------------------------------------------------------------------

func TestCPUThrottling_InvalidPrometheusURLHasItsOwnReason(t *testing.T) {
	sink := &statusRecorder{}
	prom := &fakeThrottleSource{err: fmt.Errorf("%w: prometheus url must not carry credentials", ErrPrometheusURLInvalid)}
	c := newThrottleCollector(t, nil, WithMetricsSource(&fakeMetricsSource{present: false}),
		WithThrottleSource(prom), WithCPUStatusSink(sink))
	_, err := c.Collect(context.Background())
	require.Error(t, err)
	assert.Equal(t, ReasonPrometheusURLInvalid, sink.latest(t).Reason)
	assert.Contains(t, sink.latest(t).Message, "spec.metrics.prometheus.url")
	assert.NotContains(t, sink.latest(t).Message, "credentials", "fixed text, never the underlying error")

	// With a fallback it is announced on the same reason.
	sink2 := &statusRecorder{}
	metrics := &fakeMetricsSource{present: true}
	c2 := newThrottleCollector(t, nil, WithMetricsSource(metrics), WithThrottleSource(prom), WithCPUStatusSink(sink2))
	_, err = c2.Collect(context.Background())
	require.NoError(t, err)
	assert.True(t, sink2.latest(t).Active)
	assert.Equal(t, ReasonPrometheusURLInvalid, sink2.latest(t).Reason)
}

func TestValidatePrometheusURL_FailuresAreTheInvalidSentinel(t *testing.T) {
	for _, bad := range []string{"ftp://x", "http://u:p@host", "http://host?q=1", "http://"} {
		_, err := ValidatePrometheusURL(bad)
		require.Error(t, err, "%q", bad)
		assert.ErrorIs(t, err, ErrPrometheusURLInvalid, "%q", bad)
	}
}
