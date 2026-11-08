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

// StagePropose is the Stage of a skip that is the proposer's own and not a gate
// refusal: the diagnosis names no persona, or there is no change to make.
const StagePropose = "Propose"

// Reasons a proposal was skipped without the gate refusing it.
const (
	// SkipNoPersonaRef: the diagnosis names no persona to change.
	SkipNoPersonaRef = "no-persona-ref"
	// SkipRestartNotAutomated: a restart is advice, not something the proposer applies.
	SkipRestartNotAutomated = "restart-not-automated"
	// SkipUnsupportedAction: the diagnosis's action class has no proposal path.
	SkipUnsupportedAction = "unsupported-action"
	// SkipNoResourceLimits: neither the persona nor the live workload sets limits.
	SkipNoResourceLimits = "no-resource-limits"
	// SkipNoLiveLimit: the live container does not set the limit the diagnosis calls
	// to raise, and introducing one is not a fix.
	SkipNoLiveLimit = "no-live-limit"
	// SkipNoApplicableChange: the signals call for no resource change.
	SkipNoApplicableChange = "no-applicable-change"
	// SkipGateUnavailable: the gate could not re-verify the final plan, so it is not proposed.
	SkipGateUnavailable = "gate-unavailable"
	// SkipPlanChanged: the final plan differed from what the gate cleared.
	SkipPlanChanged = "plan-changed-after-gate"
)

// ProposalResult wraps a RemediationAction with metadata about why it was proposed.
type ProposalResult struct {
	// Action is the created RemediationAction CRD. Nil if skipped.
	Action *dorguv1.RemediationAction

	// SkipReason is non-empty if the proposal was suppressed by the gate or other logic.
	// It is a sentence for a human or a log line. Nothing branches on it: Stage and
	// Reason are the machine-readable form.
	SkipReason string

	// Stage and Reason say why nothing was proposed, as enums. For a gate refusal
	// they are the refusing stage (gate.Stage*) and guardrail (gate.Guardrail*); for
	// a skip that is not the gate's (nothing to adjust, an action class the
	// proposer does not automate) Stage is StagePropose and Reason one of the Skip*
	// constants. Both are empty when the proposal was made.
	Stage  string
	Reason string

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
