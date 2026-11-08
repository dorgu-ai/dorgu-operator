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
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

const providerNameAI = "ai-enhanced"

// AIProvider enhances rule-based diagnoses with LLM-powered explanations.
//
// It re-runs the rule-based logic to get base diagnoses, then asks the model to
// explain each. What it takes from the model is the structured summary, and
// nothing else: SuggestedAction, category, severity, confidence and with them the
// deduplication key all stay the rules' (KPI I5). A reply that fails the schema
// is counted, discarded, and the rule-based diagnosis is kept as it is.
//
// A diagnosis that touches a protected namespace (see NamespaceGate) is never
// sent: it is left out of the result, so the rule-based diagnosis stands.
//
// The model is asked once per signal fingerprint (see Fingerprint). While a
// diagnosis keeps the same fingerprint the previous answer is reused, until the
// cache TTL runs out.
type AIProvider struct {
	llmClient    llm.Client
	gate         NamespaceGate
	ruleProvider *RuleBasedProvider
	cache        *llm.Cache[cachedAnswer]
	logger       logr.Logger
}

// cachedAnswer is what a fingerprint last got from the model: a validated reply,
// or the fact that asking failed (so a down provider is not retried every cycle).
type cachedAnswer struct {
	resp    *llm.DiagnosisResponse
	invalid bool // the model replied but failed the schema
	failed  bool // the request itself failed
}

// AIOption configures an AIProvider.
type AIOption func(*AIProvider)

// WithCacheTTL sets how long an answer is reused while the signal fingerprint is
// unchanged. Zero or negative disables the cache: the model is asked every cycle.
func WithCacheTTL(ttl time.Duration) AIOption {
	return func(p *AIProvider) { p.cache = llm.NewCache[cachedAnswer](ttl, 0) }
}

