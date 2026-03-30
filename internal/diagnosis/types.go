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
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// Diagnosis is the result of analyzing a set of signals.
type Diagnosis struct {
	// Summary is a human-readable explanation of the root cause.
	Summary string

	// Confidence is the overall confidence score (0.0-1.0).
	Confidence float64

	// Provider identifies the diagnosis source.
	Provider string

	// Category classifies the root cause.
	Category string

	// Severity is the highest severity among contributing signals.
	Severity detection.Severity

	// PersonaRef links to the affected Persona.
	PersonaRef *dorguv1.PersonaReference

	// AffectedResources lists all resources involved.
	AffectedResources []dorguv1.ResourceReference

	// Contributing lists the signals that led to this diagnosis.
	Contributing []ContributingSignal

	// SuggestedAction hints at what remediation might help.
	// e.g., "resource-adjustment", "restart", "rollback", "scale-up", "deployment-fix", "investigate"
	SuggestedAction string

	// DiagnosedAt is when the diagnosis was produced.
	DiagnosedAt time.Time
}

// ContributingSignal links a signal to its role in the diagnosis.
type ContributingSignal struct {
	Signal detection.Signal
	Detail string // human-readable explanation of this signal's contribution
}

// DiagnosisProvider is the interface for diagnosis strategies.
// Phase 2a: RuleBasedProvider (deterministic).
// Phase 2b: AIProvider wraps this with LLM enhancement.
type DiagnosisProvider interface {
	// Name returns the provider identifier.
	Name() string

	// Diagnose analyzes signals and produces diagnoses.
	// May return multiple diagnoses if signals indicate independent issues.
	Diagnose(ctx context.Context, signals []detection.Signal) ([]Diagnosis, error)
}
