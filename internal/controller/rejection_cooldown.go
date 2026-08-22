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

	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

const (
	// RejectionCooldown is how long a rejection holds before dorgu will propose
	// the same fix for the same incident again.
	//
	// Rejecting used to buy about 30 seconds: the health check re-proposed on the
	// next cycle, and with the AI planner configured every re-proposal is another
	// billed call, so declining cost the user money (F-07). An hour is long
	// enough that saying no is respected, short enough that a problem still
	// present an hour later gets raised again.
	RejectionCooldown = 1 * time.Hour

	// ConditionRejected records that a human declined a RemediationAction, and
	// when. The phase alone cannot carry the timestamp.
	ConditionRejected = "Rejected"

	// ReasonUserRejected is the ConditionRejected reason for a human decision.
	ReasonUserRejected = "UserRejected"
)

// rejectionSuppressesProposal reports whether a prior rejection should stop this
// proposal, returning a human-readable reason (empty when it should proceed).
//
// The suppression is scoped to the incident and its target, per CF4-2: a
// rejection is a no to one fix for one incident, not a blanket mute on the
// persona. It lifts when either the cooldown expires or the signal materially
// changes, which here means the live diagnosis is more severe than the incident
// the user declined. A warning someone waved off that has since gone critical is
// a different question, so dorgu is allowed to ask it.
//
// Deliberately not covered: a rejection on incident A does not suppress a
// different incident B that happens to resolve to the same patch. Cross-incident
// collapsing belongs to the proposer's target dedup, and treating a signal that
// cleared and came back as "already answered" would be wrong.
func (r *HealthCheckReconciler) rejectionSuppressesProposal(
	ctx context.Context,
	diag *diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
) (string, error) {
	if incident == nil || diag.PersonaRef == nil {
		return "", nil
	}

	namespace := incident.Namespace
	if namespace == "" {
		namespace = diag.PersonaRef.Namespace
	}

	opts := make([]client.ListOption, 0, 2)
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if diag.PersonaRef.Name != "" {
		opts = append(opts, client.MatchingLabels{LabelPersonaName: diag.PersonaRef.Name})
	}

	var list dorguv1.RemediationActionList
	if err := r.Client.List(ctx, &list, opts...); err != nil {
		return "", fmt.Errorf("listing RemediationActions for incident %s: %w", incident.Name, err)
	}

	for i := range list.Items {
		ra := &list.Items[i]
		if ra.Status.Phase != RemediationPhaseRejected {
			continue
		}
		if ra.Spec.IncidentRef.Name != incident.Name {
			continue
		}
		if !targetUnchanged(diag, incident) {
			continue
		}
		if severityEscalated(diag, incident) {
			continue
		}

		rejectedAt, stamped := rejectionTime(ra)
		if !stamped {
			// The CLI rejects by patching status.phase alone; the operator stamps
			// the timestamp on its next pass. Until it does there is nothing to
			// measure a cooldown against, and an un-timestamped no is still a no.
			return fmt.Sprintf("remediation %s was rejected and its cooldown has not been timestamped yet", ra.Name), nil
		}
		if elapsed := time.Since(rejectedAt); elapsed < RejectionCooldown {
			return fmt.Sprintf("remediation %s was rejected %s ago; not re-proposing for another %s",
				ra.Name, elapsed.Round(time.Second), (RejectionCooldown - elapsed).Round(time.Second)), nil
		}
	}

	return "", nil
}

// targetUnchanged reports whether the live diagnosis still points at the same
// fix the rejected remediation addressed. The incident's signal is the target
// identity dorgu proposes against, and it is frozen at creation, so a diagnosis
// that has moved to another signal is a different proposal.
func targetUnchanged(diag *diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) bool {
	return string(primarySignalType(diag)) == incident.Spec.Detection.Signal
}

// severityEscalated reports whether the live diagnosis is more severe than the
// incident the user declined. Incident severity is stamped once at creation and
// never rewritten, so it is a faithful snapshot of what the rejected proposal
// was about.
func severityEscalated(diag *diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) bool {
	return detection.SeverityRank(diag.Severity) >
		detection.SeverityRank(detection.Severity(incident.Spec.Severity))
}

// rejectionTime returns when the rejection was recorded, and whether a timestamp
// was found at all.
func rejectionTime(ra *dorguv1.RemediationAction) (time.Time, bool) {
	for i := range ra.Status.Conditions {
		c := &ra.Status.Conditions[i]
		if c.Type == ConditionRejected && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}
