/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package detection

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// C4 (P2-D11): CPU throttling detection. The default signal reads metrics.k8s.io:
// a container whose CPU usage is at or above 95% of its limit for three cycles in a
// row. The optional true signal is the cfs throttled-period ratio from a
// Prometheus-compatible endpoint, at 25% or more. Prometheus wins when both exist.

const throttleNS = "shop"

// fakeMetricsSource is metrics.k8s.io as the collector sees it.
type fakeMetricsSource struct {
	mu       sync.Mutex
	present  bool
	probeErr error
	listErr  error
	usage    map[string]string // "pod/container" -> CPU usage quantity
	calls    int
}

func (f *fakeMetricsSource) Available(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.present, f.probeErr
}

func (f *fakeMetricsSource) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	byPod := map[string]*metricsv1beta1.PodMetrics{}
	var order []string
	for key, usage := range f.usage {
		var pod, container string
		_, err := fmt.Sscanf(key, "%s %s", &pod, &container)
		if err != nil {
			panic(err)
		}
		pm, ok := byPod[pod]
		if !ok {
			pm = &metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: throttleNS}}
			byPod[pod] = pm
			order = append(order, pod)
		}
		pm.Containers = append(pm.Containers, metricsv1beta1.ContainerMetrics{
			Name:  container,
			Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(usage)},
		})
	}
	out := make([]metricsv1beta1.PodMetrics, 0, len(order))
	for _, pod := range order {
		out = append(out, *byPod[pod])
	}
	return out, nil
}

func (f *fakeMetricsSource) set(usage map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = usage
}

// fakeThrottleSource is the Prometheus side.
type fakeThrottleSource struct {
	samples []ThrottleSample
	err     error
	calls   int
}

func (f *fakeThrottleSource) Query(context.Context) ([]ThrottleSample, error) {
	f.calls++
	return f.samples, f.err
}

// statusRecorder is the sink the collector publishes its source state to.
type statusRecorder struct {
	mu   sync.Mutex
	last []SourceStatus
}

func (s *statusRecorder) ReportCPUThrottlingSource(_ context.Context, st SourceStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = append(s.last, st)
}

func (s *statusRecorder) latest(t *testing.T) SourceStatus {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.last, "the collector must publish its source state every cycle")
	return s.last[len(s.last)-1]
}

func throttlePod(name string, containers map[string]string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: throttleNS,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-5d8f7", APIVersion: "apps/v1", UID: "x"}},
		},
	}
	for cname, limit := range containers {
		c := corev1.Container{Name: cname}
		if limit != "" {
			c.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limit)}
		}
		pod.Spec.Containers = append(pod.Spec.Containers, c)
	}
	return pod
}

type excludeNS map[string]bool

func (e excludeNS) NamespaceExcluded(_ context.Context, ns string) (bool, error) { return e[ns], nil }

func newThrottleCollector(t *testing.T, objs []client.Object, opts ...CPUThrottlingOption) *CPUThrottlingCollector {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	base := []CPUThrottlingOption{WithMinCycleSpacing(0)}
	return NewCPUThrottlingCollector(c, logr.Discard(), append(base, opts...)...)
}

func collectTypes(t *testing.T, c *CPUThrottlingCollector) []Signal {
	t.Helper()
	signals, err := c.Collect(context.Background())
	require.NoError(t, err)
	return signals
}

func TestCPUThrottling_Constants(t *testing.T) {
	assert.InDelta(t, 0.95, CPUUsageLimitRatio, 1e-9)
	assert.Equal(t, 3, CPUUsageConsecutiveCycles)
	assert.InDelta(t, 0.25, PrometheusThrottleRatio, 1e-9)
	assert.Equal(t,
		"rate(container_cpu_cfs_throttled_periods_total[5m]) / rate(container_cpu_cfs_periods_total[5m])",
		PrometheusThrottleQuery)
}

