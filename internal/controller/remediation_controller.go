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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// IncidentMemory resolution outcomes written by this controller. They are the
// values of the IncidentMemory resolution.outcome enum.
const (
	IncidentOutcomeResolved     = "resolved"
	IncidentOutcomeRollback     = "rollback"
	IncidentOutcomePartial      = "partial"
	IncidentOutcomeFailed       = "failed"
	IncidentOutcomeAcknowledged = "acknowledged"

	// IncidentOutcomeSelfResolved: the incident cleared and verification passed,
	// but nothing Dorgu proposed was ever applied to the workload (--no-heal, a
	// declined prompt, a patch that changed nothing), so the recovery is not a Dorgu
	// heal and no KPI may count it as one. The HealCredited condition on the
	// RemediationAction says the same thing.
	IncidentOutcomeSelfResolved = "self-resolved"
)

// RemediationAction phase constants.
const (
	RemediationPhasePending  = "Pending"
	RemediationPhaseApproved = "Approved"
	// RemediationPhaseTicketed is where an approval that passed the gate's re-check
	// on live state waits to be applied: status.applyTicket is the one change that
	// may land.
	RemediationPhaseTicketed  = "Ticketed"
	RemediationPhaseApplying  = "Applying"
	RemediationPhaseVerifying = "Verifying"
	RemediationPhaseCompleted = "Completed"
	// RemediationPhaseAcknowledged is where an approved advisory plan settles:
	// approval is recorded, nothing is applied, and no cooldown is triggered.
	RemediationPhaseAcknowledged = "Acknowledged"
	RemediationPhaseRolledBack   = "RolledBack"
	RemediationPhaseFailed       = "Failed"
	RemediationPhaseRejected     = "Rejected"
	RemediationPhaseExpired      = "Expired"
)

// RemediationAction condition types.
const (
	ConditionApplied    = "Applied"
	ConditionVerified   = "Verified"
	ConditionRolledBack = "RolledBack"

	// ConditionExpired records that a proposal was never approved in time (or
	// was approved after its deadline) and so can no longer be applied.
	ConditionExpired = "Expired"

	// ReasonApprovalTimedOut is the ConditionExpired reason.
	ReasonApprovalTimedOut = "ApprovalTimedOut"

	// ReasonSuperseded is the other ConditionExpired reason: the incident the
	// proposal answered escalated to a worse shape, so the advice no longer fits.
	ReasonSuperseded = "Superseded"

	// ConditionWorkloadDiverged reports what a rollback could NOT reach.
	//
	// The operator restores the ApplicationPersona and has no write verbs on
	// Deployments, so a workload the CLI already patched keeps the change. In
	// clean-room run #5 the persona went back to 512Mi while the Deployment sat
	// at the 16Mi the heal had applied, and the only thing the record said was
	// "Remediation rolled back due to degraded health" (CR5-02). This condition
	// is where that divergence is stated: True when the workload still carries
	// the change, False when it does not, Unknown when Dorgu could not read it.
	// Its message names the Deployment, the container, the field, live versus
	// pre-remediation, and what to do about it, and is written to be printed
	// verbatim by a client.
	ConditionWorkloadDiverged = "WorkloadDiverged"

	// ConditionPersonaEdited reports what a rollback left alone: persona fields a
	// human changed after the heal. True when any field was left, with the fields
	// in the message. Its reason is a gate.ReasonRollback* enum.
	ConditionPersonaEdited = "PersonaEditedSinceHeal"

	// ConditionHealCredited is whether the recovery may be counted as a Dorgu heal.
	// True only when the CLI recorded a patch and the Deployment's generation moved
	// past the ticket's. False says why not, so a KPI exclusion is readable from
	// status without inference.
	ConditionHealCredited = "HealCredited"

	// ReasonNothingPatched is the HealCredited=False reason when no workload change
	// followed the ticket.
	ReasonNothingPatched = "NothingPatched"

	// ReasonResourcesNotTicket is the HealCredited=False reason when the live
	// container does not hold the values the ticket wrote.
	ReasonResourcesNotTicket = "ResourcesNotTicket"

	// ReasonWorkloadPatchedByTicket is the HealCredited=True reason.
	ReasonWorkloadPatchedByTicket = "WorkloadPatchedByTicket"
)

// Default verification wait and retry constants.
const (
	defaultVerificationWait = 10 * time.Minute
	maxUnknownRetries       = 2
)

// RemediationController reconciles RemediationAction objects through their lifecycle.
//
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationactions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationactions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories/status,verbs=get;update;patch
type RemediationController struct {
	client.Client

	// Gate is the one gate. The controller asks it to re-check an approval on live
	// state and to confirm the workload still matches the ticket; it decides
	// nothing about safety itself. With no gate nothing is approved.
	Gate *gate.Gate

	Executor *remediation.Executor
	Verifier *remediation.Verifier
	Rollback *remediation.Rollback
	Logger   logr.Logger

	// Recorder emits the Kubernetes Events for refusals and ticket issuance. Nil
	// records none.
	Recorder record.EventRecorder

	// PendingTTL is how long a proposal may wait for approval, counted from its
	// creation, before it becomes Expired. Zero disables the TTL (an explicit
	// spec.approval.deadline still applies).
	PendingTTL time.Duration
}

