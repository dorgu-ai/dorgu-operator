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

package diagnosis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// B9: the probe signals must end in a diagnosis, or detection raises them for
// nothing. Category health, severity warning, action investigate (advice: the
// rules cannot know which probe or which app setting is wrong).

func probeSignal(t detection.SignalType, pod string) detection.Signal {
	return detection.Signal{
		Type: t, Severity: detection.SeverityWarning, Category: detection.CategoryHealth,
		Source:     "pod-failure-detector",
		Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: pod, Namespace: "shop"},
		PersonaRef: &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "api", Namespace: "shop"},
		DetectedAt: time.Now(),
	}
}

func TestProbeRule_ReadinessIsAHealthDiagnosis(t *testing.T) {
	diags, err := newTestProvider().Diagnose(context.Background(),
		[]detection.Signal{probeSignal(detection.SignalReadinessNotReady, "api-1")})
	require.NoError(t, err)
	require.Len(t, diags, 1)
	d := diags[0]
	assert.Equal(t, string(detection.CategoryHealth), d.Category)
	assert.Equal(t, detection.SeverityWarning, d.Severity)
	assert.Equal(t, actionInvestigate, d.SuggestedAction)
	assert.Equal(t, detection.SignalReadinessNotReady, d.Contributing[0].Signal.Type)
	assert.Contains(t, d.Summary, "not been Ready")
	assert.Positive(t, d.Confidence)
	require.NotNil(t, d.PersonaRef)
}

func TestProbeRule_LivenessIsAHealthDiagnosis(t *testing.T) {
	diags, err := newTestProvider().Diagnose(context.Background(),
		[]detection.Signal{probeSignal(detection.SignalLivenessRestarts, "api-1")})
	require.NoError(t, err)
	require.Len(t, diags, 1)
	d := diags[0]
	assert.Equal(t, string(detection.CategoryHealth), d.Category)
	assert.Equal(t, actionInvestigate, d.SuggestedAction)
	assert.Equal(t, detection.SignalLivenessRestarts, d.Contributing[0].Signal.Type)
	assert.Contains(t, d.Summary, "liveness probe")
}

// Two pods of one app, one failing each way: two findings, each filed under its
// own signal, so neither hides the other's incident.
func TestProbeRule_DifferentPodsGetTheirOwnDiagnosis(t *testing.T) {
	diags, err := newTestProvider().Diagnose(context.Background(), []detection.Signal{
		probeSignal(detection.SignalReadinessNotReady, "api-1"),
		probeSignal(detection.SignalLivenessRestarts, "api-2"),
	})
	require.NoError(t, err)
	require.Len(t, diags, 2)
	primary := map[detection.SignalType]bool{}
	for _, d := range diags {
		primary[d.Contributing[0].Signal.Type] = true
	}
	assert.True(t, primary[detection.SignalReadinessNotReady] && primary[detection.SignalLivenessRestarts])
}

func TestProbeRule_SignalClarityIsKnown(t *testing.T) {
	assert.Greater(t, SignalClarity(detection.SignalLivenessRestarts), 0.5)
	assert.Greater(t, SignalClarity(detection.SignalReadinessNotReady), 0.5)
}

func TestProbeRule_NothingToDiagnoseWithoutItsSignal(t *testing.T) {
	assert.Nil(t, diagnoseLivenessRestarts(nil))
	assert.Nil(t, diagnoseReadinessNotReady(nil))
	assert.False(t, matchLivenessRestarts([]detection.Signal{probeSignal(detection.SignalReadinessNotReady, "p")}))
	assert.False(t, matchReadinessNotReady([]detection.Signal{probeSignal(detection.SignalLivenessRestarts, "p")}))
}

// F-4: a restart loop with no kubelet event blaming a probe is a health finding of
// its own, and its summary must not claim a probe failure.
func TestProbeRule_RestartLoopIsNotCalledAProbeFailure(t *testing.T) {
	diags, err := newTestProvider().Diagnose(context.Background(),
		[]detection.Signal{probeSignal(detection.SignalRestartLoop, "api-1")})
	require.NoError(t, err)
	require.Len(t, diags, 1)
	d := diags[0]
	assert.Equal(t, string(detection.CategoryHealth), d.Category)
	assert.Equal(t, actionInvestigate, d.SuggestedAction)
	assert.Equal(t, detection.SignalRestartLoop, d.Contributing[0].Signal.Type)
	assert.NotContains(t, d.Summary, "liveness probe")
	assert.Contains(t, d.Summary, "restart")
	assert.Greater(t, SignalClarity(detection.SignalRestartLoop), 0.5)
}

func TestProbeRule_LivenessSummaryIsNowEvidenceBacked(t *testing.T) {
	diags, err := newTestProvider().Diagnose(context.Background(),
		[]detection.Signal{probeSignal(detection.SignalLivenessRestarts, "api-1")})
	require.NoError(t, err)
	require.Len(t, diags, 1)
	assert.Contains(t, diags[0].Summary, "liveness probe")
	assert.NotContains(t, diags[0].Summary, "most likely", "the kubelet said so; this is no longer a guess")
}

func TestProbeRule_RestartLoopNeedsItsSignal(t *testing.T) {
	assert.Nil(t, diagnoseRestartLoop(nil))
	assert.False(t, matchRestartLoop([]detection.Signal{probeSignal(detection.SignalReadinessNotReady, "p")}))
}
