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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

func newTestPersona(namespace, name, memoryLimit, cpuLimit string) *dorguv1.ApplicationPersona {
	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: name,
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{
					Memory: memoryLimit,
					CPU:    cpuLimit,
				},
			},
		},
	}
}

func newTestIncident(namespace, name, personaName, signal string) *dorguv1.IncidentMemory {
	return &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "im-" + name,
			Namespace: namespace,
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      personaName,
				Namespace: namespace,
			},
			Category: "resource",
			Severity: "critical",
			Detection: dorguv1.DetectionInfo{
				Signal: signal,
			},
		},
		Status: dorguv1.IncidentMemoryStatus{
			Phase: "Detected",
		},
	}
}

func newOOMDiagnosis(namespace, personaName string, severity detection.Severity) diagnosis.Diagnosis {
	return diagnosis.Diagnosis{
		Summary:    "OOMKilled detected",
		Confidence: 0.85,
		Provider:   "rule-based",
		Category:   "resource",
		Severity:   severity,
		PersonaRef: &dorguv1.PersonaReference{
			Kind:      "ApplicationPersona",
			Name:      personaName,
			Namespace: namespace,
		},
		Contributing: []diagnosis.ContributingSignal{
			{
				Signal: detection.Signal{
					Type:       detection.SignalOOMKilled,
					Severity:   severity,
					DetectedAt: time.Now(),
				},
				Detail: "container killed due to OOM",
			},
		},
		SuggestedAction: "resource-adjustment",
		DiagnosedAt:     time.Now(),
	}
}

func TestProposer_OOM_MemoryIncrease_Warning(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom-test", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := newOOMDiagnosis("default", "my-app", detection.SeverityWarning)
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.True(t, result.Proposed)
	assert.NotNil(t, result.Action)
	assert.Equal(t, "persona-update", result.Action.Spec.Action.Type)
	assert.True(t, result.Action.Spec.Approval.Required, "approval must be required")

	// Verify memory was increased by 50% (warning).
	var patch map[string]interface{}
	require.NoError(t, json.Unmarshal(result.Action.Spec.Action.Patch.Raw, &patch))
	spec := patch["spec"].(map[string]interface{})
	resources := spec["resources"].(map[string]interface{})
	limits := resources["limits"].(map[string]interface{})
	assert.Equal(t, "384Mi", limits["memory"])
}

func TestProposer_OOM_MemoryIncrease_Critical(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom-crit", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := newOOMDiagnosis("default", "my-app", detection.SeverityCritical)
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.True(t, result.Proposed)

	// Verify memory was increased by 100% (critical) = 512Mi.
	var patch map[string]interface{}
	require.NoError(t, json.Unmarshal(result.Action.Spec.Action.Patch.Raw, &patch))
	spec := patch["spec"].(map[string]interface{})
	resources := spec["resources"].(map[string]interface{})
	limits := resources["limits"].(map[string]interface{})
	assert.Equal(t, "512Mi", limits["memory"])
}

func TestProposer_CPUSaturation(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "cpu-test", "my-app", "CPUSaturationHigh")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := diagnosis.Diagnosis{
		Summary:    "CPU saturation detected",
		Confidence: 0.80,
		Provider:   "rule-based",
		Category:   "resource",
		Severity:   detection.SeverityWarning,
		PersonaRef: &dorguv1.PersonaReference{
			Kind:      "ApplicationPersona",
			Name:      "my-app",
			Namespace: "default",
		},
		Contributing: []diagnosis.ContributingSignal{
			{
				Signal: detection.Signal{
					Type:       detection.SignalCPUSaturationHigh,
					Severity:   detection.SeverityWarning,
					DetectedAt: time.Now(),
				},
				Detail: "CPU saturation high",
			},
		},
		SuggestedAction: "resource-adjustment",
		DiagnosedAt:     time.Now(),
	}

	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.True(t, result.Proposed)

	// Verify CPU was increased by 25% (warning): 500m → 625m.
	var patch map[string]interface{}
	require.NoError(t, json.Unmarshal(result.Action.Spec.Action.Patch.Raw, &patch))
	spec := patch["spec"].(map[string]interface{})
	resources := spec["resources"].(map[string]interface{})
	limits := resources["limits"].(map[string]interface{})
	assert.Equal(t, "625m", limits["cpu"])
}

