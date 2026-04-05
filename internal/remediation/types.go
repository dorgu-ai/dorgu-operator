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

package remediation

import (
	"context"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

// ProposalResult wraps a RemediationAction with metadata about why it was proposed.
type ProposalResult struct {
	// Action is the created RemediationAction CRD. Nil if skipped.
	Action *dorguv1.RemediationAction

	// SkipReason is non-empty if the proposal was suppressed by safety or other logic.
	SkipReason string

	// Proposed is true if a RemediationAction was successfully created.
	Proposed bool
}

// SafetyResult reports whether a proposed remediation passes safety checks.
type SafetyResult struct {
	// Allowed is true if no safety violations were found.
	Allowed bool

	// Violations lists all safety rules that were violated.
	Violations []SafetyViolation
}

// SafetyViolation describes a single safety rule violation.
type SafetyViolation struct {
	// Rule identifies the safety rule (e.g., "rate-limit", "blast-radius", "deny-list", "concurrent").
	Rule string

	// Message is a human-readable explanation of the violation.
	Message string
}

// RemediationProposer generates RemediationAction CRDs from diagnoses.
type RemediationProposer interface {
	Propose(ctx context.Context, diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) (*ProposalResult, error)
}

// SafetyChecker validates a proposed remediation against safety guardrails.
type SafetyChecker interface {
	Check(ctx context.Context, action *dorguv1.RemediationAction) (*SafetyResult, error)
}