func TestCPUThrottling_MetricsServerThreshold(t *testing.T) {
	tests := []struct {
		name      string
		limit     string
		usage     []string // usage per cycle
		wantFires []bool   // whether the signal is present after each cycle
	}{
		{"below 95% never fires", "1000m", []string{"949m", "949m", "949m", "949m"}, []bool{false, false, false, false}},
		{"exactly 95% fires on the third cycle", "1000m", []string{"950m", "950m", "950m"}, []bool{false, false, true}},
		{"stays fired while it holds", "1000m", []string{"990m", "990m", "990m", "990m"}, []bool{false, false, true, true}},
		{"two cycles is not enough", "1000m", []string{"1000m", "1000m"}, []bool{false, false}},
		{"a dip resets the count", "1000m", []string{"990m", "990m", "100m", "990m", "990m"}, []bool{false, false, false, false, false}},
		{"over the limit counts", "500m", []string{"600m", "600m", "600m"}, []bool{false, false, true}},
		{"no CPU limit never counts", "", []string{"4000m", "4000m", "4000m", "4000m"}, []bool{false, false, false, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := &fakeMetricsSource{present: true}
			c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": tt.limit})},
				WithMetricsSource(metrics))
			for i, usage := range tt.usage {
				metrics.set(map[string]string{"api-1 app": usage})
				signals := collectTypes(t, c)
				assert.Equal(t, tt.wantFires[i], len(signals) == 1, "cycle %d (usage %s)", i+1, usage)
			}
		})
	}
}

func TestCPUThrottling_SignalShape(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsSource(metrics))
	metrics.set(map[string]string{"api-1 app": "980m"})
	collectTypes(t, c)
	collectTypes(t, c)
	signals := collectTypes(t, c)
	require.Len(t, signals, 1)

	s := signals[0]
	assert.Equal(t, SignalCPUThrottling, s.Type)
	assert.Equal(t, SeverityWarning, s.Severity)
	assert.Equal(t, CategoryResource, s.Category)
	assert.Equal(t, "Pod", s.Resource.Kind)
	assert.Equal(t, "api-1", s.Resource.Name)
	assert.Equal(t, throttleNS, s.Resource.Namespace)
	assert.Equal(t, "app", s.Metadata["container"], "identity is application + container + family")
	assert.Equal(t, "api", s.Metadata[MetadataKeyDeployment])
	assert.Equal(t, CPUThrottlingSourceMetricsServer, s.Metadata["source"])
	assert.Equal(t, "1", s.Metadata["cpuLimit"])
	require.NotNil(t, s.Value)
	assert.InDelta(t, 98.0, *s.Value, 0.01)
	require.NotNil(t, s.Threshold)
	assert.InDelta(t, 95.0, *s.Threshold, 0.01)
}

func TestCPUThrottling_ContainersAreCountedSeparately(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m", "sidecar": "1000m"})},
		WithMetricsSource(metrics))
	metrics.set(map[string]string{"api-1 app": "990m", "api-1 sidecar": "100m"})
	collectTypes(t, c)
	collectTypes(t, c)
	signals := collectTypes(t, c)
	require.Len(t, signals, 1)
	assert.Equal(t, "app", signals[0].Metadata["container"])
}

func TestCPUThrottling_ReplacedPodStartsFromZero(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	c := newThrottleCollector(t, []client.Object{
		throttlePod("api-1", map[string]string{"app": "1000m"}),
		throttlePod("api-2", map[string]string{"app": "1000m"}),
	}, WithMetricsSource(metrics))

	metrics.set(map[string]string{"api-1 app": "990m"})
	collectTypes(t, c)
	collectTypes(t, c)

	// api-1 is gone and its replacement shows up hot: the count is per pod.
	metrics.set(map[string]string{"api-2 app": "990m"})
	assert.Empty(t, collectTypes(t, c), "a new pod has been hot for one cycle only")

	// And a pod that vanishes for a cycle loses its count.
	metrics.set(map[string]string{})
	collectTypes(t, c)
	metrics.set(map[string]string{"api-2 app": "990m"})
	assert.Empty(t, collectTypes(t, c))
	assert.Empty(t, collectTypes(t, c))
	assert.Len(t, collectTypes(t, c), 1)
}

func TestCPUThrottling_ExcludedNamespaceRaisesNothing(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsSource(metrics), WithCPUNamespaceFilter(excludeNS{throttleNS: true}))
	metrics.set(map[string]string{"api-1 app": "1000m"})
	for range 4 {
		assert.Empty(t, collectTypes(t, c))
	}
}

func TestCPUThrottling_UnreadableDenyListIsAFailure(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsSource(metrics), WithCPUNamespaceFilter(failingFilter{}))
	metrics.set(map[string]string{"api-1 app": "1000m"})
	_, err := c.Collect(context.Background())
	require.Error(t, err, "a collector that cannot tell what it may skip must say so, not guess")
}

type failingFilter struct{}

func (failingFilter) NamespaceExcluded(context.Context, string) (bool, error) {
	return false, errors.New("cannot read ClusterPersona")
}