// Reconcile drives the RemediationAction through its lifecycle state machine.
func (r *RemediationController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Logger.WithValues("remediationaction", req.NamespacedName)

	var action dorguv1.RemediationAction
	if err := r.Get(ctx, req.NamespacedName, &action); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Every handler writes the new phase onto action and returns an error if
	// the write failed, so a changed phase after a clean return is a
	// transition that reached the API server.
	from := action.Status.Phase
	result, err := r.dispatch(ctx, logger, &action)
	if err == nil {
		metrics.RecordTransition(from, action.Status.Phase)
	}
	// A status write that lost an optimistic-lock race, or an object that went away,
	// is not a fault: requeue quietly at V(1) instead of an ERROR line and a
	// reconcile-error count (N4).
	return quietRaces(logger, result, err)
}

// dispatch runs the handler for the action's current phase.
func (r *RemediationController) dispatch(
	ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction,
) (ctrl.Result, error) {
	if restored, err := r.reassertSuperseded(ctx, action); restored {
		return ctrl.Result{}, err
	}

	switch action.Status.Phase {
	case RemediationPhasePending:
		return r.handlePending(ctx, logger, action)

	case RemediationPhaseApproved:
		return r.handleApproved(ctx, logger, action)

	case RemediationPhaseTicketed:
		return r.handleTicketed(ctx, logger, action)

	case RemediationPhaseApplying:
		return r.handleApplying(ctx, logger, action)

	case RemediationPhaseVerifying:
		return r.handleVerifying(ctx, logger, action)

	case RemediationPhaseRejected:
		return ctrl.Result{}, r.recordRejection(ctx, logger, action)

	case RemediationPhaseCompleted, RemediationPhaseAcknowledged, RemediationPhaseRolledBack,
		RemediationPhaseFailed, RemediationPhaseExpired:
		// Terminal states — no-op.
		return ctrl.Result{}, nil

	default:
		logger.V(1).Info("unknown phase, ignoring", "phase", action.Status.Phase)
		return ctrl.Result{}, nil
	}
}

// recordRejection timestamps a rejection the first time the operator sees it.
//
// `dorgu remediation reject` patches status.phase and nothing else, so the
// decision is recorded but not when it was made, and there is no time to hold a
// cooldown against. That is why declining a remediation bought about 30 seconds
// of quiet before the same fix came back, billed again (F-07). Stamping a
// condition keeps the timestamp inside the existing schema, and stamping it once
// keeps the cooldown from restarting on every pass.
func (r *RemediationController) recordRejection(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) error {
	if hasCondition(action.Status.Conditions, ConditionRejected) {
		return nil
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh dorguv1.RemediationAction
		if err := r.Get(ctx, client.ObjectKeyFromObject(action), &fresh); err != nil {
			return err
		}
		if fresh.Status.Phase != RemediationPhaseRejected ||
			hasCondition(fresh.Status.Conditions, ConditionRejected) {
			return nil
		}
		setCondition(&fresh.Status.Conditions, ConditionRejected, metav1.ConditionTrue,
			ReasonUserRejected,
			fmt.Sprintf("declined by a human; dorgu will not re-propose this fix for %s", RejectionCooldown))
		return r.Status().Update(ctx, &fresh)
	})
	if err != nil {
		return fmt.Errorf("recording the rejection timestamp on %s: %w", action.Name, err)
	}

	observeDecisionTime(action, time.Now())
	logger.Info("rejection recorded; suppressing re-proposal", "cooldown", RejectionCooldown)
	return nil
}

// hasCondition reports whether a condition of the given type is present.
func hasCondition(conditions []metav1.Condition, condType string) bool {
	for i := range conditions {
		if conditions[i].Type == condType {
			return true
		}
	}
	return false
}

// handlePending waits for a human decision, and expires the proposal when none
// arrives in time. It requeues for the deadline itself: nothing else would wake
// a Pending action, since an idle object is not reconciled again until a resync.
func (r *RemediationController) handlePending(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	deadline, ok := r.approvalDeadline(action)
	if !ok {
		return ctrl.Result{}, nil
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		logger.Info("pending remediation was never approved; expiring it", "deadline", deadline)
		return ctrl.Result{}, r.transitionToExpired(ctx, action, deadline, false)
	}
	// A second of slack so the requeue lands after the deadline, not just before.
	return ctrl.Result{RequeueAfter: remaining + time.Second}, nil
}

// approvalDeadline is when a proposal stops being approvable: the earlier of the
// operator-owned PendingTTL (counted from creation) and the author's own
// spec.approval.deadline. The author can shorten the window but never extend
// it. ok is false when neither applies.
func (r *RemediationController) approvalDeadline(action *dorguv1.RemediationAction) (time.Time, bool) {
	var deadline time.Time
	found := false

	if r.PendingTTL > 0 && !action.CreationTimestamp.IsZero() {
		deadline = action.CreationTimestamp.Add(r.PendingTTL)
		found = true
	}
	if action.Spec.Approval != nil && action.Spec.Approval.Deadline != nil {
		authored := action.Spec.Approval.Deadline.Time
		if !found || authored.Before(deadline) {
			deadline = authored
			found = true
		}
	}
	return deadline, found
}