func TestProposer_BlastRadiusCap(t *testing.T) {
	// OOM critical = 2x multiplier, which is at the blast radius cap.
	// This should still be allowed since 2x == max.
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "blast-test", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := newOOMDiagnosis("default", "my-app", detection.SeverityCritical)
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.True(t, result.Proposed, "2x increase should be at the cap but allowed")
}

func TestProposer_RateLimitBlocks(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "rate-test", "my-app", "OOMKilled")

	// Create 5 existing actions.
	var objects []runtime.Object
	objects = append(objects, persona)
	for i := range 5 {
		ra := &dorguv1.RemediationAction{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "ra-rate-" + string(rune('a'+i)),
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
				Labels: map[string]string{
					"dorgu.io/persona-kind": "ApplicationPersona",
					"dorgu.io/persona-name": "my-app",
				},
			},
			Spec: dorguv1.RemediationActionSpec{
				PersonaRef: dorguv1.PersonaReference{
					Kind: "ApplicationPersona", Name: "my-app", Namespace: "default",
				},
				Action:     dorguv1.RemediationActionDetail{Type: "persona-update"},
				Confidence: "0.85",
			},
			Status: dorguv1.RemediationActionStatus{Phase: "Completed"},
		}
		objects = append(objects, ra)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(objects...).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := newOOMDiagnosis("default", "my-app", detection.SeverityCritical)
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.False(t, result.Proposed)
	assert.Contains(t, result.SkipReason, "safety check failed")
	assert.Contains(t, result.SkipReason, "rate-limit")
}

func TestProposer_DenyListBlocks(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("kube-system", "coredns", "256Mi", "500m")
	incident := newTestIncident("kube-system", "deny-test", "coredns", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := newOOMDiagnosis("kube-system", "coredns", detection.SeverityCritical)
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.False(t, result.Proposed)
	assert.Contains(t, result.SkipReason, "deny-list")
}

func TestProposer_CrashLoopWithOOM_ProposesMemoryIncrease(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "crashloop-oom", "my-app", "CrashLoopBackOff")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := diagnosis.Diagnosis{
		Summary:    "CrashLoopBackOff with OOM correlation",
		Confidence: 0.75,
		Provider:   "rule-based",
		Category:   "health",
		Severity:   detection.SeverityCritical,
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: "my-app", Namespace: "default",
		},
		Contributing: []diagnosis.ContributingSignal{
			{
				Signal: detection.Signal{Type: detection.SignalCrashLoopBackOff, Severity: detection.SeverityCritical, DetectedAt: time.Now()},
				Detail: "pod in CrashLoopBackOff",
			},
			{
				Signal: detection.Signal{Type: detection.SignalOOMKilled, Severity: detection.SeverityCritical, DetectedAt: time.Now()},
				Detail: "OOM killed before crash",
			},
		},
		SuggestedAction: "resource-adjustment",
		DiagnosedAt:     time.Now(),
	}

	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.True(t, result.Proposed)

	var patch map[string]interface{}
	require.NoError(t, json.Unmarshal(result.Action.Spec.Action.Patch.Raw, &patch))
	spec := patch["spec"].(map[string]interface{})
	resources := spec["resources"].(map[string]interface{})
	limits := resources["limits"].(map[string]interface{})
	assert.Contains(t, limits, "memory", "should propose memory increase for crashloop+OOM")
}

func TestProposer_CrashLoopWithoutOOM_Skips(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "crashloop-nooom", "my-app", "CrashLoopBackOff")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := diagnosis.Diagnosis{
		Summary:    "CrashLoopBackOff without OOM",
		Confidence: 0.60,
		Provider:   "rule-based",
		Category:   "health",
		Severity:   detection.SeverityWarning,
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: "my-app", Namespace: "default",
		},
		Contributing: []diagnosis.ContributingSignal{
			{
				Signal: detection.Signal{Type: detection.SignalCrashLoopBackOff, Severity: detection.SeverityWarning, DetectedAt: time.Now()},
				Detail: "pod in CrashLoopBackOff",
			},
		},
		SuggestedAction: "resource-adjustment",
		DiagnosedAt:     time.Now(),
	}

	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.False(t, result.Proposed)
	assert.Equal(t, "no applicable resource adjustment", result.SkipReason)
}

