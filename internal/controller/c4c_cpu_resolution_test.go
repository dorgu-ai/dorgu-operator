package controller

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// C4c (review of PR 69): F-1 (the collect-time snapshot is pinned through runCycle) and
// F-2 (the warm-up is per container, not only per source).

// flippingCollector is a CPU-family collector that can look. Each Collect leaves it
// resolution-blind (when startBlind is set), and warmUpAfterCollect turns it warm: it
// stands for a verifier cycle that finishes warming the source between the reconciler's
// collect and its resolve step. The snapshot taken right after CollectAll is the only
// thing that still remembers it was blind.
type flippingCollector struct {
	mu         sync.Mutex
	startBlind bool
	blind      bool
	collected  bool
}

func (*flippingCollector) Name() string { return "flipping-cpu" }
func (*flippingCollector) Families() []detection.Family {
	return []detection.Family{detection.FamilyCPUThrottling}
}
func (*flippingCollector) Blind() bool { return false }

func (f *flippingCollector) Collect(context.Context) ([]detection.Signal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collected, f.blind = true, f.startBlind
	return nil, nil
}

func (f *flippingCollector) ResolutionBlind() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blind
}

func (f *flippingCollector) warmUpAfterCollect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.collected {
		f.blind = false
	}
}

// warmsOnTheFirstIncidentListAfterCollect makes the reconciler's resolve step (the first
// thing that lists IncidentMemories once the signals are collected) find the collector
// warm. runCycle reaches no incident List between CollectAll and the snapshot.
func warmsOnTheFirstIncidentListAfterCollect(t *testing.T, r *HealthCheckReconciler, c client.Client, f *flippingCollector) {
	t.Helper()
	wc, ok := c.(client.WithWatch)
	require.True(t, ok)
	r.Client = interceptor.NewClient(wc, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*dorguv1.IncidentMemoryList); ok {
				f.warmUpAfterCollect()
			}
			return cl.List(ctx, list, opts...)
		},
	})
}

func TestC4cF1_TheBlindnessSnapshotHoldsAnIncidentThroughRunCycle(t *testing.T) {
	ctx := context.Background()
	im := cpuIncidentFor()
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), limitedCheckoutPod())
	f := &flippingCollector{startBlind: true}
	r.Detection = detection.NewEngine(logr.Discard(), f)
	warmsOnTheFirstIncidentListAfterCollect(t, r, c, f)

	r.runCycle(ctx)

	assert.False(t, f.ResolutionBlind(), "the collector is warm by the time the resolve step asks")
	assert.Equal(t, PhaseDetected, phaseOf(t, c, im),
		"the source was blind when the signals were collected: a recovery between collect and resolve is not evidence")
}

// The same setup with a collector that is never blind resolves the incident, so the test
// above holds the incident because of the snapshot and for no other reason.
func TestC4cF1_ControlTheSameIncidentResolvesWhenTheSourceWasNeverBlind(t *testing.T) {
	ctx := context.Background()
	im := cpuIncidentFor()
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), limitedCheckoutPod())
	f := &flippingCollector{startBlind: false}
	r.Detection = detection.NewEngine(logr.Discard(), f)
	warmsOnTheFirstIncidentListAfterCollect(t, r, c, f)

	r.runCycle(ctx)

	assert.Equal(t, PhaseResolved, phaseOf(t, c, im))
}

// scriptedMetrics is a metrics.k8s.io whose answer a test sets between cycles: container
// usage keyed by "pod/container", all in the platform namespace.
type scriptedMetrics struct {
	mu    sync.Mutex
	usage map[string]string
}

func (*scriptedMetrics) Available(context.Context) (bool, error) { return true, nil }

func (s *scriptedMetrics) set(usage map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = usage
}

func (s *scriptedMetrics) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]metricsv1beta1.PodMetrics, 0, len(s.usage))
	for key, usage := range s.usage {
		var pod, container string
		for i := range key {
			if key[i] == '/' {
				pod, container = key[:i], key[i+1:]
				break
			}
		}
		out = append(out, metricsv1beta1.PodMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: "platform"},
			Containers: []metricsv1beta1.ContainerMetrics{{
				Name: container, Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(usage)},
			}},
		})
	}
	return out, nil
}