// transitionToExpired settles a proposal that missed its approval window.
//
// Expired is terminal, nothing was applied, and it is deliberately not a
// rejection or a failure: no human said no, and the cluster was never touched,
// so neither the rejection cooldown nor the failed-remediation cooldown holds it
// against the application. If the problem persists, Dorgu may propose again.
func (r *RemediationController) transitionToExpired(
	ctx context.Context, action *dorguv1.RemediationAction, deadline time.Time, approvedLate bool,
) error {
	message := fmt.Sprintf(
		"Not approved by %s, so this proposal expired without applying anything. "+
			"It is not a rejection and starts no cooldown; Dorgu may propose again if the problem persists.",
		deadline.UTC().Format(time.RFC3339))
	if approvedLate {
		message = fmt.Sprintf(
			"Approved after its deadline of %s, so it was refused instead of applied. "+
				"Nothing was changed. It is not a rejection and starts no cooldown; Dorgu may propose again if the problem persists.",
			deadline.UTC().Format(time.RFC3339))
	}

	action.Status.Phase = RemediationPhaseExpired
	setCondition(&action.Status.Conditions, ConditionExpired, metav1.ConditionTrue, ReasonApprovalTimedOut, message)

	if err := r.Status().Update(ctx, action); err != nil {
		return fmt.Errorf("updating status to Expired: %w", err)
	}

	eventType := corev1.EventTypeNormal
	if approvedLate {
		eventType = corev1.EventTypeWarning
	}
	emit(r.Recorder, action, eventType, EventReasonExpired, message)
	return nil
}

// handleApproved re-checks an approval against the cluster as it is now, and
// either issues the apply ticket or refuses.
//
// An approved plan with nothing to apply settles as Acknowledged instead: its
// steps are advisory, so there is nothing to clear and nothing has gone wrong.
// Sending it to the executor is what used to mark it Failed and put the app into
// a 30-minute remediation blackout (F-03).
//
// For everything else the gate runs again (ApprovalRecheck) on live state: a
// proposal can be hours old, and an approval says yes to a plan, not to whatever
// the cluster has become. A refusal is persisted with its reason and ends the
// action as Failed/GateRefusedAtApproval with no cooldown, since nothing was
// applied. A pass becomes status.applyTicket and the phase Ticketed, and the
// ticket is applied in the same pass.
func (r *RemediationController) handleApproved(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	// A proposal approved after its deadline was approved against a cluster that
	// no longer exists. Refuse it rather than apply it (OP-14).
	if deadline, ok := r.approvalDeadline(action); ok && !time.Now().Before(deadline) {
		logger.Info("approval arrived after the proposal's deadline; expiring it instead of applying",
			"deadline", deadline)
		return ctrl.Result{}, r.transitionToExpired(ctx, action, deadline, true)
	}

	if !action.HasAutoApplicableChange() {
		logger.Info("approved plan is advisory; recording the approval without applying anything",
			"actionType", action.Spec.Action.Type, "steps", len(action.Spec.Steps))
		return ctrl.Result{}, r.transitionToAcknowledged(ctx, action)
	}

	decision, err := r.Gate.Run(ctx, gate.ApprovalCandidate(action), gate.PhaseApprovalRecheck)
	if err != nil {
		// The gate could not decide (a read failed). Nothing is applied and the
		// approval is retried; this is never treated as a pass. The error is
		// returned, and controller-runtime logs a returned error once: logging it
		// here as well would print it twice (N4).
		return ctrl.Result{}, fmt.Errorf("the approval re-check could not run; nothing was applied, will retry: %w", err)
	}
	if !decision.Allowed() {
		return ctrl.Result{}, r.refuseAtApproval(ctx, logger, action, decision)
	}

	ticket, err := decision.Ticket()
	if err != nil {
		// The gate cleared a plan that, after clamping, applies nothing.
		return ctrl.Result{}, r.refuseAtApproval(ctx, logger, action, decision)
	}

	action.Status.Phase = RemediationPhaseTicketed
	action.Status.ApplyTicket = ticket
	action.Status.Gate = decision.Status()
	setCondition(&action.Status.Conditions, dorguv1.ConditionTicketed, metav1.ConditionTrue, dorguv1.ReasonTicketIssued,
		fmt.Sprintf("the gate re-checked this approval on live state and issued an apply ticket valid until %s",
			ticket.ExpiresAt.UTC().Format(time.RFC3339)))
	if err := r.Status().Update(ctx, action); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status to Ticketed: %w", err)
	}
	r.reportTicketIssued(action, ticket)
	if action.Status.ApprovedAt != nil {
		observeDecisionTime(action, action.Status.ApprovedAt.Time)
	}

	logger.Info("approval cleared the gate on live state; ticket issued", "expiresAt", ticket.ExpiresAt)

	return r.handleTicketed(ctx, logger, action)
}

