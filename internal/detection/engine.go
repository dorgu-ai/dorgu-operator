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
	"errors"
	"fmt"
	"sort"
	"strings"

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

// CollectorFailure records one collector that could not look.
type CollectorFailure struct {
	Collector string
	Err       error
}

// CollectionError reports that one or more collectors failed during CollectAll.
//
// It is returned together with the signals the working collectors did produce,
// so a caller can still act on what was seen. What it must never do is read the
// result as complete: a collector that errored did not see "nothing", it did not
// see at all. Anything that concludes health from the absence of a signal (the
// verifier, incident resolution) has to treat this error as "unknown".
type CollectionError struct {
	Failures []CollectorFailure
}

// Error implements error.
func (e *CollectionError) Error() string {
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, fmt.Sprintf("%s: %v", f.Collector, f.Err))
	}
	return fmt.Sprintf("%d collector(s) failed: %s", len(e.Failures), strings.Join(parts, "; "))
}

// Unwrap exposes every collector error to errors.Is and errors.As.
func (e *CollectionError) Unwrap() []error {
	errs := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		errs = append(errs, f.Err)
	}
	return errs
}

// Collectors returns the names of the collectors that failed, in run order.
func (e *CollectionError) Collectors() []string {
	names := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		names = append(names, f.Collector)
	}
	return names
}

// IsPartial reports whether err says collection was incomplete, in which case
// the signals returned alongside it are valid but not exhaustive.
func IsPartial(err error) bool {
	var ce *CollectionError
	return errors.As(err, &ce)
}

// CollectAll runs all collectors and returns aggregated signals.
//
// A failing collector does not block the others: their signals are still
// returned, sorted by severity (critical first) then by time (newest first).
// The failure is not swallowed, though. When any collector fails the returned
// error is a *CollectionError naming each one, so callers can tell "no signals"
// from "could not look" (see IsPartial).
func (e *Engine) CollectAll(ctx context.Context) ([]Signal, error) {
	var allSignals []Signal
	var failures []CollectorFailure

	for _, collector := range e.collectors {
		signals, err := collector.Collect(ctx)
		if err != nil {
			e.logger.Error(err, "collector failed", "collector", collector.Name())
			failures = append(failures, CollectorFailure{Collector: collector.Name(), Err: err})
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

	if len(failures) > 0 {
		return allSignals, &CollectionError{Failures: failures}
	}
	return allSignals, nil
}
