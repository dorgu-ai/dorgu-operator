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

package remediation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// C4: the rule-based proposal for CPU throttling mirrors the memory/OOM one. It raises
// the CPU limit of the container, sized off the LIVE limit, and the gate (not this
// package) decides whether the plan may stand.

func newCPUThrottlingDiagnosis(severity detection.Severity) diagnosis.Diagnosis {
	const personaName = "my-app"
	namespace := defaultNamespace
	return diagnosis.Diagnosis{
		Summary:    "CPU throttling detected",
		Confidence: 0.80,
		Provider:   "rule-based",
		Category:   "resource",
		Severity:   severity,
		PersonaRef: &dorguv1.PersonaReference{Kind: kindApplicationPersona, Name: personaName, Namespace: namespace},
		Contributing: []diagnosis.ContributingSignal{{
			Signal: detection.Signal{
				Type: detection.SignalCPUThrottling, Severity: severity, Resource: podIn(namespace), DetectedAt: time.Now(),
				Metadata: map[string]string{"container": "app", "source": "metrics.k8s.io"},
			},
			Detail: "container is at its CPU limit",
		}},
		SuggestedAction: "resource-adjustment",
		DiagnosedAt:     time.Now(),
	}
}

func TestDiagnosisTargetPath_CPUThrottlingIsTheCPULimit(t *testing.T) {
	diag := newCPUThrottlingDiagnosis(detection.SeverityWarning)
	assert.Equal(t, gate.ResourcePathCPU, diagnosisTargetPath(diag),
		"one throttled container, one CPU-limit fix: the gate dedups on this path")
}

func TestProposer_CPUThrottling_RaisesTheLiveCPULimit(t *testing.T) {
	tests := []struct {
		name       string
		severity   detection.Severity
		liveLimit  string
		personaCPU string
		want       string
	}{
		{"warning is +25% of the live limit", detection.SeverityWarning, "400m", "50m", "500m"},
		{"critical is +50%", detection.SeverityCritical, "400m", "50m", "600m"},
		{"whole cores", detection.SeverityWarning, "2", "1", "2500m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			persona := personaWithLimits("my-app", tt.personaCPU, "256Mi")
			deploy := liveDeployment("my-app",
				map[corev1.ResourceName]string{corev1.ResourceCPU: tt.liveLimit, corev1.ResourceMemory: "256Mi"}, nil)
			incident := newTestIncident(defaultNamespace, "cpu-throttle", "my-app", "CPUThrottling")
			c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
				WithRuntimeObjects(persona, deploy).
				WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

			proposer := NewProposer(c, gate.New(c, testLogger()), testLogger())
			result, err := proposer.Propose(context.Background(),
				newCPUThrottlingDiagnosis(tt.severity), incident)

			require.NoError(t, err)
			require.True(t, result.Proposed, "skip: %s", result.SkipReason)
			assert.Equal(t, dorguv1.PlanSourceRuleBased, result.Action.Spec.PlanSource)
			assert.True(t, result.Action.Spec.Approval.Required)
			assert.Equal(t, tt.want, patchLimit(t, planned(result.Action).Patch.Raw, "cpu"))
			assert.Empty(t, patchLimit(t, planned(result.Action).Patch.Raw, "memory"), "the memory limit is not touched")
			assert.Contains(t, result.Action.Spec.Explanation, tt.liveLimit, "the live limit is quoted")
			assert.Contains(t, result.Action.Spec.Explanation, "CPUThrottling")
			require.NotNil(t, result.Decision, "the proposal is the gate's decision, not the proposer's")
		})
	}
}

func TestProposer_CPUThrottling_NeverIntroducesACPULimit(t *testing.T) {
	persona := personaWithLimits("my-app", "500m", "256Mi")
	deploy := liveDeployment("my-app", map[corev1.ResourceName]string{corev1.ResourceMemory: "256Mi"}, nil)
	incident := newTestIncident(defaultNamespace, "cpu-throttle", "my-app", "CPUThrottling")
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	proposer := NewProposer(c, gate.New(c, testLogger()), testLogger())
	result, err := proposer.Propose(context.Background(),
		newCPUThrottlingDiagnosis(detection.SeverityWarning), incident)
	require.NoError(t, err)
	assert.False(t, result.Proposed)
	assert.Equal(t, SkipNoLiveLimit, result.Reason)
}

func TestProposer_CPUThrottling_ObserveModeProposesNothing(t *testing.T) {
	persona := personaWithLimits("my-app", "500m", "256Mi")
	deploy := liveDeployment("my-app", map[corev1.ResourceName]string{corev1.ResourceCPU: "500m"}, nil)
	cp := clusterPersonaExcluding()
	cp.Spec.Policies.SelfHealing.Mode = dorguv1.SelfHealingModeObserve
	incident := newTestIncident(defaultNamespace, "cpu-throttle", "my-app", "CPUThrottling")
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy, cp).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	proposer := NewProposer(c, gate.New(c, testLogger()), testLogger())
	result, err := proposer.Propose(context.Background(),
		newCPUThrottlingDiagnosis(detection.SeverityWarning), incident)
	require.NoError(t, err)
	assert.False(t, result.Proposed, "the gate's mode stage rules, with no check of the proposer's own")
}

func TestProposer_CPUThrottling_ProtectedNamespaceProposesNothing(t *testing.T) {
	persona := personaWithLimits("my-app", "500m", "256Mi")
	deploy := liveDeployment("my-app", map[corev1.ResourceName]string{corev1.ResourceCPU: "500m"}, nil)
	incident := newTestIncident(defaultNamespace, "cpu-throttle", "my-app", "CPUThrottling")
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	proposer := NewProposer(c, gate.New(c, testLogger(), gate.WithProtectedNamespaces(defaultNamespace)), testLogger())
	result, err := proposer.Propose(context.Background(),
		newCPUThrottlingDiagnosis(detection.SeverityWarning), incident)
	require.NoError(t, err)
	assert.False(t, result.Proposed)
}

func TestProposer_CPUThrottling_SecondIncidentSameFixIsDeduped(t *testing.T) {
	persona := personaWithLimits("my-app", "500m", "256Mi")
	deploy := liveDeployment("my-app", map[corev1.ResourceName]string{corev1.ResourceCPU: "500m"}, nil)
	first := newTestIncident(defaultNamespace, "cpu-a", "my-app", "CPUThrottling")
	second := newTestIncident(defaultNamespace, "cpu-b", "my-app", "CPUThrottling")
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	proposer := NewProposer(c, gate.New(c, testLogger()), testLogger())
	diag := newCPUThrottlingDiagnosis(detection.SeverityWarning)

	r1, err := proposer.Propose(context.Background(), diag, first)
	require.NoError(t, err)
	require.True(t, r1.Proposed)
	r2, err := proposer.Propose(context.Background(), diag, second)
	require.NoError(t, err)
	assert.False(t, r2.Proposed, "one throttled container is one CPU-limit proposal")
}