// refuseAtApproval ends an approval the gate refused: Failed with the
// GateRefusedAtApproval reason, no cooldown (nothing was applied), the refusing
// guardrail persisted as an enum on the GateRefused condition, and the whole
// decision on status.gate.
func (r *RemediationController) refuseAtApproval(
	ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction, decision *gate.Decision,
) error {
	if decision.Allowed() {
		// The gate allowed the plan but its clamps left nothing to issue a ticket for.
		// That is a refusal like any other: recorded on the decision and counted.
		decision.RefuseNoPlan()
	}
	stage, guardrail := decision.RefusedBy()
	reason := decision.SkipReason()
	logger.Info("the gate refused this approval on live state; nothing was applied",
		"stage", stage, "guardrail", guardrail, "result", decision.Result)

	action.Status.Gate = decision.Status()
	setCondition(&action.Status.Conditions, dorguv1.ConditionGateRefused, metav1.ConditionTrue,
		gate.ReasonEnum(guardrail), reason)

	message := fmt.Sprintf("refused at approval (%s): %s; nothing was applied", guardrail, reason)
	if err := r.transitionToFailedWithReason(ctx, action, dorguv1.ReasonGateRefusedAtApproval, message); err != nil {
		return err
	}
	r.reportApprovalRefusal(action, stage, message)
	if action.Status.ApprovedAt != nil {
		observeDecisionTime(action, action.Status.ApprovedAt.Time)
	}
	return nil
}

// handleTicketed applies the apply ticket: exactly its patch, and only when the
// gate confirms the ticket still stands (present, unexpired, same workload).
func (r *RemediationController) handleTicketed(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	logger.Info("applying the apply ticket")

	if err := r.Executor.Apply(ctx, action); err != nil {
		var precondition *remediation.PreconditionError
		if errors.As(err, &precondition) {
			// Nothing was written to the cluster, so this must not count as a
			// failed remediation for the cooldown.
			logger.Error(err, "remediation refused before apply", "guardrail", precondition.Guardrail)
			// The check at apply refused: persist which guardrail, as an enum, beside
			// the refusal at approval's. The executor always names one.
			setCondition(&action.Status.Conditions, dorguv1.ConditionGateRefused, metav1.ConditionTrue,
				gate.ReasonEnum(precondition.Guardrail), precondition.Reason)
			message := fmt.Sprintf("%v; nothing was applied", err)
			if err := r.transitionToFailedWithReason(ctx, action, dorguv1.ReasonPreconditionRejected, message); err != nil {
				return ctrl.Result{}, err
			}
			r.reportApplyRefusal(action, precondition.Guardrail, message)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to apply remediation")
		return ctrl.Result{}, r.transitionToFailed(ctx, action, fmt.Sprintf("apply failed: %v", err))
	}

	// Transition to Applying.
	now := metav1.Now()
	action.Status.Phase = RemediationPhaseApplying
	action.Status.AppliedAt = &now
	setCondition(&action.Status.Conditions, ConditionApplied, metav1.ConditionTrue, "PatchApplied", "Remediation patch applied to ApplicationPersona")

	if err := r.Status().Update(ctx, action); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status to Applying: %w", err)
	}
	if action.Status.ApprovedAt != nil {
		metrics.RecordStageDuration(metrics.StageDecisionToApplied, now.Sub(action.Status.ApprovedAt.Time))
	}

	logger.Info("patch applied, waiting for verification window")

	// Requeue after healthCheckAfter duration.
	verificationDelay := r.getVerificationDelay(action)
	return ctrl.Result{RequeueAfter: verificationDelay}, nil
}

// handleApplying checks if it's time to start verification.
func (r *RemediationController) handleApplying(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	if action.Status.AppliedAt == nil {
		// Should not happen, but handle gracefully.
		return ctrl.Result{}, r.transitionToFailed(ctx, action, "AppliedAt is nil in Applying phase")
	}

	verificationDelay := r.getVerificationDelay(action)
	elapsed := time.Since(action.Status.AppliedAt.Time)

	if elapsed < verificationDelay {
		// Not yet time to verify — requeue for remaining wait.
		remaining := verificationDelay - elapsed
		logger.V(1).Info("waiting for verification window", "remaining", remaining)
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	// Transition to Verifying.
	action.Status.Phase = RemediationPhaseVerifying
	if err := r.Status().Update(ctx, action); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status to Verifying: %w", err)
	}

	logger.Info("verification window reached, starting verification")
	return ctrl.Result{Requeue: true}, nil
}

// handleVerifying runs the verifier and transitions based on the result.
func (r *RemediationController) handleVerifying(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	result, err := r.Verifier.Verify(ctx, action)
	if err != nil {
		logger.Error(err, "verification error")
		result = remediation.VerificationUnknown
	}

	action.Status.VerificationResult = string(result)

	var res ctrl.Result
	switch result {
	case remediation.VerificationHealthy:
		err = r.handleVerificationHealthy(ctx, logger, action)

	case remediation.VerificationDegraded:
		res, err = r.handleVerificationDegraded(ctx, logger, action)

	case remediation.VerificationUnknown:
		res, err = r.handleVerificationUnknown(ctx, logger, action)

	default:
		return ctrl.Result{}, r.transitionToFailed(ctx, action, fmt.Sprintf("unexpected verification result: %s", result))
	}
	// Counted once the handler's status write has succeeded. A conflict requeues
	// the reconcile and the verification runs again; counting before the write
	// would count it twice.
	if err == nil {
		recordVerification(action, result)
	}
	return res, err
}

