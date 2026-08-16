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
	"sort"

	"github.com/go-logr/logr"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// Engine runs diagnosis providers and manages the diagnosis lifecycle.
// In Phase 2a, only RuleBasedProvider is used.
// In Phase 2b, an AIProvider can be added that receives rule-based diagnoses as context.
type Engine struct {
	providers []DiagnosisProvider
	logger    logr.Logger
}

// NewEngine creates a diagnosis engine with the given providers.
// Providers are executed in order; later providers may use earlier results as context.
func NewEngine(logger logr.Logger, providers ...DiagnosisProvider) *Engine {
	return &Engine{
		providers: providers,
		logger:    logger,
	}
}

// Analyze runs all providers against the given signals and returns aggregated diagnoses.
// Diagnoses are deduplicated by (category + suggestedAction + affectedResources),
// keeping the highest-confidence variant, and on an exact confidence tie the one
// from the later provider. Diagnoses are sorted by confidence descending.
func (e *Engine) Analyze(ctx context.Context, signals []detection.Signal) ([]Diagnosis, error) {
	if len(signals) == 0 {
		return nil, nil
	}

	var produced []rankedDiagnosis

	for stage, provider := range e.providers {
		diagnoses, err := provider.Diagnose(ctx, signals)
		if err != nil {
			e.logger.Error(err, "diagnosis provider failed", "provider", provider.Name())
			return nil, fmt.Errorf("provider %s failed: %w", provider.Name(), err)
		}
		e.logger.V(1).Info("provider produced diagnoses",
			"provider", provider.Name(),
			"count", len(diagnoses),
		)
		for _, d := range diagnoses {
			produced = append(produced, rankedDiagnosis{diagnosis: d, stage: stage})
		}
	}

	// Deduplicate: when two diagnoses match on (category + suggestedAction + resources),
	// keep the better one and say which one lost.
	deduped := e.deduplicateDiagnoses(produced)

	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].Confidence > deduped[j].Confidence
	})

	return deduped, nil
}

// rankedDiagnosis pairs a diagnosis with the position of the provider that
// produced it. The stage is read from the provider list rather than from
// Diagnosis.Provider, so a provider that mislabels its output cannot change how
// its results are ranked.
type rankedDiagnosis struct {
	diagnosis Diagnosis
	stage     int
}

// deduplicateDiagnoses keeps one diagnosis per deduplication key: the most
// confident, and on an exact tie the one from the later provider.
//
// The tie is the common case, not a corner case. AIProvider re-runs the
// rule-based logic and then applies the LLM's ConfidenceAdjustment, which no
// response parser populates, so an AI-enhanced diagnosis carries the rule-based
// confidence to the digit. Under a strict "higher confidence wins" comparison
// the rule-based result, produced first, therefore won every time: the
// ai-enhanced diagnosis the user was billed for was dropped here and never
// reached the IncidentMemory (F-05). Providers run in order and each one
// enhances what came before, so the later provider is the informative one when
// confidence cannot separate them.
func (e *Engine) deduplicateDiagnoses(produced []rankedDiagnosis) []Diagnosis {
	best := make(map[string]rankedDiagnosis)
	var order []string

	for _, candidate := range produced {
		key := deduplicationKey(&candidate.diagnosis)
		current, exists := best[key]
		if !exists {
			order = append(order, key)
			best[key] = candidate
			continue
		}
		if supersedes(candidate, current) {
			e.logDiscarded(candidate, current)
			best[key] = candidate
			continue
		}
		e.logDiscarded(current, candidate)
	}

	result := make([]Diagnosis, 0, len(order))
	for _, key := range order {
		result = append(result, best[key].diagnosis)
	}
	return result
}

// supersedes reports whether candidate should replace current as the diagnosis
// of record for their shared deduplication key.
func supersedes(candidate, current rankedDiagnosis) bool {
	if candidate.diagnosis.Confidence != current.diagnosis.Confidence {
		return candidate.diagnosis.Confidence > current.diagnosis.Confidence
	}
	return candidate.stage > current.stage
}

// logDiscarded records, at INFO, that one provider's diagnosis of a finding was
// dropped in favour of another's. Discarding a billed AI call without a word is
// what made F-05 invisible: the operator log showed the AI producing diagnoses
// while every persisted incident read "rule-based".
func (e *Engine) logDiscarded(kept, dropped rankedDiagnosis) {
	if kept.diagnosis.Provider == dropped.diagnosis.Provider {
		return
	}

	reason := "same confidence, the enhancing provider wins"
	if kept.diagnosis.Confidence != dropped.diagnosis.Confidence {
		reason = "higher confidence"
	}

	e.logger.Info("discarded a duplicate diagnosis",
		"kept", kept.diagnosis.Provider,
		"keptConfidence", kept.diagnosis.Confidence,
		"discarded", dropped.diagnosis.Provider,
		"discardedConfidence", dropped.diagnosis.Confidence,
		"category", dropped.diagnosis.Category,
		"reason", reason,
	)
}

// Providers returns the list of registered providers.
func (e *Engine) Providers() []DiagnosisProvider {
	return e.providers
}
