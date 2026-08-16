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
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/websocket"
)

// RemediationAction phase constants.
const (
	RemediationPhasePending   = "Pending"
	RemediationPhaseApproved  = "Approved"
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
	Executor  *remediation.Executor
	Verifier  *remediation.Verifier
	Rollback  *remediation.Rollback
	Logger    logr.Logger
	WebSocket *websocket.Server
}

// Reconcile drives the RemediationAction through its lifecycle state machine.
func (r *RemediationController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Logger.WithValues("remediationaction", req.NamespacedName)

	var action dorguv1.RemediationAction
	if err := r.Get(ctx, req.NamespacedName, &action); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	switch action.Status.Phase {
	case RemediationPhasePending:
		// Waiting for human approval — no action.
		return ctrl.Result{}, nil

	case RemediationPhaseApproved:
		return r.handleApproved(ctx, logger, &action)

	case RemediationPhaseApplying:
		return r.handleApplying(ctx, logger, &action)

	case RemediationPhaseVerifying:
		return r.handleVerifying(ctx, logger, &action)

	case RemediationPhaseCompleted, RemediationPhaseAcknowledged, RemediationPhaseRolledBack,
		RemediationPhaseFailed, RemediationPhaseRejected, RemediationPhaseExpired:
		// Terminal states — no-op.
		return ctrl.Result{}, nil

	default:
		logger.V(1).Info("unknown phase, ignoring", "phase", action.Status.Phase)
		return ctrl.Result{}, nil
	}
}

// handleApproved applies the remediation patch and transitions to Applying.
//
// An approved plan with nothing to apply settles as Acknowledged instead: its
// steps are advisory, so there is no patch to run and nothing has gone wrong.
// Sending it to the executor is what used to mark it Failed and put the app into
// a 30-minute remediation blackout (F-03).
func (r *RemediationController) handleApproved(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	if !action.HasAutoApplicableChange() {
		logger.Info("approved plan is advisory; recording the approval without applying anything",
			"actionType", action.Spec.Action.Type, "steps", len(action.Spec.Steps))
		return ctrl.Result{}, r.transitionToAcknowledged(ctx, action)
	}

	logger.Info("executing approved remediation")

	if err := r.Executor.Apply(ctx, action); err != nil {
		var precondition *remediation.PreconditionError
		if errors.As(err, &precondition) {
			// Nothing was written to the cluster, so this must not count as a
			// failed remediation for the cooldown.
			logger.Error(err, "remediation refused before apply")
			return ctrl.Result{}, r.transitionToFailedWithReason(ctx, action,
				dorguv1.ReasonPreconditionRejected,
				fmt.Sprintf("%v; nothing was applied", err))
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

	r.broadcastRemediation(action, "approved")

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

	switch result {
	case remediation.VerificationHealthy:
		return r.handleVerificationHealthy(ctx, logger, action)

	case remediation.VerificationDegraded:
		return r.handleVerificationDegraded(ctx, logger, action)

	case remediation.VerificationUnknown:
		return r.handleVerificationUnknown(ctx, logger, action)

	default:
		return ctrl.Result{}, r.transitionToFailed(ctx, action, fmt.Sprintf("unexpected verification result: %s", result))
	}
}

// handleVerificationHealthy completes the remediation and updates the incident.
func (r *RemediationController) handleVerificationHealthy(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	action.Status.Phase = RemediationPhaseCompleted
	setCondition(&action.Status.Conditions, ConditionVerified, metav1.ConditionTrue, "HealthRestored", "Verification confirmed health restored")

	if err := r.Status().Update(ctx, action); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status to Completed: %w", err)
	}

	r.broadcastRemediation(action, "completed")

	// Update incident with resolution info.
	if err := r.updateIncidentResolution(ctx, action, "resolved"); err != nil {
		logger.Error(err, "failed to update incident resolution")
	}

	logger.Info("remediation completed successfully")
	return ctrl.Result{}, nil
}

// handleVerificationDegraded triggers rollback if enabled.
func (r *RemediationController) handleVerificationDegraded(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	logger.Info("verification failed: health degraded, attempting rollback")

	if err := r.Rollback.Execute(ctx, action); err != nil {
		logger.Error(err, "rollback failed")
		return ctrl.Result{}, r.transitionToFailed(ctx, action, fmt.Sprintf("rollback failed: %v", err))
	}

	action.Status.Phase = RemediationPhaseRolledBack
	setCondition(&action.Status.Conditions, ConditionRolledBack, metav1.ConditionTrue, "HealthDegraded", "Remediation rolled back due to degraded health")

	if err := r.Status().Update(ctx, action); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status to RolledBack: %w", err)
	}

	r.broadcastRemediation(action, "rolledback")

	// Update incident with rollback info.
	if err := r.updateIncidentResolution(ctx, action, "rollback"); err != nil {
		logger.Error(err, "failed to update incident resolution after rollback")
	}

	logger.Info("remediation rolled back")
	return ctrl.Result{}, nil
}

