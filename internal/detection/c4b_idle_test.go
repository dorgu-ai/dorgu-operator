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
	assert.Equal(t, ThrottleSample{Namespace: "shop", Pod: "api-1", Container: "app", Ratio: 0}, samples[0])
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

	t.Run("NaN for every limited container is idle", func(t *testing.T) {
		sink, c, err := idle(t, "NaN")
		require.NoError(t, err, "with no metrics-server this used to fail the collector")
		st := sink.latest(t)
		assert.True(t, st.Active)
		assert.Equal(t, CPUThrottlingSourcePrometheus, st.Source)
		assert.Equal(t, ReasonPrometheusActive, st.Reason, "not PrometheusNoData: cAdvisor is being scraped")
		assert.False(t, c.Blind())
		assert.False(t, c.ResolutionBlind(), "Prometheus's ratio is a five-minute rate already; idle is a real answer")
	})

	t.Run("garbage for every container is still no data", func(t *testing.T) {
		sink, c, err := idle(t, "+Inf")
		require.Error(t, err)
		assert.Equal(t, ReasonPrometheusNoData, sink.latest(t).Reason)
		assert.True(t, c.Blind())
	})
}