// A second Collect call inside one cycle (the verifier runs detection too) must not
// count as another cycle, or two verifications would fire a signal a minute early.
func TestCPUThrottling_CallsInsideOneCycleCountOnce(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	metrics := &fakeMetricsSource{present: true}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsSource(metrics),
		WithMinCycleSpacing(30*time.Second),
		WithCPUClock(func() time.Time { return now }))
	metrics.set(map[string]string{"api-1 app": "990m"})

	for range 10 {
		assert.Empty(t, collectTypes(t, c), "ten calls at the same instant are one cycle")
	}
	now = now.Add(60 * time.Second)
	assert.Empty(t, collectTypes(t, c), "second cycle")
	now = now.Add(60 * time.Second)
	assert.Len(t, collectTypes(t, c), 1, "third cycle fires")
	// A verifier-style call right after returns what the cycle saw.
	assert.Len(t, collectTypes(t, c), 1)
}

func TestCPUThrottling_PrometheusSignal(t *testing.T) {
	pod := throttlePod("api-1", map[string]string{"app": "1000m", "nolimit": ""})
	tests := []struct {
		name    string
		samples []ThrottleSample
		want    int
	}{
		{"fires at 25%", []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.25}}, 1},
		{"fires well above", []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.9}}, 1},
		{"below 25% does not", []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.2499}}, 0},
		{"a ratio above 1 is not a ratio", []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 7}}, 0},
		{"negative is ignored", []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: -1}}, 0},
		{"unknown pod is ignored", []ThrottleSample{{Namespace: throttleNS, Pod: "ghost", Container: "app", Ratio: 0.9}}, 0},
		{"another namespace's pod is ignored", []ThrottleSample{{Namespace: "other", Pod: "api-1", Container: "app", Ratio: 0.9}}, 0},
		{"unknown container is ignored", []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "ghost", Ratio: 0.9}}, 0},
		{"a container with no CPU limit cannot be throttled", []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "nolimit", Ratio: 0.9}}, 0},
		{"duplicate series count once", []ThrottleSample{
			{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.9},
			{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.8},
		}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prom := &fakeThrottleSource{samples: tt.samples}
			c := newThrottleCollector(t, []client.Object{pod}, WithThrottleSource(prom))
			signals := collectTypes(t, c)
			assert.Len(t, signals, tt.want)
			for _, s := range signals {
				assert.Equal(t, SignalCPUThrottling, s.Type)
				assert.Equal(t, CPUThrottlingSourcePrometheus, s.Metadata["source"])
				assert.Equal(t, "app", s.Metadata["container"])
			}
		})
	}
}

func TestCPUThrottling_PrometheusFiresImmediately(t *testing.T) {
	// The ratio is already a five-minute rate, so it needs no three-cycle wait.
	prom := &fakeThrottleSource{samples: []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.5}}}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})}, WithThrottleSource(prom))
	signals := collectTypes(t, c)
	require.Len(t, signals, 1)
	require.NotNil(t, signals[0].Value)
	assert.InDelta(t, 50.0, *signals[0].Value, 0.01)
	require.NotNil(t, signals[0].Threshold)
	assert.InDelta(t, 25.0, *signals[0].Threshold, 0.01)
}

func TestCPUThrottling_PrometheusWinsWhenBothArePresent(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(map[string]string{"api-1 app": "1000m"}) // hot by usage
	prom := &fakeThrottleSource{samples: nil}            // not throttled by the true signal
	sink := &statusRecorder{}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsSource(metrics), WithThrottleSource(prom), WithCPUStatusSink(sink))

	for range 5 {
		assert.Empty(t, collectTypes(t, c), "the true throttling ratio says no, and it outranks usage")
	}
	assert.Zero(t, metrics.calls, "metrics.k8s.io is not read while Prometheus answers")
	st := sink.latest(t)
	assert.True(t, st.Active)
	assert.Equal(t, CPUThrottlingSourcePrometheus, st.Source)
}

func TestCPUThrottling_FallsBackWhenPrometheusIsDown(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(map[string]string{"api-1 app": "990m"})
	prom := &fakeThrottleSource{err: errors.New("connection refused")}
	sink := &statusRecorder{}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsSource(metrics), WithThrottleSource(prom), WithCPUStatusSink(sink))

	collectTypes(t, c)
	collectTypes(t, c)
	signals, err := c.Collect(context.Background())
	require.NoError(t, err, "detection still sees through metrics.k8s.io, so this cycle is not blind")
	require.Len(t, signals, 1)
	assert.Equal(t, CPUThrottlingSourceMetricsServer, signals[0].Metadata["source"])

	st := sink.latest(t)
	assert.True(t, st.Active)
	assert.Equal(t, CPUThrottlingSourceMetricsServer, st.Source)
	assert.Equal(t, ReasonPrometheusUnreachable, st.Reason, "the fallback is announced, not silent")
}

