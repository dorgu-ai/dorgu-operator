package detection

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// C4b N-2: a Prometheus ratio is 0/0 for a container that did no work in the window,
// and the API writes that as "NaN". An idle but healthy cluster answers NaN for every
// container; that is "answered, idle", not "no cAdvisor series at all".

func TestPrometheusThrottle_ANaNRatioIsAnIdleContainerNotNoData(t *testing.T) {
	src := promServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(vector(series("shop", "api-1", "app", "NaN"))))
	})
	samples, err := src.Query(context.Background())
	require.NoError(t, err)
	require.Len(t, samples, 1, "the container answered: it was idle")
	assert.Equal(t, ThrottleSample{Namespace: "shop", Pod: "api-1", Container: "app", Ratio: 0, NoPeriods: true}, samples[0],
		"ratio 0, and marked: the counters never advanced, which C4c tells apart from a measured 0")
}

func TestPrometheusThrottle_GarbageIsStillDroppedWhenNaNIsKept(t *testing.T) {
	for name, value := range map[string]string{"+Inf": "+Inf", "-Inf": "-Inf", "negative": "-0.5", "above one": "1.5", "text": "lots"} {
		t.Run(name, func(t *testing.T) {
			src := promServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(vector(series("shop", "api-1", "app", value))))
			})
			samples, err := src.Query(context.Background())
			require.NoError(t, err)
			assert.Empty(t, samples)
		})
	}
}

func TestCPUThrottling_AnIdleButHealthyClusterIsAnActiveSourceWithNoThrottling(t *testing.T) {
	pod := throttlePod("api-1", map[string]string{"app": "1000m"})
	idle := func(t *testing.T, value string) (*statusRecorder, *CPUThrottlingCollector, error) {
		t.Helper()
		src := promServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(vector(series(throttleNS, "api-1", "app", value))))
		})
		sink := &statusRecorder{}
		c := newThrottleCollector(t, []client.Object{pod}, WithThrottleSource(src), WithCPUStatusSink(sink))
		_, err := c.Collect(context.Background())
		return sink, c, err
	}

	t.Run("NaN for every limited container is not an active source (C4c I-1)", func(t *testing.T) {
		// Every known container reads 0/0: the CFS counters never advanced, which is what a
		// runtime that exports none of them looks like. That is not a source that measured
		// "nothing throttled", so with no metrics-server there is nothing to read.
		sink, c, err := idle(t, "NaN")
		require.Error(t, err)
		st := sink.latest(t)
		assert.False(t, st.Active)
		assert.Equal(t, ReasonPrometheusNoData, st.Reason)
		assert.True(t, c.Blind())
	})

	t.Run("a mix of NaN and a measured ratio is idle", func(t *testing.T) {
		pods := []client.Object{pod, throttlePod("api-2", map[string]string{"app": "1000m"})}
		src := promServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(vector(
				series(throttleNS, "api-1", "app", "NaN"),
				series(throttleNS, "api-2", "app", "0"),
			)))
		})
		sink := &statusRecorder{}
		c := newThrottleCollector(t, pods, WithThrottleSource(src), WithCPUStatusSink(sink))
		signals, err := c.Collect(context.Background())
		require.NoError(t, err, "one container measured 0: cAdvisor is being scraped and the cluster is quiet")
		assert.Empty(t, signals)
		st := sink.latest(t)
		assert.True(t, st.Active)
		assert.Equal(t, CPUThrottlingSourcePrometheus, st.Source)
		assert.Equal(t, ReasonPrometheusActive, st.Reason)
		assert.False(t, c.Blind())
		assert.False(t, c.ResolutionBlind(), "Prometheus's ratio is a five-minute rate already; idle is a real answer")
	})

	t.Run("a NaN beside a throttled container still raises the signal", func(t *testing.T) {
		pods := []client.Object{pod, throttlePod("api-2", map[string]string{"app": "1000m"})}
		prom := &fakeThrottleSource{samples: []ThrottleSample{
			{Namespace: throttleNS, Pod: "api-1", Container: "app", NoPeriods: true},
			{Namespace: throttleNS, Pod: "api-2", Container: "app", Ratio: 0.6},
		}}
		c := newThrottleCollector(t, pods, WithThrottleSource(prom))
		signals, err := c.Collect(context.Background())
		require.NoError(t, err)
		require.Len(t, signals, 1)
		assert.Equal(t, SeverityCritical, signals[0].Severity)
	})

	t.Run("NaN for every container falls back to metrics.k8s.io as before C4b", func(t *testing.T) {
		metrics := &fakeMetricsSource{present: true}
		metrics.set(map[string]string{"api-1 app": "990m"})
		prom := &fakeThrottleSource{samples: []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", NoPeriods: true}}}
		sink := &statusRecorder{}
		c := newThrottleCollector(t, []client.Object{pod}, WithThrottleSource(prom), WithMetricsSource(metrics), WithCPUStatusSink(sink))

		var signals []Signal
		for range CPUUsageConsecutiveCycles {
			var err error
			signals, err = c.Collect(context.Background())
			require.NoError(t, err)
		}
		st := sink.latest(t)
		assert.True(t, st.Active)
		assert.Equal(t, CPUThrottlingSourceMetricsServer, st.Source, "Prometheus is not an active idle source")
		assert.Equal(t, ReasonPrometheusNoData, st.Reason, "and the status says why")
		require.Len(t, signals, 1, "the pinned container is found by the usage threshold again")
		assert.Equal(t, SeverityWarning, signals[0].Severity)
		assert.False(t, c.ResolutionBlind(), "metrics.k8s.io is warm after three cycles")
	})

	t.Run("a measured 0 from every container is idle, not a fallback", func(t *testing.T) {
		metrics := &fakeMetricsSource{present: true}
		metrics.set(map[string]string{"api-1 app": "990m"})
		prom := &fakeThrottleSource{samples: []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0}}}
		sink := &statusRecorder{}
		c := newThrottleCollector(t, []client.Object{pod}, WithThrottleSource(prom), WithMetricsSource(metrics), WithCPUStatusSink(sink))
		_, err := c.Collect(context.Background())
		require.NoError(t, err)
		assert.Equal(t, CPUThrottlingSourcePrometheus, sink.latest(t).Source)
		assert.Zero(t, metrics.calls, "metrics.k8s.io is not consulted when Prometheus has measured something")
	})

	t.Run("garbage for every container is still no data", func(t *testing.T) {
		sink, c, err := idle(t, "+Inf")
		require.Error(t, err)
		assert.Equal(t, ReasonPrometheusNoData, sink.latest(t).Reason)
		assert.True(t, c.Blind())
	})
}