func TestProposer_UnknownActionType_Skips(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := diagnosis.Diagnosis{
		Summary:    "Unknown issue",
		Confidence: 0.50,
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: "my-app", Namespace: "default",
		},
		SuggestedAction: "investigate",
		DiagnosedAt:     time.Now(),
	}

	incident := newTestIncident("default", "unknown", "my-app", "Unknown")
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.False(t, result.Proposed)
	assert.Contains(t, result.SkipReason, "unsupported action type")
}

func TestProposer_RestartAction_Skips(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := diagnosis.Diagnosis{
		Summary:    "Needs restart",
		Confidence: 0.70,
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: "my-app", Namespace: "default",
		},
		SuggestedAction: "restart",
		DiagnosedAt:     time.Now(),
	}

	incident := newTestIncident("default", "restart", "my-app", "ProbeFailure")
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.False(t, result.Proposed)
	assert.Contains(t, result.SkipReason, "restart")
}

func TestProposer_NilPersonaRef_Skips(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := diagnosis.Diagnosis{
		Summary:         "No persona",
		SuggestedAction: "resource-adjustment",
	}

	incident := newTestIncident("default", "no-persona", "my-app", "OOMKilled")
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.False(t, result.Proposed)
	assert.Contains(t, result.SkipReason, "no persona reference")
}

func TestProposer_PrePatchStateCaptured(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "prepatch-test", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := newOOMDiagnosis("default", "my-app", detection.SeverityWarning)
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	require.True(t, result.Proposed)
	require.NotNil(t, result.Action.Spec.Action.PrePatchState)

	var prePatch map[string]interface{}
	require.NoError(t, json.Unmarshal(result.Action.Spec.Action.PrePatchState.Raw, &prePatch))
	spec := prePatch["spec"].(map[string]interface{})
	resources := spec["resources"].(map[string]interface{})
	limits := resources["limits"].(map[string]interface{})
	assert.Equal(t, "256Mi", limits["memory"])
}

func TestProposer_RollbackSpec(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "rollback-test", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	proposer := NewProposer(c, safety, testLogger())

	diag := newOOMDiagnosis("default", "my-app", detection.SeverityWarning)
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	require.True(t, result.Proposed)
	require.NotNil(t, result.Action.Spec.Rollback)
	assert.True(t, result.Action.Spec.Rollback.Enabled)
	assert.Equal(t, int32(1), result.Action.Spec.Rollback.MaxRetries)
	assert.NotNil(t, result.Action.Spec.Rollback.HealthCheckAfter)
}

func TestGenerateActionName_Format(t *testing.T) {
	incident := newTestIncident("default", "test", "my-app", "OOMKilled")
	name := generateActionName(incident, "resource-adjustment")

	assert.True(t, strings.HasPrefix(name, "ra-"))
	assert.LessOrEqual(t, len(name), maxActionNameLength)
	// Should contain persona name and action type components.
	assert.Contains(t, name, "my-app")
	assert.Contains(t, name, "resource-adjustment")
}

func TestGenerateActionName_MaxLength(t *testing.T) {
	longName := strings.Repeat("a", 200)
	incident := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "im-" + longName,
			Namespace: "default",
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind: "ApplicationPersona",
				Name: longName,
			},
		},
	}

	name := generateActionName(incident, "resource-adjustment")
	assert.LessOrEqual(t, len(name), maxActionNameLength)
}

func TestBuildNestedMap(t *testing.T) {
	result := buildNestedMap("spec", "resources", "limits", "memory", "512Mi")

	expected := map[string]interface{}{
		"spec": map[string]interface{}{
			"resources": map[string]interface{}{
				"limits": map[string]interface{}{
					"memory": "512Mi",
				},
			},
		},
	}

	resultJSON, _ := json.Marshal(result)
	expectedJSON, _ := json.Marshal(expected)
	assert.JSONEq(t, string(expectedJSON), string(resultJSON))
}