func TestCPUThrottling_NoSourceAtAllIsOffNotAnError(t *testing.T) {
	sink := &statusRecorder{}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsSource(&fakeMetricsSource{present: false}), WithCPUStatusSink(sink))

	signals, err := c.Collect(context.Background())
	require.NoError(t, err, "no metrics API is a missing capability, which must not blind verification everywhere")
	assert.Empty(t, signals)

	st := sink.latest(t)
	assert.False(t, st.Active)
	assert.Equal(t, ReasonNoMetricsSource, st.Reason)
	assert.Contains(t, st.Message, "metrics.k8s.io")
	assert.Contains(t, st.Message, "spec.metrics.prometheus.url")
}

func TestCPUThrottling_NoSourcesConfiguredAtAll(t *testing.T) {
	sink := &statusRecorder{}
	c := newThrottleCollector(t, nil, WithCPUStatusSink(sink))
	signals, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.Empty(t, signals)
	assert.False(t, sink.latest(t).Active)
	assert.Equal(t, ReasonNoMetricsSource, sink.latest(t).Reason)
}

func TestCPUThrottling_PrometheusNotConfiguredFallsToMetricsServer(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	sink := &statusRecorder{}
	c := newThrottleCollector(t, nil,
		WithMetricsSource(metrics), WithThrottleSource(&fakeThrottleSource{err: ErrPrometheusNotConfigured}), WithCPUStatusSink(sink))
	_, err := c.Collect(context.Background())
	require.NoError(t, err)
	st := sink.latest(t)
	assert.True(t, st.Active)
	assert.Equal(t, CPUThrottlingSourceMetricsServer, st.Source)
	assert.Equal(t, ReasonMetricsServerActive, st.Reason, "not configured is not a fault")
}

// B2b: a collector that could not look must never read as healthy.
func TestCPUThrottling_FailuresSurface(t *testing.T) {
	pod := throttlePod("api-1", map[string]string{"app": "1000m"})
	tests := []struct {
		name  string
		opts  func() []CPUThrottlingOption
		setup func(c *CPUThrottlingCollector)
	}{
		{"metrics API listing fails", func() []CPUThrottlingOption {
			return []CPUThrottlingOption{WithMetricsSource(&fakeMetricsSource{present: true, listErr: errors.New("503")})}
		}, nil},
		{"metrics API discovery fails", func() []CPUThrottlingOption {
			return []CPUThrottlingOption{WithMetricsSource(&fakeMetricsSource{probeErr: errors.New("discovery timeout")})}
		}, nil},
		{"Prometheus down and no metrics-server", func() []CPUThrottlingOption {
			return []CPUThrottlingOption{
				WithMetricsSource(&fakeMetricsSource{present: false}),
				WithThrottleSource(&fakeThrottleSource{err: errors.New("connection refused")}),
			}
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &statusRecorder{}
			c := newThrottleCollector(t, []client.Object{pod}, append(tt.opts(), WithCPUStatusSink(sink))...)
			_, err := c.Collect(context.Background())
			require.Error(t, err)
			assert.False(t, sink.latest(t).Active, "a failing source is reported as off, with its reason")
			assert.NotEmpty(t, sink.latest(t).Reason)
		})
	}
}

func TestCPUThrottling_PodListFailureSurfaces(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("apiserver unavailable")
		},
	}).Build()
	col := NewCPUThrottlingCollector(c, logr.Discard(), WithMinCycleSpacing(0),
		WithMetricsSource(&fakeMetricsSource{present: true}))
	_, err := col.Collect(context.Background())
	require.Error(t, err)
}

// A metrics API that was there and is gone is a collector that cannot look. Absence
// from the start is a missing capability; disappearance mid-flight would otherwise let
// a verification of a CPU fix read Healthy because nobody was watching.
func TestCPUThrottling_SourceLostAfterBeingSeenIsAFailure(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	sink := &statusRecorder{}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsSource(metrics), WithCPUStatusSink(sink))
	_, err := c.Collect(context.Background())
	require.NoError(t, err)

	metrics.mu.Lock()
	metrics.present = false
	metrics.mu.Unlock()
	_, err = c.Collect(context.Background())
	require.Error(t, err)
	st := sink.latest(t)
	assert.False(t, st.Active)
	assert.Equal(t, ReasonMetricsSourceLost, st.Reason)
}

