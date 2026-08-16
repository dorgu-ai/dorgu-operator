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

package llm

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Supported provider names, as accepted by NewClient and returned by Provider().
const (
	providerClaude = "claude"
	providerGemini = "gemini"
)

// Client provides LLM capabilities for diagnosis enhancement.
type Client interface {
	// EnhanceDiagnosis takes rule-based diagnosis context and returns enriched explanation.
	EnhanceDiagnosis(ctx context.Context, req DiagnosisRequest) (*DiagnosisResponse, error)
	// Provider returns the provider name (e.g., "claude", "gemini").
	Provider() string
}

// DiagnosisRequest contains context for AI-enhanced diagnosis.
type DiagnosisRequest struct {
	// RuleBasedDiagnosis is the output from the deterministic rule engine.
	Summary         string
	Category        string
	Severity        string
	Confidence      float64
	SuggestedAction string

	// Signal context
	Signals []SignalContext

	// Incident history (for pattern recognition)
	PreviousOccurrences int32
	PreviousResolutions []string
}

// SignalContext provides signal details for LLM context.
type SignalContext struct {
	Type     string
	Message  string
	Resource string // "Pod/api-server-xyz", "Node/worker-1"
	Value    string // "92%", "248Mi"
}

// DiagnosisResponse contains AI-enhanced diagnosis.
type DiagnosisResponse struct {
	// EnhancedSummary replaces the rule-based summary with richer explanation.
	EnhancedSummary string
	// RecommendedAction may refine or confirm the rule-based suggestion.
	RecommendedAction string
	// ConfidenceAdjustment: positive = increase, negative = decrease (AI's assessment).
	ConfidenceAdjustment float64
}

const defaultTimeout = 30 * time.Second

// NewClient creates an LLM client based on the provider name and API key.
func NewClient(provider, apiKey string) (Client, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("API key is required for LLM provider %q", provider)
	}

	httpClient := &http.Client{Timeout: defaultTimeout}

	switch provider {
	case providerClaude:
		return &ClaudeClient{
			apiKey: apiKey,
			model:  defaultClaudeModel,
			client: httpClient,
		}, nil
	case providerGemini:
		return &GeminiClient{
			apiKey: apiKey,
			model:  defaultGeminiModel,
			client: httpClient,
		}, nil
	default:
		return nil, fmt.Errorf("unknown LLM provider %q (supported: claude, gemini)", provider)
	}
}