// handleVerificationHealthy completes the remediation and updates the incident.
//
// Healthy is not always clean. When the workload was changed by someone else
// after the ticket was written (replaced, or edited by another writer), the
// verification result cannot be credited to the change Dorgu applied: the outcome
// is recorded as partial with the WorkloadDiverged reason. The phase stays
// Completed, since verification did pass; the condition and the incident outcome
// are what a KPI reads to tell a clean heal from an unattributable one.
func (r *RemediationController) handleVerificationHealthy(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) error {
	outcome := r.recordTicketDivergence(ctx, logger, action)
	if outcome == IncidentOutcomeResolved {
		outcome = r.creditOrSelfResolve(ctx, logger, action)
	}

	action.Status.Phase = RemediationPhaseCompleted
	setCondition(&action.Status.Conditions, ConditionVerified, metav1.ConditionTrue, "HealthRestored", "Verification confirmed health restored")

	if err := r.Status().Update(ctx, action); err != nil {
		return fmt.Errorf("updating status to Completed: %w", err)
	}
	recordHealCredit(action, outcome)

	// Update incident with resolution info.
	if err := r.updateIncidentResolution(ctx, action, outcome); err != nil {
		logFailure(logger, err, "failed to update incident resolution")
	}

	logger.Info("remediation completed", "incidentOutcome", outcome)
	return nil
}

// recordTicketDivergence compares the live workload to the ticket and returns the
// incident outcome for a healthy verification: resolved for a clean heal, partial
// when the workload diverged from the ticket or could not be read to tell.
//
// An action applied before tickets existed has none to compare to and stays
// resolved. A workload that cannot be read is not claimed clean: it is partial
// with its own reason, so it never counts as a clean heal.
func (r *RemediationController) recordTicketDivergence(
	ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction,
) string {
	if action.Status.ApplyTicket == nil {
		return IncidentOutcomeResolved
	}

	problem, err := r.Gate.WorkloadDivergence(ctx, action)
	switch {
	case err != nil:
		logger.Error(err, "could not read the workload to compare it to the ticket")
		setCondition(&action.Status.Conditions, ConditionWorkloadDiverged, metav1.ConditionUnknown,
			remediation.ReasonWorkloadUnreadable,
			"Verification passed, but Dorgu could not read the workload to tell whether someone else changed it since the ticket. This is not counted as a clean heal.")
		return IncidentOutcomePartial
	case problem != nil:
		logger.Info("verification passed, but the workload diverged from the ticket", "guardrail", problem.Guardrail)
		setCondition(&action.Status.Conditions, ConditionWorkloadDiverged, metav1.ConditionTrue,
			dorguv1.ReasonWorkloadDivergedFromTicket,
			"Verification passed, but the workload was changed by someone else after the ticket was written ("+problem.Reason+
				"), so the heal cannot be credited to the change Dorgu applied.")
		return IncidentOutcomePartial
	}
	setCondition(&action.Status.Conditions, ConditionWorkloadDiverged, metav1.ConditionFalse,
		"WorkloadMatchesTicket", "The workload still matches the ticket: nothing but the approved change touched it.")
	return IncidentOutcomeResolved
}

// creditOrSelfResolve decides whether a clean, healthy verification is a Dorgu
// heal. It is only when the workload was actually changed after the ticket: the
// CLI recorded its patch (WorkloadPatched) and the Deployment's generation moved
// past the ticket's observed one. A --no-heal approval, a declined prompt and a
// patch that changed nothing leave neither, and the recovery is then the
// application's own: outcome self-resolved, HealCredited=False, never counted as
// a heal (F10). The generation is the apiserver's, so a status writer cannot
// claim a heal by adding the condition alone.
//
// An action with no ticket predates tickets and stays resolved. If the workload
// cannot be read the heal is not credited either.
func (r *RemediationController) creditOrSelfResolve(
	ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction,
) string {
	ticket := action.Status.ApplyTicket
	if ticket == nil {
		return IncidentOutcomeResolved
	}

	patched := false
	notTicket := ""
	if cond := findCondition(action.Status.Conditions, gate.ConditionWorkloadPatched); cond != nil && cond.Status == metav1.ConditionTrue {
		evidence, err := r.Gate.HealEvidence(ctx, action)
		switch {
		case err != nil:
			logFailure(logger, err, "could not read the workload to confirm the patch; not crediting the heal")
		case evidence.Generation <= ticket.ObservedGeneration:
			// The Deployment's spec never moved: the patch changed nothing.
		case !evidence.ResourcesMatchTicket:
			notTicket = evidence.Mismatch
		default:
			patched = true
		}
	}
	if patched {
		setCondition(&action.Status.Conditions, ConditionHealCredited, metav1.ConditionTrue, ReasonWorkloadPatchedByTicket,
			"The workload was changed after the ticket (recorded by the CLI, confirmed by the Deployment's generation) and its resources are the ticket's: this recovery counts as a Dorgu heal.")
		return IncidentOutcomeResolved
	}
	if notTicket != "" {
		setCondition(&action.Status.Conditions, ConditionHealCredited, metav1.ConditionFalse, ReasonResourcesNotTicket,
			"The workload changed after the ticket, but its resources are not the values the ticket wrote ("+notTicket+
				"), so what recovered it is not this heal and it is not counted as a Dorgu heal.")
		return IncidentOutcomeSelfResolved
	}
	setCondition(&action.Status.Conditions, ConditionHealCredited, metav1.ConditionFalse, ReasonNothingPatched,
		"Nothing Dorgu proposed was applied to the workload (no patch recorded, or the Deployment's generation did not move), so this recovery is the application's own and is not counted as a Dorgu heal.")
	return IncidentOutcomeSelfResolved
}