const (
	checkoutPodName = "checkout-57c95bf9b8-47vp9"
	otherPodName    = "other-6c9d8b7f5-abcde"
)

func otherLimitedPod() *corev1.Pod {
	pod := basePod("platform", otherPodName, "other")
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}
	return pod
}

// cpuHarness is a reconciler over the review's cluster: platform/checkout (limit 500m, Ready
// for an hour, an open CPUThrottling incident last seen six minutes ago) next to an unrelated
// limited pod, a real collector reading a scripted metrics.k8s.io, and a clock the test drives.
type cpuHarness struct {
	t         *testing.T
	r         *HealthCheckReconciler
	c         client.Client
	im        *dorguv1.IncidentMemory
	metrics   *scriptedMetrics
	collector *detection.CPUThrottlingCollector
	engine    *detection.Engine
	now       time.Time
	interval  time.Duration
}

func newCPUHarness(t *testing.T, interval time.Duration) *cpuHarness {
	t.Helper()
	im := cpuIncidentFor()
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), deploymentFor("platform", "other", 1),
		limitedCheckoutPod(), otherLimitedPod())
	h := &cpuHarness{t: t, r: r, c: c, im: im, metrics: &scriptedMetrics{}, interval: interval,
		now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	h.collector = detection.NewCPUThrottlingCollector(c, logr.Discard(),
		detection.WithMetricsSource(h.metrics),
		detection.WithMinCycleSpacing(interval/2),
		detection.WithCPUClock(func() time.Time { return h.now }))
	h.engine = detection.NewEngine(logr.Discard(), h.collector)
	r.Detection = h.engine
	return h
}

// collect is a cycle that only reads: the signals are not processed, as when the cluster
// has already been through the cycles that raised them.
func (h *cpuHarness) collect() {
	h.t.Helper()
	h.now = h.now.Add(h.interval)
	_, err := h.engine.CollectAll(context.Background())
	require.NoError(h.t, err)
}

// cycle is one full reconciler cycle at the next interval.
func (h *cpuHarness) cycle() {
	h.t.Helper()
	h.now = h.now.Add(h.interval)
	h.r.runCycle(context.Background())
}

func (h *cpuHarness) phase() string { return phaseOf(h.t, h.c, h.im) }

var (
	quietOther = map[string]string{otherPodName + "/other": "50m"}
	checkoutAt = checkoutPodName + "/checkout"
)

func with(usage map[string]string, extra map[string]string) map[string]string {
	out := map[string]string{}
	maps.Copy(out, usage)
	maps.Copy(out, extra)
	return out
}

// The reviewer's probe for F-2: the source is warm (three good cycles on an unrelated pod),
// then the pinned checkout pod appears in it for the first time. Its streak is 1, the grace
// period has long passed, the pod is Ready: the incident used to resolve while the container
// was pinned at 500m of 500m. The intervals are the default, the 2.5 minutes the review gave
// as the point where the hole opens, and one longer than the grace period itself.
func TestC4cF2_APinnedContainerNewToAWarmSourceHoldsTheIncident(t *testing.T) {
	for _, interval := range []time.Duration{time.Minute, 150 * time.Second, 6 * time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			h := newCPUHarness(t, interval)
			h.metrics.set(quietOther)
			h.cycle()
			h.cycle()
			assert.Equal(t, PhaseDetected, h.phase(), "the source is still warming up")

			h.metrics.set(with(quietOther, map[string]string{checkoutAt: "500m"}))
			h.cycle() // third good cycle: the source is warm; checkout has a streak of 1
			assert.Equal(t, PhaseDetected, h.phase(), "pinned at its limit and new to the source: streak 1 of 3")
			h.cycle() // streak 2
			assert.Equal(t, PhaseDetected, h.phase(), "streak 2 of 3")
		})
	}
}

