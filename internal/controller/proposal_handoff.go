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

	// A rejection, a cooldown, observe mode, a protected namespace and every other
	// reason not to propose are the gate's, and are decided inside Propose before
	// anything is spent on a planning call.
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
	case result != nil && result.Suppressed:
		return metrics.ProposalSuppressed
	case result != nil && result.Proposed:
		return metrics.ProposalProposed
	default:
		return metrics.ProposalSkipped
	}
}
