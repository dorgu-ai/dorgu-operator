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
	"time"

	"github.com/go-logr/logr"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
)

const providerNameAI = "ai-enhanced"

// AIProvider enhances rule-based diagnoses with LLM-powered explanations.
// It re-runs the rule-based logic to get base diagnoses, then enhances each
// with an LLM call. On any LLM failure, it returns an empty slice so
// the rule-based diagnoses from the primary provider are used as-is.
type AIProvider struct {
	llmClient    llm.Client
	ruleProvider *RuleBasedProvider
	logger       logr.Logger
}

// NewAIProvider creates an AI-enhanced diagnosis provider.
func NewAIProvider(client llm.Client, logger logr.Logger) *AIProvider {
	return &AIProvider{
		llmClient:    client,
		ruleProvider: NewRuleBasedProvider(logger),
		logger:       logger.WithName("ai-diagnosis"),
	}
}

// Name returns the provider identifier.
func (p *AIProvider) Name() string { return providerNameAI }

// Diagnose runs rule-based diagnosis first, then enhances each result with LLM context.
// Returns enhanced diagnoses on success, or an empty slice on any LLM failure.
func (p *AIProvider) Diagnose(ctx context.Context, signals []detection.Signal) ([]Diagnosis, error) {
	// Get rule-based diagnoses as the foundation.
	baseDiagnoses, err := p.ruleProvider.Diagnose(ctx, signals)
	if err != nil {
		return nil, fmt.Errorf("rule-based diagnosis failed: %w", err)
	}
	if len(baseDiagnoses) == 0 {
		return nil, nil
	}

	enhanced := make([]Diagnosis, 0, len(baseDiagnoses))

	for _, d := range baseDiagnoses {
		req := buildLLMRequest(d, signals)

		resp, err := p.llmClient.EnhanceDiagnosis(ctx, req)
		if err != nil {
			p.logger.V(0).Info("LLM enhancement failed, skipping AI diagnoses",
				"error", err,
				"provider", p.llmClient.Provider(),
				"category", d.Category,
			)
			// Graceful degradation: return empty so rule-based results are used.
			return nil, nil
		}

		enhancedDiag := enhanceDiagnosis(d, resp)
		enhanced = append(enhanced, enhancedDiag)
	}

	return enhanced, nil
}

// buildLLMRequest converts a diagnosis and its signals into an LLM request.
func buildLLMRequest(d Diagnosis, signals []detection.Signal) llm.DiagnosisRequest {
	var signalContexts []llm.SignalContext
	for _, cs := range d.Contributing {
		sc := llm.SignalContext{
			Type:    string(cs.Signal.Type),
			Message: cs.Signal.Message,
		}
		if cs.Signal.Resource.Kind != "" {
			sc.Resource = fmt.Sprintf("%s/%s", cs.Signal.Resource.Kind, cs.Signal.Resource.Name)
			if cs.Signal.Resource.Namespace != "" {
				sc.Resource = fmt.Sprintf("%s/%s/%s", cs.Signal.Resource.Kind, cs.Signal.Resource.Namespace, cs.Signal.Resource.Name)
			}
		}
		if cs.Signal.Value != nil {
			sc.Value = fmt.Sprintf("%.1f%%", *cs.Signal.Value)
		}
		signalContexts = append(signalContexts, sc)
	}

	return llm.DiagnosisRequest{
		Summary:         d.Summary,
		Category:        d.Category,
		Severity:        string(d.Severity),
		Confidence:      d.Confidence,
		SuggestedAction: d.SuggestedAction,
		Signals:         signalContexts,
	}
}

// enhanceDiagnosis applies the LLM response to a base diagnosis.
func enhanceDiagnosis(base Diagnosis, resp *llm.DiagnosisResponse) Diagnosis {
	enhanced := Diagnosis{
		Summary:           base.Summary,
		Confidence:        base.Confidence,
		Provider:          providerNameAI,
		Category:          base.Category,
		Severity:          base.Severity,
		PersonaRef:        base.PersonaRef,
		AffectedResources: base.AffectedResources,
		Contributing:      base.Contributing,
		SuggestedAction:   base.SuggestedAction,
		DiagnosedAt:       time.Now(),
	}

	if resp.EnhancedSummary != "" {
		enhanced.Summary = resp.EnhancedSummary
	}
	if resp.RecommendedAction != "" {
		enhanced.SuggestedAction = resp.RecommendedAction
	}

	// Apply confidence adjustment, clamped to [0.0, 1.0].
	adjusted := enhanced.Confidence + resp.ConfidenceAdjustment
	if adjusted > 1.0 {
		adjusted = 1.0
	}
	if adjusted < 0.0 {
		adjusted = 0.0
	}
	enhanced.Confidence = adjusted

	return enhanced
}
