package controller

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// Persisted refusals (B6, task 1.10). Every refusal the gate makes ends up as
// three things: a status condition with a reason enum, a Kubernetes Event named
// Dorgu<Stage>Refused, and a count in dorgu_gate_decisions_total (the gate counts
// that itself, on every run). This file is the first two, and the observability
// that goes with tickets. None of it decides anything: the gate decided, and this
// reports it. Nothing here reads a message back; the enum is the record.

const (
	// EventReasonTicketIssued is the Event reason when the approval re-check issues
	// an apply ticket.
	EventReasonTicketIssued = "DorguTicketIssued"

	// EventReasonExpired is the Event reason when a proposal missed its approval
	// window, or was approved after it.
	EventReasonExpired = "DorguExpired"

	// EventReasonSuperseded is the Event reason when a Pending proposal was expired
	// because its incident escalated to a worse shape.
	EventReasonSuperseded = "DorguSuperseded"

	// maxEventMessage bounds an Event message; the apiserver allows 1024.
	maxEventMessage = 512
)

// refusalEventReason is the Event reason for a refusal at a stage:
// DorguGroundRefused, DorguModePolicyRefused, DorguTicketRefused, ...
func refusalEventReason(stage string) string { return "Dorgu" + stage + "Refused" }

// proposalRefusalReason is the ConditionProposalRefused reason: the stage and the
// guardrail, both CamelCase, so a reader can split it without reading a message.
func proposalRefusalReason(stage, guardrail string) string {
	return stage + "_" + gate.ReasonEnum(guardrail)
}

// emit records a Kubernetes Event on obj. With no recorder configured (a test, or
// a build with events off) it does nothing.
func emit(rec record.EventRecorder, obj runtime.Object, eventType, reason, message string) {
	if rec == nil {
		return
	}
	if len(message) > maxEventMessage {
		message = truncateRunes(message, maxEventMessage-len("…")) + "…"
	}
	rec.Event(obj, eventType, reason, message)
}

// --- proposal-time refusals: no RemediationAction exists to carry them ---------

// recordProposalRefusal persists a proposal the gate refused on the incident it
// was for: a ProposalRefused condition and a Warning Event. The same refusal
// every cycle writes and emits once: the condition is only rewritten when its
// reason changes, and the Event goes out with it.
//
// Only the gate's refusals are recorded. A skip that is the proposer's own (a
// restart is advice, nothing to adjust) is not a refusal and stays a log line.
func (r *HealthCheckReconciler) recordProposalRefusal(
	ctx context.Context, incident *dorguv1.IncidentMemory, result *remediation.ProposalResult,
) {
	reason := proposalRefusalReason(result.Stage, result.Reason)
	changed := false
	err := retryIncidentWrite(func(int) error {
		var fresh dorguv1.IncidentMemory
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(incident), &fresh); err != nil {
			return err
		}
		if cond := findCondition(fresh.Status.Conditions, dorguv1.ConditionProposalRefused); cond != nil &&
			cond.Status == metav1.ConditionTrue && cond.Reason == reason {
			changed = false
			return nil
		}
		setCondition(&fresh.Status.Conditions, dorguv1.ConditionProposalRefused, metav1.ConditionTrue, reason, result.SkipReason)
		// The durable record, which nothing clears (B10 f): ProposalRefused is rewritten when the
		// incident resolves or a proposal answers it, and `dorgu report` could then not count
		// a refusal that had happened. The message carries no plan text.
		setCondition(&fresh.Status.Conditions, dorguv1.ConditionProposalRefusalRecorded, metav1.ConditionTrue, reason,
			"the gate refused a proposal for this incident; this record is kept after ProposalRefused is cleared")
		changed = true
		return r.Client.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logFailure(r.Logger, err, "could not record the proposal refusal on the incident", "incident", incident.Name)
		return
	}
	if changed {
		emit(r.Recorder, incident, corev1.EventTypeWarning, refusalEventReason(result.Stage), result.SkipReason)
	}
}

// clearProposalRefusal marks a standing refusal as answered once a proposal was
// made, so the incident does not keep saying it was refused.
func (r *HealthCheckReconciler) clearProposalRefusal(ctx context.Context, incident *dorguv1.IncidentMemory, reason, message string) {
	err := retryIncidentWrite(func(int) error {
		var fresh dorguv1.IncidentMemory
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(incident), &fresh); err != nil {
			return err
		}
		cond := findCondition(fresh.Status.Conditions, dorguv1.ConditionProposalRefused)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			return nil
		}
		setCondition(&fresh.Status.Conditions, dorguv1.ConditionProposalRefused, metav1.ConditionFalse, reason, message)
		return r.Client.Status().Update(ctx, &fresh)
	})
	if err != nil {
		logFailure(r.Logger, err, "could not clear the proposal refusal on the incident", "incident", incident.Name)
	}
}

