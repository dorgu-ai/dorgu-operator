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
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
)

// mockLLMClient implements llm.Client for testing.
type mockLLMClient struct {
	response *llm.DiagnosisResponse
	err      error
	calls    int
}

func (m *mockLLMClient) EnhanceDiagnosis(_ context.Context, _ llm.DiagnosisRequest) (*llm.DiagnosisResponse, error) {
	m.calls++
	return m.response, m.err
}

func (m *mockLLMClient) Provider() string { return "mock" }

func TestAIProvider_Name(t *testing.T) {
	provider := NewAIProvider(&mockLLMClient{}, logr.Discard())
	if provider.Name() != "ai-enhanced" {
		t.Errorf("name = %q, want %q", provider.Name(), "ai-enhanced")
	}
}

func TestAIProvider_Diagnose_EnhancesRuleBasedSummary(t *testing.T) {
	mock := &mockLLMClient{
		response: &llm.DiagnosisResponse{
			EnhancedSummary:   "Memory limit of 256Mi is insufficient. The workload peaks at ~280Mi during startup. Increase to 512Mi.",
			RecommendedAction: "resource-adjustment",
		},
	}

	provider := NewAIProvider(mock, logr.Discard())

	signals := []detection.Signal{
		{
			Type:     detection.SignalOOMKilled,
			Severity: detection.SeverityCritical,
			Category: detection.CategoryResource,
			Source:   "pod-collector",
			Message:  "Container killed due to OOM",
			Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "api-server-xyz", Namespace: "default"},
		},
	}

	diagnoses, err := provider.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) == 0 {
		t.Fatal("expected at least one diagnosis")
	}
	if diagnoses[0].Provider != "ai-enhanced" {
		t.Errorf("provider = %q, want %q", diagnoses[0].Provider, "ai-enhanced")
	}
	if diagnoses[0].Summary != "Memory limit of 256Mi is insufficient. The workload peaks at ~280Mi during startup. Increase to 512Mi." {
		t.Errorf("summary not enhanced: %q", diagnoses[0].Summary)
	}
	if mock.calls == 0 {
		t.Error("LLM client was not called")
	}
}

func TestAIProvider_Diagnose_GracefulDegradation(t *testing.T) {
	mock := &mockLLMClient{
		err: fmt.Errorf("API timeout"),
	}

	provider := NewAIProvider(mock, logr.Discard())

	signals := []detection.Signal{
		{
			Type:     detection.SignalOOMKilled,
			Severity: detection.SeverityCritical,
			Category: detection.CategoryResource,
			Source:   "pod-collector",
			Message:  "Container killed due to OOM",
			Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "api-server-xyz", Namespace: "default"},
		},
	}

	diagnoses, err := provider.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("expected graceful degradation, got error: %v", err)
	}
	if len(diagnoses) != 0 {
		t.Errorf("expected empty diagnoses on LLM failure, got %d", len(diagnoses))
	}
}

func TestAIProvider_Diagnose_NoSignals(t *testing.T) {
	mock := &mockLLMClient{}
	provider := NewAIProvider(mock, logr.Discard())

	diagnoses, err := provider.Diagnose(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diagnoses != nil {
		t.Errorf("expected nil for no signals, got %d diagnoses", len(diagnoses))
	}
	if mock.calls != 0 {
		t.Error("LLM should not be called with no signals")
	}
}

func TestAIProvider_ConfidenceAdjustment_Clamped(t *testing.T) {
	tests := []struct {
		name           string
		baseConfidence float64
		adjustment     float64
		want           float64
	}{
		{
			name:           "positive adjustment within range",
			baseConfidence: 0.70,
			adjustment:     0.10,
			want:           0.80,
		},
		{
			name:           "clamped to 1.0",
			baseConfidence: 0.95,
			adjustment:     0.20,
			want:           1.0,
		},
		{
			name:           "clamped to 0.0",
			baseConfidence: 0.10,
			adjustment:     -0.50,
			want:           0.0,
		},
		{
			name:           "negative adjustment within range",
			baseConfidence: 0.80,
			adjustment:     -0.10,
			want:           0.70,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := Diagnosis{
				Summary:         "test",
				Confidence:      tt.baseConfidence,
				Category:        "resource",
				Severity:        detection.SeverityCritical,
				SuggestedAction: "investigate",
				DiagnosedAt:     time.Now(),
			}
			resp := &llm.DiagnosisResponse{
				ConfidenceAdjustment: tt.adjustment,
			}

			result := enhanceDiagnosis(base, resp)

			// Allow floating point imprecision.
			if diff := result.Confidence - tt.want; diff > 0.001 || diff < -0.001 {
				t.Errorf("confidence = %.4f, want %.4f", result.Confidence, tt.want)
			}
		})
	}
}

func TestBuildLLMRequest(t *testing.T) {
	value := 92.0
	d := Diagnosis{
		Summary:         "OOM kill detected",
		Category:        "resource",
		Severity:        detection.SeverityCritical,
		Confidence:      0.85,
		SuggestedAction: "resource-adjustment",
		Contributing: []ContributingSignal{
			{
				Signal: detection.Signal{
					Type:    detection.SignalOOMKilled,
					Message: "Container killed",
					Resource: dorguv1.ResourceReference{
						Kind:      "Pod",
						Name:      "api-server",
						Namespace: "default",
					},
					Value: &value,
				},
				Detail: "OOM killed",
			},
		},
	}

	req := buildLLMRequest(d, nil)

	if req.Summary != "OOM kill detected" {
		t.Errorf("summary = %q", req.Summary)
	}
	if req.Category != "resource" {
		t.Errorf("category = %q", req.Category)
	}
	if len(req.Signals) != 1 {
		t.Fatalf("signals count = %d, want 1", len(req.Signals))
	}
	if req.Signals[0].Resource != "Pod/default/api-server" {
		t.Errorf("resource = %q, want Pod/default/api-server", req.Signals[0].Resource)
	}
	if req.Signals[0].Value != "92.0%" {
		t.Errorf("value = %q, want 92.0%%", req.Signals[0].Value)
	}
}
