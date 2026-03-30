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
// Diagnoses are sorted by confidence descending.
func (e *Engine) Analyze(ctx context.Context, signals []detection.Signal) ([]Diagnosis, error) {
	if len(signals) == 0 {
		return nil, nil
	}

	var allDiagnoses []Diagnosis

	for _, provider := range e.providers {
		diagnoses, err := provider.Diagnose(ctx, signals)
		if err != nil {
			e.logger.Error(err, "diagnosis provider failed", "provider", provider.Name())
			return nil, fmt.Errorf("provider %s failed: %w", provider.Name(), err)
		}
		e.logger.V(1).Info("provider produced diagnoses",
			"provider", provider.Name(),
			"count", len(diagnoses),
		)
		allDiagnoses = append(allDiagnoses, diagnoses...)
	}

	sort.Slice(allDiagnoses, func(i, j int) bool {
		return allDiagnoses[i].Confidence > allDiagnoses[j].Confidence
	})

	return allDiagnoses, nil
}

// Providers returns the list of registered providers.
func (e *Engine) Providers() []DiagnosisProvider {
	return e.providers
}
