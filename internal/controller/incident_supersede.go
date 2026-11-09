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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// Superseding a proposal for the milder shape (re-review N-2).
//
// An incident that escalates keeps its name, so a proposal made for its milder shape
// is, by name, still the incident's outstanding proposal and the gate would defer the
// worse shape's behind it for as long as it waits (the pending TTL). It is advice for
// a fault that has since become a worse one, so a Pending proposal is expired with the
// reason Superseded, and the worse shape is proposed in the same cycle.
//
// Only a Pending proposal for a strictly milder shape is superseded, matched by
// lineage as the gate's dedup is (see overtakenByEscalation).
//
// Only Pending is superseded. Approved, Ticketed, Applying and Verifying mean a human
// or the operator has acted on it; it is not the operator's to withdraw, and the worse
// shape defers until it finishes. Superseded is a form of Expired: terminal, nothing
// applied, not a rejection, no cooldown. Approving one afterwards does not stick.

// supersedePending expires the incident's Pending proposals. from is the signal the
// incident had, for the message.
func (r *HealthCheckReconciler) supersedePending(ctx context.Context, incident *dorguv1.IncidentMemory, from string) error {
	var actions dorguv1.RemediationActionList
	if err := r.Client.List(ctx, &actions, client.InNamespace(incident.Namespace)); err != nil {
		return fmt.Errorf("listing RemediationActions to supersede: %w", err)
	}
	var errs []error
	for i := range actions.Items {
		ra := &actions.Items[i]
		if !gate.SameIncidentLineage(ra.Spec.IncidentRef.Name, incident.Name) || ra.Status.Phase != RemediationPhasePending {
			continue
		}
		if !overtakenByEscalation(ra, incident) {
			continue
		}
		if err := r.supersede(ctx, client.ObjectKeyFromObject(ra), from, incident.Spec.Detection.Signal); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// overtakenByEscalation reports whether a Pending proposal is advice the incident's
// escalation has overtaken. Only a proposal for a strictly milder shape is.
//
//   - It says what it answered (dorgu.io/answers-signal): superseded only if that
//     shape ranks below the shape the incident has now. One that answers the current
//     shape or a worse one still fits (an OOM, a readiness, an OOM again must not
//     expire the standing OOM proposal and mint another), and a signal outside the
//     progression has no rank to compare, so it is not provably as severe and goes.
//   - It does not say (made before proposals recorded it): superseded only if the
//     incident escalated after it was created, which is when it can only have answered
//     an earlier, milder shape. With no escalated-at, an unreadable one, or one that is
//     not after the proposal, it is kept: leaving a human's queue alone and letting the
//     gate defer is the safe direction. The stamp has whole-second resolution, as the
//     proposal's creation time does, so the same second is not provably after.
func overtakenByEscalation(ra *dorguv1.RemediationAction, incident *dorguv1.IncidentMemory) bool {
	if answered := ra.Annotations[gate.AnnotationAnswersSignal]; answered != "" {
		was, wasRanked := rankOf(answered)
		now, nowRanked := rankOf(incident.Spec.Detection.Signal)
		return !(wasRanked && nowRanked && was >= now)
	}
	escalated, err := time.Parse(time.RFC3339, incident.Annotations[gate.AnnotationEscalatedAt])
	return err == nil && escalated.After(ra.CreationTimestamp.Time)
}

// supersede expires one proposal, re-reading it first: a human may have approved it
// since the list.
func (r *HealthCheckReconciler) supersede(ctx context.Context, key client.ObjectKey, from, to string) error {
	var superseded *dorguv1.RemediationAction
	err := retryIncidentWrite(func(int) error {
		var ra dorguv1.RemediationAction
		if err := r.Client.Get(ctx, key, &ra); err != nil {
			return err
		}
		if ra.Status.Phase != RemediationPhasePending {
			superseded = nil
			return nil
		}
		ra.Status.Phase = RemediationPhaseExpired
		setCondition(&ra.Status.Conditions, ConditionExpired, metav1.ConditionTrue, ReasonSuperseded, fmt.Sprintf(
			"Superseded: the incident this proposal answered (%s) has become a worse one (%s), so this advice no longer fits. "+
				"Nothing was applied. It is not a rejection and starts no cooldown; the worse shape is proposed for separately.",
			from, to))
		if err := r.Client.Status().Update(ctx, &ra); err != nil {
			return err
		}
		superseded = &ra
		return nil
	})
	if err != nil {
		return fmt.Errorf("superseding %s: %w", key, err)
	}
	if superseded == nil {
		return nil
	}
	metrics.RecordTransition(RemediationPhasePending, RemediationPhaseExpired)
	emit(r.Recorder, superseded, corev1.EventTypeNormal, EventReasonSuperseded,
		fmt.Sprintf("superseded: its incident escalated from %s to %s", from, to))
	return nil
}

// reassertSuperseded puts a superseded proposal back to Expired if anything moved it.
// The CLI approves only a Pending proposal, but status is writable by whoever holds
// that RBAC (a known P1 gap, P2 task 2.5), so the operator does not take the phase on
// trust: a proposal it superseded stays superseded, with no ticket.
func (r *RemediationController) reassertSuperseded(ctx context.Context, action *dorguv1.RemediationAction) (bool, error) {
	if action.Status.Phase == RemediationPhaseExpired || !isSuperseded(action) {
		return false, nil
	}
	action.Status.Phase = RemediationPhaseExpired
	action.Status.ApplyTicket = nil
	if err := r.Status().Update(ctx, action); err != nil {
		return true, fmt.Errorf("restoring a superseded proposal to Expired: %w", err)
	}
	emit(r.Recorder, action, corev1.EventTypeWarning, EventReasonSuperseded,
		"this proposal was superseded and cannot be approved; it was put back to Expired")
	return true, nil
}

func isSuperseded(action *dorguv1.RemediationAction) bool {
	for i := range action.Status.Conditions {
		c := &action.Status.Conditions[i]
		if c.Type == ConditionExpired && c.Status == metav1.ConditionTrue && c.Reason == ReasonSuperseded {
			return true
		}
	}
	return false
}
