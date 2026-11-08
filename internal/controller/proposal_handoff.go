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

package controller

import (
	"context"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/websocket"
)

// proposeRemediation hands a recorded incident to the remediation proposer, if
// one is configured and the incident is still open. It is the single seam
// between the incident store and the proposal path: everything a proposal needs
// to decide (guardrails, grounding, cooldowns) lives behind Proposer.Propose.
func (r *HealthCheckReconciler) proposeRemediation(
	ctx context.Context,
	diag *diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
) {
	if r.Proposer == nil || incident == nil || incident.Status.Phase == PhaseResolved {
		return
	}

	// Honour a rejection before spending anything. Proposing runs an AI
	// planning call, so re-asking a question the user already answered bills
	// them for saying no (F-07).
	suppressed, rejectionErr := r.rejectionSuppressesProposal(ctx, diag, incident)
	if rejectionErr != nil {
		// Fail closed. Guessing "no rejection" when we cannot tell is the
		// expensive direction to be wrong in.
		r.Logger.Error(rejectionErr, "could not read the rejection history; not proposing a remediation",
			"incident", incident.Name)
		return
	}
	if suppressed != "" {
		metrics.RecordProposal(metrics.SourceDorgu, diag.SuggestedAction, metrics.ProposalSuppressed)
		r.Logger.V(1).Info("remediation not proposed", "reason", suppressed, "incident", incident.Name)
		return
	}

	result, proposeErr := r.Proposer.Propose(ctx, *diag, incident)
	metrics.RecordProposal(metrics.SourceDorgu, diag.SuggestedAction, proposalOutcome(result, proposeErr))
	switch {
	case proposeErr != nil:
		r.Logger.Error(proposeErr, "failed to propose remediation", "incident", incident.Name)
	case result.Proposed:
		r.Logger.Info("remediation proposed", "action", result.Action.Name, "incident", incident.Name)
		r.notifyRemediationCreated(result.Action)
	case result.SkipReason != "":
		r.Logger.V(1).Info("remediation skipped", "reason", result.SkipReason, "incident", incident.Name)
	}
}

// notifyRemediationCreated publishes the new RemediationAction, if anyone is
// listening.
func (r *HealthCheckReconciler) notifyRemediationCreated(action *dorguv1.RemediationAction) {
	n := r.notifier()
	if n == nil || action == nil {
		return
	}
	n.BroadcastRemediation(websocket.RemediationEvent{
		EventType:   "created",
		Name:        action.Name,
		Namespace:   action.Namespace,
		ActionType:  action.Spec.Action.Type,
		Phase:       action.Status.Phase,
		Confidence:  action.Spec.Confidence,
		PersonaName: action.Spec.PersonaRef.Name,
		PersonaKind: action.Spec.PersonaRef.Kind,
	})
}

// proposalOutcome classifies a Propose call for dorgu_proposals_total.
func proposalOutcome(result *remediation.ProposalResult, err error) string {
	switch {
	case err != nil:
		return metrics.ProposalError
	case result != nil && result.Proposed:
		return metrics.ProposalProposed
	default:
		return metrics.ProposalSkipped
	}
}