// findCondition returns the condition of the given type, or nil.
func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// handleVerificationDegraded rolls the change back because health degraded.
func (r *RemediationController) handleVerificationDegraded(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	logger.Info("verification failed: health degraded, attempting rollback")
	return r.rollBack(ctx, logger, action, "HealthDegraded", "Remediation rolled back due to degraded health.")
}

// rollBack restores the ApplicationPersona and then states, on the record,
// what that did and did not reach.
//
// Two halves exist: the persona, which the operator writes and can restore, and
// the live workload, which the CLI may have patched with the user's own
// credentials and which the operator has no verbs to touch (CR5-02). The record
// has to say which half moved:
//
//   - persona restored and workload matches its pre-remediation values: phase
//     RolledBack, incident outcome "rollback";
//   - persona restored but the workload still carries the change: phase
//     RolledBack, incident outcome "partial", WorkloadDiverged=True with the
//     command that finishes the job;
//   - workload unreadable: RolledBack, outcome "rollback", and the condition
//     says Dorgu cannot tell;
//   - the rollback itself could not run: Failed, and the message says the change
//     is STILL APPLIED.
//
// reason and cause describe why the rollback was triggered.
func (r *RemediationController) rollBack(
	ctx context.Context,
	logger logr.Logger,
	action *dorguv1.RemediationAction,
	reason, cause string,
) (ctrl.Result, error) {
	persona := fmt.Sprintf("%s/%s", personaNamespaceOf(action), action.Spec.PersonaRef.Name)

	restoration, err := r.Rollback.Execute(ctx, action)
	if err != nil {
		logger.Error(err, "rollback failed")
		return ctrl.Result{}, r.transitionToFailed(ctx, action, fmt.Sprintf(
			"rollback failed: %v. %s The change is STILL APPLIED to ApplicationPersona %s and was NOT undone.",
			err, cause, persona))
	}

	action.Status.Phase = RemediationPhaseRolledBack

	// CR5-02: say what the rollback did NOT reach. The operator restored the
	// persona and cannot un-patch a Deployment, so a workload the CLI already
	// changed keeps the change. Leaving that unsaid is what let a user read
	// "RolledBack" and walk away from a cluster that had not rolled back.
	outcome := r.recordRollbackScope(ctx, logger, action)
	edited := recordPersonaEdits(action, restoration, persona)

	setCondition(&action.Status.Conditions, ConditionRolledBack, metav1.ConditionTrue, reason,
		cause+" "+rollbackScopeSentence(outcome, persona))

	if err := r.Status().Update(ctx, action); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status to RolledBack: %w", err)
	}
	// Counted once the write succeeded: a conflict requeues and the rollback runs again.
	metrics.RecordRollback(rollbackDivergedLabel(outcome))

	// A rollback that left the workload changed is partial, not a rollback.
	resolution := IncidentOutcomeRollback
	if outcome.Diverged() || edited {
		resolution = IncidentOutcomePartial
	}
	if err := r.updateIncidentResolution(ctx, action, resolution); err != nil {
		logFailure(logger, err, "failed to update incident resolution after rollback")
	}

	logger.Info("remediation rolled back", "reason", reason, "incidentOutcome", resolution)
	return ctrl.Result{}, nil
}

// recordPersonaEdits states on the action which persona fields the rollback left
// alone and why, and reports whether there were any (F4). The operator restores
// what Dorgu wrote; a field a human changed afterwards is theirs, and the record
// must say the rollback was partial instead of "RolledBack".
func recordPersonaEdits(action *dorguv1.RemediationAction, result *remediation.RollbackResult, persona string) bool {
	if !result.Partial() {
		setCondition(&action.Status.Conditions, ConditionPersonaEdited, metav1.ConditionFalse,
			"PersonaRestored", "Every persona field Dorgu changed was restored.")
		return false
	}
	reason := result.Skipped[0].Reason
	parts := make([]string, 0, len(result.Skipped))
	for _, f := range result.Skipped {
		if f.Reason != reason {
			reason = "Mixed"
		}
		switch f.Reason {
		case gate.ReasonRollbackHumanEdited:
			parts = append(parts, fmt.Sprintf("%s is %s, Dorgu wrote %s", f.Path, f.Current, f.Wrote))
		default:
			parts = append(parts, fmt.Sprintf("%s has no recorded baseline to restore (Dorgu wrote %s)", f.Path, f.Wrote))
		}
	}
	setCondition(&action.Status.Conditions, ConditionPersonaEdited, metav1.ConditionTrue, reason,
		fmt.Sprintf("The rollback left %d field(s) of ApplicationPersona %s as they are, so it is partial: %s. "+
			"A rollback restores what Dorgu changed, never a later edit.", len(result.Skipped), persona, strings.Join(parts, "; ")))
	return true
}