// No error spam: a source that stays broken logs once, then at most once per interval.
func TestCPUThrottling_FailureLogIsRateLimited(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logger := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+args)
	}, funcr.Options{})

	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := NewCPUThrottlingCollector(fake.NewClientBuilder().WithScheme(scheme).Build(), logger,
		WithMinCycleSpacing(0),
		WithCPUClock(func() time.Time { return now }),
		WithMetricsSource(&fakeMetricsSource{present: true, listErr: errors.New("503")}))

	for range 20 {
		_, err := c.Collect(context.Background())
		require.Error(t, err, "every cycle still reports the failure to the engine")
		now = now.Add(time.Minute)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.LessOrEqual(t, len(lines), 5, "20 failing minutes must not log 20 lines: %v", lines)
	assert.NotEmpty(t, lines, "but the first failure must be loud")
}

func TestCPUThrottling_OffStateLogsOncePerInterval(t *testing.T) {
	var mu sync.Mutex
	count := 0
	logger := funcr.New(func(_, _ string) {
		mu.Lock()
		defer mu.Unlock()
		count++
	}, funcr.Options{})
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := NewCPUThrottlingCollector(fake.NewClientBuilder().WithScheme(scheme).Build(), logger,
		WithMinCycleSpacing(0), WithCPUClock(func() time.Time { return now }),
		WithMetricsSource(&fakeMetricsSource{present: false}))
	for range 50 {
		_, err := c.Collect(context.Background())
		require.NoError(t, err)
		now = now.Add(time.Minute)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, count, 1, "the off state is announced")
	assert.LessOrEqual(t, count, 12, "and not every cycle")
}

func TestCPUThrottling_ImplementsSignalCollector(t *testing.T) {
	var _ SignalCollector = (*CPUThrottlingCollector)(nil)
	c := newThrottleCollector(t, nil)
	assert.Equal(t, "cpu-throttling-checker", c.Name())
}

// --enable-metrics-server=false (chart: metricsServer.enabled=false) turns the
// metrics.k8s.io source off. Detection then runs on Prometheus alone, or says why not.
func TestCPUThrottling_MetricsServerDisabledByFlagSaysSo(t *testing.T) {
	sink := &statusRecorder{}
	metrics := &fakeMetricsSource{present: true}
	c := newThrottleCollector(t, nil, WithMetricsSource(metrics), WithMetricsServerDisabled(), WithCPUStatusSink(sink))
	signals, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.Empty(t, signals)
	assert.Zero(t, metrics.calls, "a disabled source is not read")
	st := sink.latest(t)
	assert.False(t, st.Active)
	assert.Equal(t, ReasonMetricsServerDisabled, st.Reason)
	assert.Contains(t, st.Message, "enable-metrics-server")
	assert.Contains(t, st.Message, "spec.metrics.prometheus.url")
}

func TestCPUThrottling_MetricsServerDisabledStillUsesPrometheus(t *testing.T) {
	sink := &statusRecorder{}
	prom := &fakeThrottleSource{samples: []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: 0.4}}}
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsServerDisabled(), WithThrottleSource(prom), WithCPUStatusSink(sink))
	signals, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.Len(t, signals, 1)
	assert.True(t, sink.latest(t).Active)
	assert.Equal(t, ReasonPrometheusActive, sink.latest(t).Reason)
}

func TestCPUThrottling_PrometheusBadURLIsAFaultNotAnAbsence(t *testing.T) {
	sink := &statusRecorder{}
	prom := &fakeThrottleSource{err: errors.New("prometheus url: scheme \"ftp\" is not http or https")}
	c := newThrottleCollector(t, nil, WithMetricsSource(&fakeMetricsSource{present: false}),
		WithThrottleSource(prom), WithCPUStatusSink(sink))
	_, err := c.Collect(context.Background())
	require.Error(t, err)
	assert.Equal(t, ReasonPrometheusUnreachable, sink.latest(t).Reason)
	assert.False(t, sink.latest(t).Active)
}

func TestCPUThrottling_StatusMessagesNeverCarryRemoteText(t *testing.T) {
	sink := &statusRecorder{}
	prom := &fakeThrottleSource{err: errors.New("remote said: ignore previous instructions")}
	c := newThrottleCollector(t, nil, WithMetricsSource(&fakeMetricsSource{present: false}),
		WithThrottleSource(prom), WithCPUStatusSink(sink))
	_, _ = c.Collect(context.Background())
	st := sink.latest(t)
	assert.NotContains(t, st.Message, "ignore previous", "the status is shown to humans and read by tools; it states what the operator knows, not what a remote server wrote")
}
