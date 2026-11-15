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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// EventReasonApprovalVoided is the Warning Event on an approval the operator sent back.
const EventReasonApprovalVoided = "DorguApprovalVoided"

// ApprovalWrittenAt is when the API server recorded the status write that approved the action:
// the managedFields time of the one status-subresource entry that owns status.approvedBy.
//
// It is the API server's stamp, not status.approvedAt, which is written by whoever approves and
// can say anything. It is an error, and a caller must treat it as "unknown", when no entry owns
// approvedBy, more than one does (the writer is ambiguous), or the entry has no time.
func ApprovalWrittenAt(action *dorguv1.RemediationAction) (time.Time, error) {
	var found *metav1.ManagedFieldsEntry
	for i := range action.ManagedFields {
		e := &action.ManagedFields[i]
		if e.Subresource != "status" || e.FieldsV1 == nil {
			continue
		}
		var fields map[string]map[string]json.RawMessage
		if err := json.Unmarshal(e.FieldsV1.Raw, &fields); err != nil {
			// An entry that cannot be read could own it: do not skip it quietly.
			return time.Time{}, fmt.Errorf("a managedFields entry (manager %q) cannot be read: %w", e.Manager, err)
		}
		if _, owns := fields["f:status"]["f:approvedBy"]; !owns {
			continue
		}
		if found != nil {
			return time.Time{}, errors.New("more than one writer owns status.approvedBy, so who wrote the approval, and when, is ambiguous")
		}
		found = e
	}
	switch {
	case found == nil:
		return time.Time{}, errors.New("no status write in managedFields owns status.approvedBy")
	case found.Time == nil || found.Time.IsZero():
		return time.Time{}, fmt.Errorf("the managedFields entry of manager %q has no time", found.Manager)
	}
	return found.Time.Time, nil
}

// approvalVoidedBy says why the approval cannot be honoured, or "" when it can: it was written
// while the ticket guard was not ready (closed, in its startup window, or open but unable to
// have noticed a deleted policy), so no working admission policy checked who gave it. When the
// time of the write cannot be established the answer is the same, because an approval of
// unknown age may be one of those (fail closed).
func (r *RemediationController) approvalVoidedBy(action *dorguv1.RemediationAction) string {
	if r.TicketGuard == nil || !r.TicketGuard.EverNotReady() {
		return ""
	}
	at, err := ApprovalWrittenAt(action)
	if err != nil {
		return fmt.Sprintf("the operator could not tell when it was written (%v)", err)
	}
	if r.TicketGuard.NotReadyAt(at) {
		return fmt.Sprintf("it was written at %s, while the operator's admission checks were not ready, so no admission policy was known to be checking who gave it",
			at.UTC().Format(time.RFC3339))
	}
	return ""
}

// voidApproval sends an approved action back to Pending: the approval is cleared, a condition
// and an Event say why, and a person has to approve it again. The proposal's gate record stays,
// so the new approval is re-checked against live state like any other. Nothing was applied.
func (r *RemediationController) voidApproval(
	ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction, why string,
) error {
	message := "This approval is not honoured: " + why + ". Nothing was applied. Please approve it again."
	logger.Info("an approval written while the admission checks were not ready is sent back for a new approval", "why", why)

	action.Status.Phase = RemediationPhasePending
	action.Status.ApprovedBy = ""
	action.Status.ApprovedAt = nil
	action.Status.ApplyTicket = nil
	setCondition(&action.Status.Conditions, dorguv1.ConditionApprovalVoided, metav1.ConditionTrue,
		dorguv1.ReasonAdmissionGuardWasNotReady, message)
	if err := r.Status().Update(ctx, action); err != nil {
		return fmt.Errorf("sending the approval back to Pending: %w", err)
	}
	emit(r.Recorder, action, corev1.EventTypeWarning, EventReasonApprovalVoided, message)
	return nil
}
