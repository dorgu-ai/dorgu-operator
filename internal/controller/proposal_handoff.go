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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// EventReasonContainerMismatch is the Event reason when a CPU throttling proposal was
// skipped because the throttled container is not the one the persona's single resources
// block describes (remediation.SkipContainerMismatch).
const EventReasonContainerMismatch = "DorguContainerMismatch"

// containerMismatchEventInterval is how often the same incident may say it again. A
// Kubernetes Event lives about an hour, so a fault that outlasts it is told once more.
const containerMismatchEventInterval = time.Hour

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
		if r.firstProposalFor(ctx, incident, result.Action) {
			metrics.RecordStageDuration(metrics.StageDetectToProposal, time.Since(incident.Spec.Detection.FirstSeen.Time))
		}
		// The gate refused the AI plan's persona change and its advisory steps were
		// proposed instead: the refusal of the first plan is still recorded (KPI I3),
		// and the proposal answers it.
		for _, refused := range planRefusalsOf(result) {
			if gate.IsStage(refused.Stage) {
				r.recordProposalRefusal(ctx, incident, refused)
			}
		}
		r.clearProposalRefusal(ctx, incident, dorguv1.ReasonProposalMade, "a RemediationAction was proposed for this incident")
	case result.SkipReason != "":
		r.Logger.V(1).Info("remediation skipped", "reason", result.SkipReason, "incident", incident.Name,
			"stage", result.Stage, "enum", result.Reason)
		// A refusal by the gate creates no RemediationAction, so the incident is where
		// it is recorded. The proposer's own skips (nothing to adjust, a restart is
		// advice) are not refusals and are not.
		if gate.IsStage(result.Stage) {
			r.recordProposalRefusal(ctx, incident, result)
		}
		// The intake refusing Dorgu's own proposal (it is not a gate stage, so it is
		// not recorded on the incident) is rare after the planner's rate-limit
		// exemption, and a silent skip is how it was once missed: say so.
		if result.Stage == dorguv1alpha1.StageIntake {
			r.Logger.Info("the proposal intake refused Dorgu's own proposal",
				"incident", incident.Name, "reason", result.Reason, "message", result.SkipReason)
		}
		r.reportContainerMismatch(incident, result)
		// A proposal that is outstanding answers any earlier refusal.
		if result.Stage == remediation.StageDeferred {
			r.clearProposalRefusal(ctx, incident, dorguv1.ReasonProposalDeferred,
				"an earlier proposal for this incident is outstanding or cooling down")
		}
	}
}

// reportContainerMismatch tells a person what the proposer's container-mismatch skip
// says: the throttled container is not the one this persona's resources block describes,
// so nothing is proposed and its limit has to be raised by hand. It is the proposer's own
// skip and not a gate refusal, so the incident carries no condition for it and a V(1) log
// line reaches nobody; an Event on the incident does. Once per incident per
// containerMismatchEventInterval, like the other Events (which go out once per change).
func (r *HealthCheckReconciler) reportContainerMismatch(incident *dorguv1.IncidentMemory, result *remediation.ProposalResult) {
	if result.Stage != remediation.StagePropose || result.Reason != remediation.SkipContainerMismatch {
		return
	}
	r.mismatchEventsOnce.Do(func() { r.mismatchEvents = detection.NewLogThrottle(containerMismatchEventInterval) })
	if ok, _ := r.mismatchEvents.Allow(fmt.Sprintf("%s/%s", incident.Namespace, incident.Name), time.Now()); !ok {
		return
	}
	emit(r.Recorder, incident, corev1.EventTypeWarning, EventReasonContainerMismatch, result.SkipReason)
}

// planRefusalsOf is every refusal a narrowed proposal answers, in order (L-5): a chain of
// narrowings records each one. A proposer that only sets PlanRefusal has one.
func planRefusalsOf(result *remediation.ProposalResult) []*remediation.ProposalResult {
	if len(result.PlanRefusals) > 0 {
		return result.PlanRefusals
	}
	if result.PlanRefusal != nil {
		return []*remediation.ProposalResult{result.PlanRefusal}
	}
	return nil
}

// proposalOutcome classifies a Propose call for dorgu_proposals_total.
func proposalOutcome(result *remediation.ProposalResult, err error) string {
	switch {
	case err != nil:
		return metrics.ProposalError
	case result != nil && result.Suppressed:
		return metrics.ProposalSuppressed
	case result != nil && result.Stage == remediation.StageDeferred:
		return metrics.ProposalDeferred
	case result != nil && result.Proposed:
		return metrics.ProposalProposed
	default:
		return metrics.ProposalSkipped
	}
}

// firstProposalFor reports whether action is the first RemediationAction ever
// proposed for the incident. detect-to-proposal is the machine's latency from
// first detection to its first proposal (KPI Q1): a second action for the same
// incident, hours later after a failure or a rejection, is not that latency and
// adds no sample. When the RemediationActions cannot be listed nothing is
// guessed and nothing is observed.
func (r *HealthCheckReconciler) firstProposalFor(
	ctx context.Context, incident *dorguv1.IncidentMemory, action *dorguv1.RemediationAction,
) bool {
	var actions dorguv1.RemediationActionList
	if err := r.Client.List(ctx, &actions, client.InNamespace(incident.Namespace)); err != nil {
		logFailure(r.Logger, err, "could not list RemediationActions to time the first proposal", "incident", incident.Name)
		return false
	}
	for i := range actions.Items {
		other := &actions.Items[i]
		if other.Name != action.Name && other.Spec.IncidentRef.Name == incident.Name {
			return false
		}
	}
	return true
}
