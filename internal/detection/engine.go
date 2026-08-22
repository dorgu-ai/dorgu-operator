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

package detection

import (
	"context"
	"sort"

	"github.com/go-logr/logr"
)

// SignalCollector collects health signals from one domain.
// Each detector (node, pod, resource, controlplane) implements this.
type SignalCollector interface {
	// Name returns the collector's identifier.
	Name() string

	// Collect gathers all current signals from this domain.
	Collect(ctx context.Context) ([]Signal, error)
}

// Engine orchestrates signal collection from all registered collectors.
type Engine struct {
	collectors []SignalCollector
	correlator PersonaCorrelator
	logger     logr.Logger
}

// SetPersonaCorrelator sets the correlator used to link signals to personas.
func (e *Engine) SetPersonaCorrelator(c PersonaCorrelator) {
	e.correlator = c
}

// NewEngine creates a detection engine with the given collectors.
func NewEngine(logger logr.Logger, collectors ...SignalCollector) *Engine {
	return &Engine{
		collectors: collectors,
		logger:     logger.WithName("detection-engine"),
	}
}

// CollectAll runs all collectors and returns aggregated signals.
// Collectors that error are logged but don't block other collectors.
// Signals are sorted by severity (critical first) then by time (newest first).
func (e *Engine) CollectAll(ctx context.Context) ([]Signal, error) {
	var allSignals []Signal

	for _, collector := range e.collectors {
		signals, err := collector.Collect(ctx)
		if err != nil {
			e.logger.Error(err, "collector failed", "collector", collector.Name())
			continue
		}
		allSignals = append(allSignals, signals...)
	}

	// Correlate signals to ApplicationPersonas.
	if e.correlator != nil {
		e.correlator.Correlate(ctx, allSignals)
	}

	sort.Slice(allSignals, func(i, j int) bool {
		ri := SeverityRank(allSignals[i].Severity)
		rj := SeverityRank(allSignals[j].Severity)
		if ri != rj {
			return ri > rj
		}
		return allSignals[i].DetectedAt.After(allSignals[j].DetectedAt)
	})

	return allSignals, nil
}
