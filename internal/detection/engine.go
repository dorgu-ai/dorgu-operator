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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

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

// FamilyCollector is a SignalCollector whose failure blinds only the signal families
// it owns, not the whole cycle.
//
// Most collectors look at the pods and workloads every incident is about: if one of
// them cannot look, silence about anything proves nothing, and the engine reports the
// cycle as incomplete (CollectionError). A collector that reads an optional, separate
// source (CPU usage from metrics.k8s.io or Prometheus) is different. Its absence or
// failure says nothing about a crash loop, and treating it as a whole-cycle failure
// would freeze every resolution and make every verification Unknown, rolling back
// correct fixes of any type, because a metrics endpoint blinked.
//
// A FamilyCollector's error is therefore logged and attributed to its families:
// Engine.SignalBlind answers for them, and CollectAll does not return it. The
// consumers that conclude health from silence (incident resolution, the verifier)
// ask SignalBlind about the signal they are judging.
type FamilyCollector interface {
	SignalCollector

	// Families lists the families this collector is the only observer of.
	Families() []Family

	// Blind reports whether the collector currently cannot vouch for silence:
	// it has not completed a look yet (a restarted process), has no source, or its
	// last look failed. Collect returns an error only for a source that exists and
	// could not be read; Blind is also true for one that is absent, which is a
	// missing capability and not an error.
	Blind() bool
}

// Engine orchestrates signal collection from all registered collectors.
type Engine struct {
	collectors []SignalCollector
	correlator PersonaCorrelator
	logger     logr.Logger

	// failedFamilyCollectors holds the FamilyCollectors whose latest Collect failed.
	mu                     sync.RWMutex
	failedFamilyCollectors map[string]bool

	// failureLog rate-limits the ERROR line for a collector that keeps failing.
	failureLog *LogThrottle
	now        func() time.Time
}

// SignalBlind reports whether the engine cannot vouch for the absence of a signal of
// this type: a FamilyCollector that owns the signal's family failed in the latest
// CollectAll, or reports itself blind. A signal in no family is never blind here;
// whether the whole cycle could look is what CollectAll's error says.
func (e *Engine) SignalBlind(t SignalType) bool {
	family, ok := FamilyOf(t)
	if !ok {
		return false
	}
	return e.FamilyBlind(family)
}

// FamilyBlind is SignalBlind for a family.
func (e *Engine) FamilyBlind(family Family) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, c := range e.collectors {
		fc, ok := c.(FamilyCollector)
		if !ok || !slices.Contains(fc.Families(), family) {
			continue
		}
		if e.failedFamilyCollectors[fc.Name()] || fc.Blind() {
			return true
		}
	}
	return false
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
		failureLog: NewLogThrottle(LogRepeatInterval),
		now:        time.Now,
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

// failureKey identifies one failure of one collector: a second, different cause inside
// the window is a new line, not a held-back repeat of the first.
func failureKey(name string, err error) string {
	sum := sha256.Sum256([]byte(err.Error()))
	return name + "/" + hex.EncodeToString(sum[:8])
}

// logCollectorFailure logs a failing collector at ERROR once per distinct error, then
// at most once per LogRepeatInterval, with the number of repeats it held back. The
// failure itself is not throttled: CollectAll returns it every cycle (or, for a
// FamilyCollector, Engine.SignalBlind reports it).
func (e *Engine) logCollectorFailure(name string, err error) {
	if e.failureLog == nil || e.now == nil { // an Engine not built by NewEngine
		e.logger.Error(err, "collector failed", "collector", name)
		return
	}
	ok, held := e.failureLog.Allow(failureKey(name, err), e.now())
	if !ok {
		return
	}
	kv := []any{"collector", name}
	if held > 0 {
		kv = append(kv, "failuresSinceLastLog", held)
	}
	e.logger.Error(err, "collector failed", kv...)
}

// CollectAll runs all collectors and returns aggregated signals.
//
// A failing collector does not block the others: their signals are still
// returned, sorted by severity (critical first) then by time (newest first).
// The failure is not swallowed, though. When any collector fails the returned
// error is a *CollectionError naming each one, so callers can tell "no signals"
// from "could not look" (see IsPartial).
//
// The exception is a FamilyCollector: its failure is logged and recorded for
// SignalBlind, and it is not part of the returned error, because it blinds only
// its own families (see FamilyCollector).
func (e *Engine) CollectAll(ctx context.Context) ([]Signal, error) {
	var allSignals []Signal
	var failures []CollectorFailure
	failedFamily := map[string]bool{}

	for _, collector := range e.collectors {
		signals, err := collector.Collect(ctx)
		if err != nil {
			e.logCollectorFailure(collector.Name(), err)
			if _, scoped := collector.(FamilyCollector); scoped {
				failedFamily[collector.Name()] = true
			} else {
				failures = append(failures, CollectorFailure{Collector: collector.Name(), Err: err})
			}
			continue
		}
		allSignals = append(allSignals, signals...)
	}

	e.mu.Lock()
	e.failedFamilyCollectors = failedFamily
	e.mu.Unlock()

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
