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

// These tests cover the reason a paid-for AI diagnosis never reached the user
// (F-05). AIProvider re-runs the rule-based logic and then applies the LLM's
// ConfidenceAdjustment, which no response parser ever populates. So the AI
// diagnosis carries the rule-based confidence *exactly*, and the engine's
// "higher confidence wins" merge, being a strict comparison over a list that
// starts with the rule-based provider, discarded it every single time.
package diagnosis

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
)

// oomSignals returns a single critical OOMKilled signal correlated to a persona.
func oomSignals() []detection.Signal {
	return []detection.Signal{
		{
			Type:     detection.SignalOOMKilled,
			Severity: detection.SeverityCritical,
			Category: detection.CategoryResource,
			Source:   "pod-collector",
			Message:  "Container killed due to OOM",
			Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "report-worker-abc", Namespace: "apps"},
			PersonaRef: &dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      "report-worker",
				Namespace: "apps",
			},
			DetectedAt: time.Now(),
		},
	}
}

// The engine must keep the AI-enhanced diagnosis when both providers report the
// same confidence for the same finding. This is the exact production shape: the
// LLM returns no confidence adjustment, so the two confidences are identical.
func TestEngine_EqualConfidence_KeepsEnhancingProvider(t *testing.T) {
	same := []dorguv1.ResourceReference{
		{Kind: "Pod", Name: "report-worker-abc", Namespace: "apps"},
	}
	ruleBased := &mockProvider{
		name: providerNameRuleBased,
		diagnoses: []Diagnosis{{
			Summary:           "Container OOM-killed. Memory limit may be insufficient.",
			Confidence:        0.70,
			Provider:          providerNameRuleBased,
			Category:          "resource",
			SuggestedAction:   actionResourceAdjustment,
			AffectedResources: same,
		}},
	}
	aiEnhanced := &mockProvider{
		name: providerNameAI,
		diagnoses: []Diagnosis{{
			Summary:           "report-worker peaks at ~120M but is limited to 48Mi.",
			Confidence:        0.70,
			Provider:          providerNameAI,
			Category:          "resource",
			SuggestedAction:   actionResourceAdjustment,
			AffectedResources: same,
		}},
	}

	engine := NewEngine(logr.Discard(), ruleBased, aiEnhanced)

	diagnoses, err := engine.Analyze(context.Background(), oomSignals())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) != 1 {
		t.Fatalf("expected 1 deduplicated diagnosis, got %d", len(diagnoses))
	}
	if diagnoses[0].Provider != providerNameAI {
		t.Errorf("provider = %q, want %q: the AI diagnosis was discarded on a confidence tie",
			diagnoses[0].Provider, providerNameAI)
	}
	if diagnoses[0].Summary != "report-worker peaks at ~120M but is limited to 48Mi." {
		t.Errorf("summary = %q, want the AI summary", diagnoses[0].Summary)
	}
}

// A rule-based diagnosis that is genuinely more confident still wins, so the
// tie-break does not become "AI always wins".
func TestEngine_HigherConfidenceWinsOverProviderOrder(t *testing.T) {
	same := []dorguv1.ResourceReference{
		{Kind: "Pod", Name: "report-worker-abc", Namespace: "apps"},
	}
	ruleBased := &mockProvider{
		name: providerNameRuleBased,
		diagnoses: []Diagnosis{{
			Summary:           providerNameRuleBased,
			Confidence:        0.90,
			Provider:          providerNameRuleBased,
			Category:          "resource",
			SuggestedAction:   actionResourceAdjustment,
			AffectedResources: same,
		}},
	}
	aiEnhanced := &mockProvider{
		name: providerNameAI,
		diagnoses: []Diagnosis{{
			Summary:           providerNameAI,
			Confidence:        0.40,
			Provider:          providerNameAI,
			Category:          "resource",
			SuggestedAction:   actionResourceAdjustment,
			AffectedResources: same,
		}},
	}

	engine := NewEngine(logr.Discard(), ruleBased, aiEnhanced)

	diagnoses, err := engine.Analyze(context.Background(), oomSignals())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) != 1 {
		t.Fatalf("expected 1 deduplicated diagnosis, got %d", len(diagnoses))
	}
	if diagnoses[0].Provider != providerNameRuleBased {
		t.Errorf("provider = %q, want %q: the more confident diagnosis must win",
			diagnoses[0].Provider, providerNameRuleBased)
	}
}

// End to end through the real providers: a real RuleBasedProvider plus a real
// AIProvider over a stub LLM that returns what the parsers actually produce (an
// enhanced summary, a recommended action, and no confidence adjustment). The
// surviving diagnosis must be the AI one.
func TestEngine_RealProviders_AIEnhancedSurvives(t *testing.T) {
	mock := &mockLLMClient{
		response: &llm.DiagnosisResponse{
			EnhancedSummary:   "report-worker was OOM-killed: it needs ~120M, the limit is 48Mi.",
			RecommendedAction: actionResourceAdjustment,
			// No ConfidenceAdjustment, exactly as parseEnhancedResponse returns.
		},
	}

	engine := NewEngine(
		logr.Discard(),
		NewRuleBasedProvider(logr.Discard()),
		NewAIProvider(mock, logr.Discard()),
	)

	diagnoses, err := engine.Analyze(context.Background(), oomSignals())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) == 0 {
		t.Fatal("expected at least one diagnosis")
	}
	if mock.calls == 0 {
		t.Fatal("the LLM was never called; the test is not exercising the AI path")
	}

	for _, d := range diagnoses {
		if d.Provider == providerNameRuleBased {
			t.Errorf("a rule-based diagnosis survived alongside its AI enhancement: %q (confidence %.2f)",
				d.Summary, d.Confidence)
		}
	}
	if diagnoses[0].Provider != providerNameAI {
		t.Fatalf("provider = %q, want %q", diagnoses[0].Provider, providerNameAI)
	}
	if diagnoses[0].Summary != mock.response.EnhancedSummary {
		t.Errorf("summary = %q, want the AI summary %q", diagnoses[0].Summary, mock.response.EnhancedSummary)
	}
}
