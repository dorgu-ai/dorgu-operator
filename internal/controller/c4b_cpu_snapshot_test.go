package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// A verifier-like cycle between the collect and the resolve must not flip the answer:
// blindness is captured once, right after the collection the resolution is based on.
func TestC4bN1_BlindnessAtCollectTimeHoldsEvenIfTheSourceRecoversBeforeResolve(t *testing.T) {
	ctx := context.Background()
	im := cpuIncidentFor()
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), limitedCheckoutPod())

	collector := detection.NewCPUThrottlingCollector(c, logr.Discard(), detection.WithMinCycleSpacing(0)) // no source
	engine := detection.NewEngine(logr.Discard(), collector)
	r.Detection = engine
	_, err := engine.CollectAll(ctx)
	require.NoError(t, err)
	blindAtCollect := engine.ResolutionBlindFamilies()
	require.NotEmpty(t, blindAtCollect)

	// The source comes back (and is warm) before the resolve step runs.
	warm := detection.NewCPUThrottlingCollector(c, logr.Discard(), detection.WithMinCycleSpacing(0),
		detection.WithMetricsSource(quietMetrics{present: true}))
	r.Detection = detection.NewEngine(logr.Discard(), warm)
	for range detection.CPUUsageConsecutiveCycles {
		_, err = r.Detection.CollectAll(ctx)
		require.NoError(t, err)
	}

	require.NoError(t, r.resolveClearedHolding(ctx, map[string]bool{}, blindAtCollect))
	assert.Equal(t, PhaseDetected, phaseOf(t, c, im), "it was blind when the signals were collected")
}
