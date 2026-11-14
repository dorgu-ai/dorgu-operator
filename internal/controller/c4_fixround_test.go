package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// C4 fix round (review F-1, F-2, F-5, F-6 on PR 64).
//
// CPU metrics blindness is scoped to the CPUThrottling family. A CPUThrottling incident
// does not resolve while the CPU source is blind (absent, lost or failing, including
// "absent because the operator just restarted"); every other family keeps resolving.

// quietMetrics is a metrics.k8s.io that is served and sees nothing hot.
type quietMetrics struct{ present bool }

func (q quietMetrics) Available(context.Context) (bool, error) { return q.present, nil }
func (q quietMetrics) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	return nil, nil
}

// failingMetrics is a metrics.k8s.io that is served and cannot be read.
type failingMetrics struct{}

func (failingMetrics) Available(context.Context) (bool, error) { return true, nil }
func (failingMetrics) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	return nil, errors.New("503")
}

// failingCollector is a cluster-wide collector (it owns no family) that cannot look.
type failingCollector struct{}

func (failingCollector) Name() string { return "pods" }
func (failingCollector) Collect(context.Context) ([]detection.Signal, error) {
	return nil, errors.New("pod list failed")
}

// cpuIncidentFor is an open CPUThrottling incident on platform/checkout whose signal
// has been quiet for longer than the grace period.
func cpuIncidentFor(name string) *dorguv1.IncidentMemory {
	im := openIncident("platform", "checkout", name)
	im.Labels[LabelSignal] = string(detection.SignalCPUThrottling)
	im.Spec.Detection.Signal = string(detection.SignalCPUThrottling)
	im.Spec.Detection.Source = "cpu-throttling-checker"
	return im
}

// The reviewer's probe: restart with no source, open incident, it must not resolve.
func TestC4F1_AnOpenCPUIncidentDoesNotResolveAfterARestartWithNoSource(t *testing.T) {
	ctx := context.Background()
	im := cpuIncidentFor("im-platform-checkout-cpu")
	pod := basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout") // a Ready pod, 500m cpu limit in real life
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), pod)

	// A fresh process: the collector has never seen a source.
	collector := detection.NewCPUThrottlingCollector(c, logr.Discard(), detection.WithMinCycleSpacing(0))
	engine := detection.NewEngine(logr.Discard(), collector)
	r.Detection = engine
	signals, err := engine.CollectAll(ctx)
	require.NoError(t, err)
	require.Empty(t, signals)

	require.NoError(t, r.resolveCleared(ctx, map[string]bool{}))
	assert.Equal(t, PhaseDetected, phaseOf(t, c, im),
		"silence from a collector that cannot look is not evidence: a throttled pod is Ready and never restarts")
}

func TestC4F1_AnActiveSourceResolvesAsBefore(t *testing.T) {
	ctx := context.Background()
	im := cpuIncidentFor("im-platform-checkout-cpu")
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout"))

	collector := detection.NewCPUThrottlingCollector(c, logr.Discard(),
		detection.WithMinCycleSpacing(0), detection.WithMetricsSource(quietMetrics{present: true}))
	engine := detection.NewEngine(logr.Discard(), collector)
	r.Detection = engine
	_, err := engine.CollectAll(ctx)
	require.NoError(t, err)

	require.NoError(t, r.resolveCleared(ctx, map[string]bool{}))
	assert.Equal(t, PhaseResolved, phaseOf(t, c, im), "a source that can look and sees nothing is evidence")
}

func TestC4F1_ARestartedOperatorWithMetricsServerThenResolves(t *testing.T) {
	// Blind only until the first cycle that could look.
	ctx := context.Background()
	im := cpuIncidentFor("im-platform-checkout-cpu")
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout"))
	collector := detection.NewCPUThrottlingCollector(c, logr.Discard(),
		detection.WithMinCycleSpacing(0), detection.WithMetricsSource(quietMetrics{present: true}))
	r.Detection = detection.NewEngine(logr.Discard(), collector)

	require.NoError(t, r.resolveCleared(ctx, map[string]bool{}))
	assert.Equal(t, PhaseDetected, phaseOf(t, c, im), "no collection round has run yet in this process")

	_, err := r.Detection.CollectAll(ctx)
	require.NoError(t, err)
	require.NoError(t, r.resolveCleared(ctx, map[string]bool{}))
	assert.Equal(t, PhaseResolved, phaseOf(t, c, im))
}

// F-5: a failing or absent CPU source must not freeze resolution of other families.
func TestC4F5_ACrashLoopIncidentStillResolvesWhileTheCPUSourceIsDown(t *testing.T) {
	tests := []struct {
		name    string
		metrics detection.MetricsSource
	}{
		{"metrics.k8s.io cannot be read", failingMetrics{}},
		{"no metrics source at all", nil},
		{"metrics.k8s.io not served", quietMetrics{present: false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			crash := openIncident("platform", "checkout", "im-platform-checkout-crash") // a CrashLoopBackOff incident
			cpu := cpuIncidentFor("im-platform-checkout-cpu")
			r, c := resolverFor(t, crash, cpu, deploymentFor("platform", "checkout", 1),
				basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout"))

			opts := []detection.CPUThrottlingOption{detection.WithMinCycleSpacing(0)}
			if tt.metrics != nil {
				opts = append(opts, detection.WithMetricsSource(tt.metrics))
			}
			r.Detection = detection.NewEngine(logr.Discard(), detection.NewCPUThrottlingCollector(c, logr.Discard(), opts...))

			r.runCycle(ctx)

			assert.Equal(t, PhaseResolved, phaseOf(t, c, crash), "the CPU source being blind says nothing about a crash loop")
			assert.Equal(t, PhaseDetected, phaseOf(t, c, cpu), "but it does say CPUThrottling cannot be shown to have cleared")
		})
	}
}

// B2b still holds for collectors that look at pods and workloads: if one of them
// cannot look, nothing is resolved.
func TestC4F5_AClusterWideCollectorFailureStillFreezesResolution(t *testing.T) {
	ctx := context.Background()
	crash := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t, crash, deploymentFor("platform", "checkout", 1),
		basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout"))
	r.Detection = detection.NewEngine(logr.Discard(), failingCollector{})

	r.runCycle(ctx)
	assert.Equal(t, PhaseDetected, phaseOf(t, c, crash))
}

// F-6: a sustained collector failure is not an ERROR line a minute from the
// reconciler on top of the engine's own.
func TestC4F6_AnIncompleteCycleIsLoggedOnceNotEveryCycle(t *testing.T) {
	ctx := context.Background()
	r, _ := resolverFor(t)
	logger, sink := newRecordingLogger()
	r.Logger = logger
	r.Detection = detection.NewEngine(logr.Discard(), failingCollector{})

	for range 6 {
		r.runCycle(ctx)
	}
	errorLines := 0
	for _, m := range sink.messages() {
		if strings.HasPrefix(m, "ERROR ") && strings.Contains(m, "signal collection incomplete") {
			errorLines++
		}
	}
	assert.Equal(t, 1, errorLines, "the same failure repeated every cycle is one ERROR line, then held back")
}