// rollbackScopeSentence is the one sentence on the RolledBack condition that
// says which half was restored.
func rollbackScopeSentence(outcome remediation.RollbackOutcome, persona string) string {
	switch outcome.Reason() {
	case remediation.ReasonWorkloadDiverged:
		return fmt.Sprintf("The persona (%s) was restored; the live workload was NOT, and still carries the change. "+
			"See the %s condition for what to revert.", persona, ConditionWorkloadDiverged)
	case remediation.ReasonWorkloadUnreadable:
		return fmt.Sprintf("The persona (%s) was restored; Dorgu could not read the live workload, so it cannot say "+
			"whether the workload still carries the change. See the %s condition.", persona, ConditionWorkloadDiverged)
	default:
		return fmt.Sprintf("The persona (%s) was restored and the live workload matches its pre-remediation values.", persona)
	}
}

// personaNamespaceOf is the namespace the action's persona lives in.
func personaNamespaceOf(action *dorguv1.RemediationAction) string {
	if action.Spec.PersonaRef.Namespace != "" {
		return action.Spec.PersonaRef.Namespace
	}
	return action.Namespace
}

// recordRollbackScope writes the WorkloadDiverged condition and logs the
// divergence, so the fact is available to a client reading the CRD, to anyone
// reading the operator log, and to `dorgu remediation` without either of them
// having to diff the Deployment by hand.
func (r *RemediationController) recordRollbackScope(
	ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction,
) remediation.RollbackOutcome {
	outcome := r.Rollback.InspectRollback(ctx, action)

	status := metav1.ConditionFalse
	switch outcome.Reason() {
	case remediation.ReasonWorkloadDiverged:
		status = metav1.ConditionTrue
	case remediation.ReasonWorkloadUnreadable:
		status = metav1.ConditionUnknown
	}

	setCondition(&action.Status.Conditions, ConditionWorkloadDiverged, status,
		outcome.Reason(), outcome.Message())

	if !outcome.Diverged() {
		logger.V(1).Info("rollback scope recorded", "reason", outcome.Reason())
		return outcome
	}

	fields := make([]string, 0, len(outcome.Divergences))
	for _, d := range outcome.Divergences {
		fields = append(fields, fmt.Sprintf("%s live=%s pre-remediation=%s", d.Field, d.Live, d.Intended))
	}
	logger.Info("rollback restored the persona but the live workload still carries the change; Dorgu has no write access to Deployments",
		"deployment", fmt.Sprintf("%s/%s", outcome.Ref.Namespace, outcome.Ref.Name),
		"container", outcome.Ref.Container,
		"managedBy", outcome.Ref.ManagedBy,
		"diverged", fields)
	return outcome
}

// handleVerificationUnknown retries verification, and rolls the change back
// when it still cannot be verified after maxUnknownRetries attempts.
//
// A change nobody can confirm is healthy must not be left applied, and the
// retries must terminate: the counter lives in the Verified condition's message,
// which setCondition used to leave alone when status and reason were unchanged,
// so it stuck at 1/2 and the action sat in Verifying forever.
func (r *RemediationController) handleVerificationUnknown(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	retryCount := r.getRetryCount(action)

	if retryCount >= maxUnknownRetries {
		logger.Info("verification still Unknown after the maximum retries, rolling back",
			"retries", retryCount)
		return r.rollBack(ctx, logger, action, "VerificationUnknown", fmt.Sprintf(
			"Remediation rolled back because its effect could not be verified after %d retries.", maxUnknownRetries))
	}

	// Advance the counter. upsertCondition rewrites the message on every call;
	// setCondition would keep "retry 1/2" for as long as status and reason stay put.
	upsertCondition(&action.Status.Conditions, ConditionVerified, metav1.ConditionFalse, unknownRetryReason(retryCount+1),
		fmt.Sprintf("Verification returned Unknown, retry %d/%d", retryCount+1, maxUnknownRetries))

	if err := r.Status().Update(ctx, action); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating retry count: %w", err)
	}

	logger.Info("verification unknown, will retry", "retry", retryCount+1, "maxRetries", maxUnknownRetries)
	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

// transitionToAcknowledged settles an approved advisory plan. The approval is
// recorded, the manual steps stand as the plan of record, and the incident is
// marked acknowledged rather than resolved: nothing was actually changed.
func (r *RemediationController) transitionToAcknowledged(ctx context.Context, action *dorguv1.RemediationAction) error {
	action.Status.Phase = RemediationPhaseAcknowledged
	setCondition(&action.Status.Conditions, ConditionApplied, metav1.ConditionFalse,
		dorguv1.ReasonAdvisoryOnly,
		"Approval recorded. This plan has no auto-applicable step, so the operator changed nothing; the steps are for a human to apply.")

	if err := r.Status().Update(ctx, action); err != nil {
		return fmt.Errorf("updating status to Acknowledged: %w", err)
	}

	if err := r.updateIncidentResolution(ctx, action, IncidentOutcomeAcknowledged); err != nil {
		r.Logger.Error(err, "failed to record incident acknowledgement")
	}

	return nil
}

// transitionToFailed sets the action to Failed phase with a reason.
func (r *RemediationController) transitionToFailed(ctx context.Context, action *dorguv1.RemediationAction, reason string) error {
	return r.transitionToFailedWithReason(ctx, action, "Failed", reason)
}

