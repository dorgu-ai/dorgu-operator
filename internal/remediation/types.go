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
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// ProposalResult wraps a RemediationAction with metadata about why it was proposed.
type ProposalResult struct {
	// Action is the created RemediationAction CRD. Nil if skipped.
	Action *dorguv1.RemediationAction

	// SkipReason is non-empty if the proposal was suppressed by the gate or other logic.
	SkipReason string

	// Proposed is true if a RemediationAction was successfully created.
	Proposed bool

	// Suppressed is true when the proposal was withheld because a human already
	// declined this fix (the rejection cooldown), as opposed to any other refusal.
	// It feeds dorgu_proposals_total{result="suppressed"}.
	Suppressed bool

	// Decision is the gate's last word on the proposal: nil only when the
	// proposal was skipped before the gate was asked.
	Decision *gate.Decision
}

// RemediationProposer generates RemediationAction CRDs from diagnoses.
type RemediationProposer interface {
	Propose(ctx context.Context, diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) (*ProposalResult, error)
}