// The container that was throttled drops out of the source for longer than the grace period
// (metrics-server lost the pod, a partial answer) and comes back still pinned. Nothing in
// the source said it was quiet while it was away, and it reads as quiet for the first two
// cycles after it is back.
func TestC4cF2_AThrottledContainerAbsentFromTheSourceForOverFiveMinutesHoldsTheIncident(t *testing.T) {
	h := newCPUHarness(t, 6*time.Minute)
	h.metrics.set(with(quietOther, map[string]string{checkoutAt: "500m"}))
	for range detection.CPUUsageConsecutiveCycles {
		h.collect() // the signal is raised, and the collector remembers the container
	}

	h.metrics.set(quietOther) // checkout is gone from the answer; the source as a whole is warm
	h.cycle()
	assert.Equal(t, PhaseDetected, h.phase(), "6 minutes with no reading of a container that was throttled")
	h.cycle()
	assert.Equal(t, PhaseDetected, h.phase(), "12 minutes")

	h.metrics.set(with(quietOther, map[string]string{checkoutAt: "500m"}))
	h.cycle()
	assert.Equal(t, PhaseDetected, h.phase(), "back and pinned: streak 1")
	h.cycle()
	assert.Equal(t, PhaseDetected, h.phase(), "streak 2")
}

// A container that was throttled and is read as quiet three times in a row after it came
// back is quiet: the hold ends and the incident resolves on the usual evidence.
func TestC4cF2_TheHoldEndsAfterThreeQuietReadings(t *testing.T) {
	h := newCPUHarness(t, time.Minute)
	h.metrics.set(with(quietOther, map[string]string{checkoutAt: "500m"}))
	for range detection.CPUUsageConsecutiveCycles {
		h.collect()
	}
	h.metrics.set(quietOther)
	h.cycle()
	require.Equal(t, PhaseDetected, h.phase())

	h.metrics.set(with(quietOther, map[string]string{checkoutAt: "40m"}))
	h.cycle()
	require.Equal(t, PhaseDetected, h.phase(), "read once since it came back")
	h.cycle()
	require.Equal(t, PhaseDetected, h.phase(), "read twice")
	h.cycle()
	assert.Equal(t, PhaseResolved, h.phase(), "read quiet three times in a row")
}

// Another workload's hot container does not hold this workload's incident.
func TestC4cF2_AnotherWorkloadsHotContainerDoesNotHoldTheIncident(t *testing.T) {
	h := newCPUHarness(t, time.Minute)
	// The other workload is hot on the first and third cycles and quiet on the second, so it
	// is pending (streak 1) when the source turns warm and never raises a signal.
	hotOther := map[string]string{otherPodName + "/other": "500m", checkoutAt: "40m"}
	quiet := map[string]string{otherPodName + "/other": "50m", checkoutAt: "40m"}
	h.metrics.set(hotOther)
	h.cycle()
	h.metrics.set(quiet)
	h.cycle()
	require.Equal(t, PhaseDetected, h.phase(), "warming up")
	h.metrics.set(hotOther)
	h.cycle()
	assert.Equal(t, PhaseResolved, h.phase(),
		"checkout has been read quiet and the hot container belongs to the other workload")
}

// The family scoping of F-5 holds for pending containers too: a hot CPU container does not
// hold a crash-loop incident of the same workload.
func TestC4cF2_ACPUHotContainerDoesNotHoldAnotherSignalsIncident(t *testing.T) {
	h := newCPUHarness(t, time.Minute)
	crash := openIncident("platform", "checkout", "im-platform-checkout-crash")
	require.NoError(t, h.c.Create(context.Background(), crash))
	h.metrics.set(with(quietOther, map[string]string{checkoutAt: "500m"}))

	h.cycle()

	assert.Equal(t, PhaseResolved, phaseOf(t, h.c, crash), "a crash-loop incident asks nothing of the CPU source")
	assert.Equal(t, PhaseDetected, h.phase(), "the CPU incident is held")
}