// transitionToFailedWithReason sets the action to Failed with an explicit
// condition reason. The reason is load-bearing: the safety checker reads it to
// tell a rejected-before-apply action (no cluster change, no cooldown) from a
// remediation that actually went wrong.
func (r *RemediationController) transitionToFailedWithReason(
	ctx context.Context,
	action *dorguv1.RemediationAction,
	conditionReason string,
	reason string,
) error {
	action.Status.Phase = RemediationPhaseFailed
	setCondition(&action.Status.Conditions, ConditionApplied, metav1.ConditionFalse, conditionReason, reason)

	if err := r.Status().Update(ctx, action); err != nil {
		return fmt.Errorf("updating status to Failed: %w", err)
	}

	// Update incident with failure info.
	if err := r.updateIncidentResolution(ctx, action, IncidentOutcomeFailed); err != nil {
		logFailure(r.Logger, err, "failed to update incident resolution on failure")
	}

	return nil
}

// updateIncidentResolution populates the IncidentMemory resolution fields.
func (r *RemediationController) updateIncidentResolution(ctx context.Context, action *dorguv1.RemediationAction, outcome string) error {
	namespace := action.Spec.IncidentRef.Namespace
	if namespace == "" {
		namespace = action.Namespace
	}

	var incident dorguv1.IncidentMemory
	key := client.ObjectKey{
		Name:      action.Spec.IncidentRef.Name,
		Namespace: namespace,
	}
	if err := r.Get(ctx, key, &incident); err != nil {
		return fmt.Errorf("getting IncidentMemory %s: %w", key, err)
	}

	// Calculate duration from detection to now.
	var duration *metav1.Duration
	detectionTime := incident.Spec.Detection.FirstSeen.Time
	if !detectionTime.IsZero() {
		d := time.Since(detectionTime)
		duration = &metav1.Duration{Duration: d}
	}

	incident.Spec.Resolution = &dorguv1.ResolutionInfo{
		Action: action.Spec.Explanation,
		RemediationRef: &dorguv1.RemediationReference{
			Name:      action.Name,
			Namespace: action.Namespace,
		},
		AppliedAt: action.Status.AppliedAt,
		Outcome:   outcome,
		Duration:  duration,
	}

	// Set the resolved phase label on the metadata here (before the spec
	// update) so it lands on the same write — labels are metadata, so a
	// Status().Update() below cannot persist them.
	if outcome == IncidentOutcomeResolved || outcome == IncidentOutcomeSelfResolved {
		if incident.Labels == nil {
			incident.Labels = make(map[string]string)
		}
		incident.Labels[LabelPhase] = PhaseResolved
	}

	if err := r.Update(ctx, &incident); err != nil {
		return fmt.Errorf("updating IncidentMemory spec: %w", err)
	}
	metrics.RecordIncidentResolved(outcome)

	// If resolved, update incident status phase. Re-fetch inside the retry
	// loop so we pick up the new ResourceVersion after our own spec update
	// (and any concurrent writes from the healthcheck reconciler).
	if outcome == IncidentOutcomeResolved || outcome == IncidentOutcomeSelfResolved {
		statusErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, key, &incident); err != nil {
				return err
			}
			incident.Status.Phase = PhaseResolved
			resolveProposalRefusal(&incident.Status.Conditions)
			return r.Status().Update(ctx, &incident)
		})
		if statusErr != nil {
			return fmt.Errorf("updating IncidentMemory status: %w", statusErr)
		}
	}

	return nil
}

// getVerificationDelay returns the duration to wait before verification.
func (r *RemediationController) getVerificationDelay(action *dorguv1.RemediationAction) time.Duration {
	if action.Spec.Rollback != nil && action.Spec.Rollback.HealthCheckAfter != nil {
		return action.Spec.Rollback.HealthCheckAfter.Duration
	}
	return defaultVerificationWait
}

// unknownRetryReasons are the Verified reasons that count Unknown verification
// retries: index n is the reason once n retries have been recorded. The count is
// the reason enum, read back by equality, so no message is ever parsed (KPI I5).
var unknownRetryReasons = [...]string{"", "VerificationUnknownRetry1", "VerificationUnknownRetry2"}

// unknownRetryReason is the Verified reason after n recorded retries.
func unknownRetryReason(n int) string { return unknownRetryReasons[n] }

// getRetryCount returns how many Unknown verification retries the Verified
// condition records.
func (r *RemediationController) getRetryCount(action *dorguv1.RemediationAction) int {
	cond := findCondition(action.Status.Conditions, ConditionVerified)
	if cond == nil {
		return 0
	}
	for n := len(unknownRetryReasons) - 1; n > 0; n-- {
		if cond.Reason == unknownRetryReasons[n] {
			return n
		}
	}
	return 0
}

// SetupWithManager registers the RemediationController with the manager.
func (r *RemediationController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dorguv1.RemediationAction{}).
		Complete(r)
}

// recordVerification counts a verification result, and, when it is conclusive,
// how long the heal took from applied to verified.
func recordVerification(action *dorguv1.RemediationAction, result remediation.VerificationResult) {
	switch result {
	case remediation.VerificationHealthy:
		metrics.RecordVerification(metrics.VerificationHealthy)
	case remediation.VerificationDegraded:
		metrics.RecordVerification(metrics.VerificationDegraded)
	default:
		metrics.RecordVerification(metrics.VerificationUnknown)
		return
	}
	if action.Status.AppliedAt != nil {
		metrics.RecordStageDuration(metrics.StageAppliedToVerified, time.Since(action.Status.AppliedAt.Time))
	}
}