// handleVerificationUnknown retries verification or fails after max retries.
func (r *RemediationController) handleVerificationUnknown(ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction) (ctrl.Result, error) {
	retryCount := r.getRetryCount(action)

	if retryCount >= maxUnknownRetries {
		logger.Info("max verification retries exceeded, failing remediation")
		return ctrl.Result{}, r.transitionToFailed(ctx, action, "verification returned Unknown after max retries")
	}

	// Increment retry count via condition message.
	setCondition(&action.Status.Conditions, ConditionVerified, metav1.ConditionFalse, "VerificationUnknown",
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

	r.broadcastRemediation(action, "acknowledged")

	if err := r.updateIncidentResolution(ctx, action, "acknowledged"); err != nil {
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

	r.broadcastRemediation(action, "failed")

	// Update incident with failure info.
	if err := r.updateIncidentResolution(ctx, action, "failed"); err != nil {
		r.Logger.Error(err, "failed to update incident resolution on failure")
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
	if outcome == "resolved" {
		if incident.Labels == nil {
			incident.Labels = make(map[string]string)
		}
		incident.Labels[LabelPhase] = PhaseResolved
	}

	if err := r.Update(ctx, &incident); err != nil {
		return fmt.Errorf("updating IncidentMemory spec: %w", err)
	}

	// If resolved, update incident status phase. Re-fetch inside the retry
	// loop so we pick up the new ResourceVersion after our own spec update
	// (and any concurrent writes from the healthcheck reconciler).
	if outcome == "resolved" {
		statusErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, key, &incident); err != nil {
				return err
			}
			incident.Status.Phase = PhaseResolved
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

// getRetryCount extracts the current retry count from the Verified condition message.
func (r *RemediationController) getRetryCount(action *dorguv1.RemediationAction) int {
	for _, c := range action.Status.Conditions {
		if c.Type == ConditionVerified && c.Reason == "VerificationUnknown" {
			var attempt, maxAttempts int
			if _, err := fmt.Sscanf(c.Message, "Verification returned Unknown, retry %d/%d",
				&attempt, &maxAttempts); err == nil {
				return attempt
			}
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

// broadcastRemediation emits a WebSocket event describing a RemediationAction
// phase transition. Safe to call with a nil WebSocket server.
func (r *RemediationController) broadcastRemediation(action *dorguv1.RemediationAction, eventType string) {
	if r.WebSocket == nil {
		return
	}
	r.WebSocket.BroadcastRemediation(websocket.RemediationEvent{
		EventType:   eventType,
		Name:        action.Name,
		Namespace:   action.Namespace,
		ActionType:  action.Spec.Action.Type,
		Phase:       action.Status.Phase,
		Confidence:  action.Spec.Confidence,
		PersonaName: action.Spec.PersonaRef.Name,
		PersonaKind: action.Spec.PersonaRef.Kind,
	})
}
