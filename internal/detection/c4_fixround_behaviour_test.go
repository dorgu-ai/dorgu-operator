package detection

import (
	"context"
	"errors"
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

// C4 fix round (review F-6..F-11 on PR 64): behaviour fixes that need no new API.

// ---------------------------------------------------------------------------
// F-7: the count belongs to the pod instance, not its name
// ---------------------------------------------------------------------------

func TestCPUThrottling_SameNamedReplacementPodStartsFromZero(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	old := throttlePod("db-0", map[string]string{"app": "1000m"})
	old.UID = types.UID("uid-old")
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(old).Build()
	metrics := &fakeMetricsSource{present: true}
	metrics.set(map[string]string{"db-0 app": "990m"})
	c := NewCPUThrottlingCollector(k, logr.Discard(), WithMinCycleSpacing(0), WithMetricsSource(metrics))

	collectTypes(t, c)
	collectTypes(t, c)

	// A StatefulSet deletes db-0 and recreates it with the same name and a new UID.
	require.NoError(t, k.Delete(context.Background(), old))
	replacement := throttlePod("db-0", map[string]string{"app": "1000m"})
	replacement.UID = types.UID("uid-new")
	require.NoError(t, k.Create(context.Background(), replacement))

	assert.Empty(t, collectTypes(t, c), "the new instance has been hot for one cycle")
	assert.Empty(t, collectTypes(t, c))
	assert.Len(t, collectTypes(t, c), 1, "three cycles of its own")
}

// ---------------------------------------------------------------------------
// F-9: what a cycle is
// ---------------------------------------------------------------------------

func TestCPUThrottling_AnErrorCycleResetsTheStreak(t *testing.T) {
	metrics := &fakeMetricsSource{present: true}
	metrics.set(map[string]string{"api-1 app": "990m"})
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})}, WithMetricsSource(metrics))

	collectTypes(t, c) // hot 1
	metrics.mu.Lock()
	metrics.listErr = errors.New("503")
	metrics.mu.Unlock()
	_, err := c.Collect(context.Background()) // a cycle that could not look
	require.Error(t, err)
	metrics.mu.Lock()
	metrics.listErr = nil
	metrics.mu.Unlock()

	assert.Empty(t, collectTypes(t, c), "hot, error, hot is not two consecutive cycles")
	assert.Empty(t, collectTypes(t, c))
	assert.Len(t, collectTypes(t, c), 1, "three consecutive readable cycles after the error")
}

func TestCPUThrottling_ACallInsideTheSpacingWindowIsNotACycle(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	metrics := &fakeMetricsSource{present: true}
	metrics.set(map[string]string{"api-1 app": "990m"})
	c := newThrottleCollector(t, []client.Object{throttlePod("api-1", map[string]string{"app": "1000m"})},
		WithMetricsSource(metrics), WithMinCycleSpacing(30*time.Second), WithCPUClock(func() time.Time { return now }))

	collectTypes(t, c)
	for range 5 { // a verifier hammering the engine inside the window
		now = now.Add(time.Second)
		assert.Empty(t, collectTypes(t, c))
	}
	now = now.Add(time.Minute)
	assert.Empty(t, collectTypes(t, c), "second cycle")
	now = now.Add(time.Minute)
	assert.Len(t, collectTypes(t, c), 1, "third cycle")
}

// ---------------------------------------------------------------------------
// F-11: critical comes from the Prometheus path only
// ---------------------------------------------------------------------------

func TestCPUThrottling_SeverityByPath(t *testing.T) {
	pod := throttlePod("api-1", map[string]string{"app": "1000m"})
	prom := func(ratio float64) []ThrottleSample {
		return []ThrottleSample{{Namespace: throttleNS, Pod: "api-1", Container: "app", Ratio: ratio}}
	}
	tests := []struct {
		name  string
		ratio float64
		want  Severity
	}{
		{"25% is a warning", 0.25, SeverityWarning},
		{"just under half is a warning", 0.4999, SeverityWarning},
		{"50% is critical", 0.5, SeverityCritical},
		{"all periods throttled is critical", 1, SeverityCritical},
	}
	for _, tt := range tests {
		t.Run("prometheus "+tt.name, func(t *testing.T) {
			c := newThrottleCollector(t, []client.Object{pod}, WithThrottleSource(&fakeThrottleSource{samples: prom(tt.ratio)}))
			signals := collectTypes(t, c)
			require.Len(t, signals, 1)
			assert.Equal(t, tt.want, signals[0].Severity)
		})
	}

	t.Run("metrics.k8s.io stays a warning however hot", func(t *testing.T) {
		metrics := &fakeMetricsSource{present: true}
		metrics.set(map[string]string{"api-1 app": "5000m"})
		c := newThrottleCollector(t, []client.Object{pod}, WithMetricsSource(metrics))
		collectTypes(t, c)
		collectTypes(t, c)
		signals := collectTypes(t, c)
		require.Len(t, signals, 1)
		assert.Equal(t, SeverityWarning, signals[0].Severity, "usage at the limit is evidence of throttling, not a measurement of it")
	})
}

// ---------------------------------------------------------------------------
// F-6: the engine's failure throttle is per error text
// ---------------------------------------------------------------------------

type changingError struct {
	mu  sync.Mutex
	err error
}

func (c *changingError) Name() string { return "changing" }
func (c *changingError) Collect(context.Context) ([]Signal, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return nil, c.err
}
func (c *changingError) set(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func TestEngine_ADifferentErrorIsNotHeldBack(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logger := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+args)
	}, funcr.Options{})
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	col := &changingError{err: errors.New("listing pods: timeout")}
	e := NewEngine(logger, col)
	e.now = func() time.Time { return now }

	_, _ = e.CollectAll(context.Background())
	now = now.Add(time.Minute)
	_, _ = e.CollectAll(context.Background())
	col.set(errors.New("listing pods: forbidden"))
	now = now.Add(time.Minute)
	_, _ = e.CollectAll(context.Background())
	now = now.Add(time.Minute)
	_, _ = e.CollectAll(context.Background())

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "timeout")
	assert.Contains(t, joined, "forbidden", "a new cause inside the window is not hidden behind the old one")
	assert.Len(t, lines, 2, "and the repeat of each is still throttled")
}

// ---------------------------------------------------------------------------
// F-8: why Prometheus was not used is visible at the default log level
// ---------------------------------------------------------------------------

func TestCPUThrottling_FallbackCauseIsLoggedAtInfoOncePerInterval(t *testing.T) {
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
	metrics := &fakeMetricsSource{present: true}
	prom := &fakeThrottleSource{err: errors.New("prometheus response is over the 4194304 byte limit")}
	c := NewCPUThrottlingCollector(fake.NewClientBuilder().WithScheme(scheme).Build(), logger,
		WithMinCycleSpacing(0), WithCPUClock(func() time.Time { return now }),
		WithMetricsSource(metrics), WithThrottleSource(prom))

	for range 10 {
		_, err := c.Collect(context.Background())
		require.NoError(t, err)
		now = now.Add(time.Minute)
	}
	mu.Lock()
	defer mu.Unlock()
	var cause []string
	for _, l := range lines {
		if strings.Contains(l, "4194304") {
			cause = append(cause, l)
		}
	}
	assert.NotEmpty(t, cause, "at the default verbosity somebody can see why the true signal never arrives")
	assert.LessOrEqual(t, len(cause), 3, "but not once per cycle")
}