// earliestSignal is when the oldest contributing signal was detected, for the
// time-to-detect measurement. ok is false when no signal carries a time.
func earliestSignal(diag *diagnosis.Diagnosis) (time.Time, bool) {
	var earliest time.Time
	for _, cs := range diag.Contributing {
		at := cs.Signal.DetectedAt
		if at.IsZero() {
			continue
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest, !earliest.IsZero()
}

// --- RemediationAction: approval, ticket and apply -----------------------------

// reportApprovalRefusal is the Event and the count for an approval the gate
// refused. The condition (GateRefused) and the Failed phase are written by the
// caller, in the same status update.
func (r *RemediationController) reportApprovalRefusal(action *dorguv1.RemediationAction, stage, message string) {
	metrics.RecordTicket(metrics.TicketRefusedAtApproval)
	emit(r.Recorder, action, corev1.EventTypeWarning, refusalEventReason(stage), message)
}

// reportTicketIssued is the Event and the count for a ticket the approval
// re-check issued.
func (r *RemediationController) reportTicketIssued(action *dorguv1.RemediationAction, ticket *dorguv1.ApplyTicket) {
	metrics.RecordTicket(metrics.TicketIssued)
	emit(r.Recorder, action, corev1.EventTypeNormal, EventReasonTicketIssued,
		fmt.Sprintf("the gate re-checked this approval on live state and issued an apply ticket valid until %s",
			ticket.ExpiresAt.UTC().Format(time.RFC3339)))
}

// reportApplyRefusal is the Event and the count for a ticket the apply-time check
// did not honour. An expired ticket is counted as expired and nothing else.
func (r *RemediationController) reportApplyRefusal(action *dorguv1.RemediationAction, guardrail, message string) {
	if guardrail == gate.GuardrailTicketExpired {
		metrics.RecordTicket(metrics.TicketExpired)
	} else {
		metrics.RecordTicket(metrics.TicketRefusedAtApply)
	}
	emit(r.Recorder, action, corev1.EventTypeWarning, refusalEventReason(gate.StageTicket), message)
}

// observeDecisionTime records how long a proposal waited for its decision.
func observeDecisionTime(action *dorguv1.RemediationAction, decidedAt time.Time) {
	if action.CreationTimestamp.IsZero() {
		return
	}
	metrics.RecordStageDuration(metrics.StageProposalToDecision, decidedAt.Sub(action.CreationTimestamp.Time))
}

// recordHealCredit counts a healthy verification as a credited heal or not, and
// why, from the conditions the controller has just written. The reasons are the
// enum reasons of those conditions; nothing is read from a message.
func recordHealCredit(action *dorguv1.RemediationAction, outcome string) {
	switch outcome {
	case IncidentOutcomeResolved:
		if c := findCondition(action.Status.Conditions, ConditionHealCredited); c != nil && c.Status == metav1.ConditionTrue {
			metrics.RecordHealCredit(true, metrics.HealReasonWorkloadPatched)
		}
	case IncidentOutcomeSelfResolved:
		reason := metrics.HealReasonNothingPatched
		if c := findCondition(action.Status.Conditions, ConditionHealCredited); c != nil && c.Reason == ReasonResourcesNotTicket {
			reason = metrics.HealReasonResourcesNotTicket
		}
		metrics.RecordHealCredit(false, reason)
	case IncidentOutcomePartial:
		reason := metrics.HealReasonWorkloadDiverged
		if c := findCondition(action.Status.Conditions, ConditionWorkloadDiverged); c != nil && c.Status == metav1.ConditionUnknown {
			reason = metrics.HealReasonWorkloadUnreadable
		}
		metrics.RecordHealCredit(false, reason)
	}
}

// rollbackDivergedLabel is the workload_diverged label of dorgu_rollbacks_total.
func rollbackDivergedLabel(outcome remediation.RollbackOutcome) string {
	switch outcome.Reason() {
	case remediation.ReasonWorkloadDiverged:
		return "true"
	case remediation.ReasonWorkloadUnreadable:
		return "unknown"
	default:
		return "false"
	}
}

// truncateRunes cuts s to at most max bytes without splitting a UTF-8 rune.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// resolveProposalRefusal marks a standing ProposalRefused condition as no longer
// true because the incident was resolved.
func resolveProposalRefusal(conditions *[]metav1.Condition) {
	cond := findCondition(*conditions, dorguv1.ConditionProposalRefused)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return
	}
	setCondition(conditions, dorguv1.ConditionProposalRefused, metav1.ConditionFalse,
		dorguv1.ReasonIncidentResolved, "the incident was resolved")
}