// NewAIProvider creates an AI-enhanced diagnosis provider. The gate is the
// deny-list; with a nil gate nothing is sent, because an unavailable deny-list
// fails closed.
func NewAIProvider(client llm.Client, gate NamespaceGate, logger logr.Logger, opts ...AIOption) *AIProvider {
	p := &AIProvider{
		llmClient:    client,
		gate:         gate,
		ruleProvider: NewRuleBasedProvider(logger),
		cache:        llm.NewCache[cachedAnswer](llm.DefaultCacheTTL, 0),
		logger:       logger.WithName("ai-diagnosis"),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Gate returns the deny-list this provider asks before every model call. It is
// exposed so wiring tests can assert the provider is gated by the same checker
// as the rest of the pipeline.
func (p *AIProvider) Gate() NamespaceGate { return p.gate }

// Name returns the provider identifier.
func (p *AIProvider) Name() string { return providerNameAI }

// Diagnose runs rule-based diagnosis first, then enhances each result with the
// model's structured explanation. A diagnosis the model cannot or may not help
// with is left to the rule-based provider.
func (p *AIProvider) Diagnose(ctx context.Context, signals []detection.Signal) ([]Diagnosis, error) {
	// Get rule-based diagnoses as the foundation.
	baseDiagnoses, err := p.ruleProvider.Diagnose(ctx, signals)
	if err != nil {
		return nil, fmt.Errorf("rule-based diagnosis failed: %w", err)
	}
	if len(baseDiagnoses) == 0 {
		return nil, nil
	}

	opts := CallOptionsFrom(ctx)
	if opts.NoModel {
		p.logger.V(1).Info("not sending the diagnosis to the model: the caller withheld this group")
		return nil, nil
	}

	enhanced := make([]Diagnosis, 0, len(baseDiagnoses))
	for _, d := range baseDiagnoses {
		if ns, protected := ProtectedNamespace(ctx, p.gate, d); protected {
			p.logger.V(1).Info("not sending the diagnosis to the model: protected or unknown namespace",
				"namespace", ns,
				"category", d.Category,
			)
			continue
		}

		if result, ok := p.enhance(ctx, d, opts); ok {
			enhanced = append(enhanced, result)
		}
	}
	return enhanced, nil
}

// enhance returns the diagnosis to use in place of base, and false when the
// rule-based provider's diagnosis should stand on its own.
func (p *AIProvider) enhance(ctx context.Context, base Diagnosis, opts CallOptions) (Diagnosis, bool) {
	fingerprint := Fingerprint(base)

	if answer, hit := p.cache.Get(fingerprint); hit {
		metrics.RecordLLMCache(llm.PurposeDiagnosis, metrics.LLMCacheHit)
		return p.apply(base, answer)
	}
	metrics.RecordLLMCache(llm.PurposeDiagnosis, metrics.LLMCacheMiss)

	var occurrences int32
	if opts.PreviousOccurrences != nil {
		occurrences = opts.PreviousOccurrences(ctx, base)
	}

	resp, err := llm.EnhanceDiagnosis(ctx, p.llmClient, buildLLMRequest(base, occurrences))
	switch {
	case err == nil:
		answer := cachedAnswer{resp: resp}
		p.cache.Set(fingerprint, answer)
		p.logAdvice(base, resp)
		return p.apply(base, answer)

	case errors.Is(err, llm.ErrInvalidOutput):
		p.logger.V(0).Info("model reply failed schema validation, keeping the rule-based diagnosis",
			"error", err,
			"provider", p.llmClient.Provider(),
			"category", base.Category,
		)
		answer := cachedAnswer{invalid: true}
		p.cache.SetFor(fingerprint, answer, llm.FailureTTL)
		return p.apply(base, answer)

	default:
		p.logger.V(0).Info("LLM enhancement failed, keeping the rule-based diagnosis",
			"error", err,
			"provider", p.llmClient.Provider(),
			"category", base.Category,
		)
		answer := cachedAnswer{failed: true}
		p.cache.SetFor(fingerprint, answer, llm.FailureTTL)
		return p.apply(base, answer)
	}
}

// apply turns a (possibly cached) answer into the diagnosis to return.
func (p *AIProvider) apply(base Diagnosis, answer cachedAnswer) (Diagnosis, bool) {
	switch {
	case answer.failed:
		// Nothing from the model: the rule-based provider's diagnosis stands.
		return Diagnosis{}, false
	case answer.invalid:
		// The model answered and the answer is not usable. The rule-based
		// diagnosis is returned unchanged and still labelled rule-based.
		return base, true
	default:
		return enhanceDiagnosis(base, answer.resp), true
	}
}

// logAdvice records what the model recommended next to what the rules decided.
// The recommendation is advisory: it is logged, and nothing acts on it.
func (p *AIProvider) logAdvice(base Diagnosis, resp *llm.DiagnosisResponse) {
	if resp.RecommendedAction == base.SuggestedAction {
		return
	}
	p.logger.V(1).Info("model recommends a different action; the rules' action stands",
		"rulesAction", base.SuggestedAction,
		"modelAction", resp.RecommendedAction,
		"modelConfidence", resp.Confidence,
		"category", base.Category,
	)
}

// buildLLMRequest converts a diagnosis into an LLM request. The contributing
// signals it needs are already carried on the diagnosis.
func buildLLMRequest(d Diagnosis, previousOccurrences int32) llm.DiagnosisRequest {
	signalContexts := make([]llm.SignalContext, 0, len(d.Contributing))
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
		Summary:             d.Summary,
		Category:            d.Category,
		Severity:            string(d.Severity),
		Confidence:          d.Confidence,
		SuggestedAction:     d.SuggestedAction,
		PreviousOccurrences: previousOccurrences,
		Signals:             signalContexts,
	}
}

// enhanceDiagnosis applies the model's validated summary to a base diagnosis.
// Only the summary text is taken. The suggested action, category, severity and
// confidence are the rules', so the deduplication key and everything downstream
// of it are decided without reading the model.
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
	if resp.Summary != "" {
		enhanced.Summary = resp.Summary
	}
	return enhanced
}
