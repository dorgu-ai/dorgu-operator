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

package gate

import (
	"context"
	"fmt"
	"slices"
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

const (
	// RejectionCooldown is how long a rejection holds before dorgu will propose
	// the same fix for the same incident again.
	//
	// Rejecting used to buy about 30 seconds: the health check re-proposed on the
	// next cycle, and with the AI planner configured every re-proposal is another
	// billed call, so declining cost the user money (F-07). An hour is long enough
	// that saying no is respected, short enough that a problem still present an
	// hour later gets raised again.
	RejectionCooldown = 1 * time.Hour

	// ConditionRejected records that a human declined a RemediationAction, and
	// when. The phase alone cannot carry the timestamp.
	ConditionRejected = "Rejected"

	phasePending  = "Pending"
	phaseRejected = "Rejected"
)

// activeRemediationPhases are the phases that mean a remediation is already in
// flight, so proposing another would duplicate it. Terminal phases (Completed,
// Failed, RolledBack, Rejected, Expired) do NOT block a fresh proposal, e.g. a
// recurrence after a prior fix failed. The empty phase covers a just-created
// action whose status write has not landed yet.
var activeRemediationPhases = []string{"", phasePending, phaseApproved, phaseTicketed, phaseApplying, phaseVerifying}

// dedupCooldownStage stops a proposal that is redundant (an active remediation
// already answers this incident or this fix) or that a human already declined.
// It runs at proposal only: at approval the plan IS the active remediation.
type dedupCooldownStage struct{ g *Gate }

func (dedupCooldownStage) Name() string { return StageDedupCooldown }

func (s dedupCooldownStage) Run(ctx context.Context, cand *Candidate, d *Decision) error {
	if d.Phase != PhaseProposal || cand.Facts.Incident == nil {
		return nil
	}

	siblings, err := s.g.siblings(ctx, cand)
	if err != nil {
		return err
	}

	if reason := activeRemediationReason(cand, siblings); reason != "" {
		d.refuse(StageDedupCooldown, GuardrailActiveAction, reason, nil)
		return nil
	}
	d.pass(StageDedupCooldown, GuardrailActiveAction, "no active remediation covers this incident or fix", nil)

	if reason := s.rejectionReason(cand, siblings); reason != "" {
		d.refuse(StageDedupCooldown, GuardrailRejection, reason, nil)
		return nil
	}
	d.pass(StageDedupCooldown, GuardrailRejection, "no recent rejection for this incident", nil)
	return nil
}

// activeRemediationReason reports whether an in-flight sibling makes this
// proposal redundant: it answers the same incident (the reconciler re-proposes
// every cycle), or it already changes the same target (a second incident for one
// root cause, such as a CrashLoopBackOff trailing an OOMKill, both fixed by
// raising the memory limit).
func activeRemediationReason(cand *Candidate, siblings []dorguv1.RemediationAction) string {
	incident := cand.Facts.Incident
	for i := range siblings {
		ra := &siblings[i]
		if !slices.Contains(activeRemediationPhases, ra.Status.Phase) {
			continue
		}
		if SameIncidentLineage(ra.Spec.IncidentRef.Name, incident.Name) {
			return fmt.Sprintf("active remediation already exists for incident %s", ra.Spec.IncidentRef.Name)
		}
		if cand.Facts.TargetPath != "" && remediationTargetsPath(ra, cand.Facts.TargetPath) {
			return fmt.Sprintf("active remediation already exists for persona %s targeting %s (via %s)",
				cand.Target.Name, cand.Facts.TargetPath, ra.Name)
		}
	}
	return ""
}

// remediationTargetsPath reports whether an existing RemediationAction changes
// the given persona-spec path, inspecting both the single Action patch (rule
// path) and every step patch (AI plan path).
func remediationTargetsPath(ra *dorguv1.RemediationAction, target string) bool {
	if ra.Spec.Action.Patch != nil && PatchTouchesPath(ra.Spec.Action.Patch.Raw, target) {
		return true
	}
	for i := range ra.Spec.Steps {
		if step := &ra.Spec.Steps[i]; step.Patch != nil && PatchTouchesPath(step.Patch.Raw, target) {
			return true
		}
	}
	return false
}

// rejectionReason reports whether a prior rejection should stop this proposal.
//
// The suppression is scoped to the incident and its target (CF4-2): a rejection
// is a no to one fix for one incident, not a blanket mute on the persona. "One
// incident" means one fault across its recurrences (see IncidentLineage): a
// fault that comes back after resolving is a new IncidentMemory, and the human's
// no to its fix still holds for the cooldown. It
// lifts when the cooldown expires or the signal materially changes, which here
// means the live diagnosis is more severe than the incident the user declined, or
// is a strictly worse shape of the fault than the one the rejected plan answered
// (an incident that escalated has its own severity raised, so the severity
// comparison alone would call the escalation "the same").
//
// Deliberately not covered: a rejection on incident A does not suppress a
// different incident B that happens to resolve to the same patch. Cross-incident
// collapsing belongs to the target dedup above.
func (s dedupCooldownStage) rejectionReason(cand *Candidate, siblings []dorguv1.RemediationAction) string {
	incident := cand.Facts.Incident
	for i := range siblings {
		ra := &siblings[i]
		if ra.Status.Phase != phaseRejected || !SameIncidentLineage(ra.Spec.IncidentRef.Name, incident.Name) {
			continue
		}
		if cand.Facts.PrimarySignal != incident.Spec.Detection.Signal {
			continue
		}
		if detection.SeverityRank(detection.Severity(cand.Facts.Severity)) >
			detection.SeverityRank(detection.Severity(incident.Spec.Severity)) {
			continue
		}

		if strictlyWorseShape(cand.Facts.PrimarySignal, ra.Annotations[AnnotationAnswersSignal]) {
			// The human said no to a milder shape of this fault. The fault is now a
			// worse one, which they have not been asked about.
			continue
		}

		rejectedAt, stamped := rejectionTime(ra)
		if !stamped {
			// The CLI rejects by patching status.phase alone; the operator stamps the
			// timestamp on its next pass. Until it does there is nothing to measure a
			// cooldown against, and an un-timestamped no is still a no.
			return fmt.Sprintf("remediation %s was rejected and its cooldown has not been timestamped yet", ra.Name)
		}
		if elapsed := s.g.now().Sub(rejectedAt); elapsed < RejectionCooldown {
			return fmt.Sprintf("remediation %s was rejected %s ago; not re-proposing for another %s",
				ra.Name, elapsed.Round(time.Second), (RejectionCooldown - elapsed).Round(time.Second))
		}
	}
	return ""
}

// strictlyWorseShape reports whether signal is further along the progression of
// one container's fault than the signal a rejected proposal answered. A rejection
// that does not say what it answered, or a signal outside the progression, is never
// "worse": it is compared as it always was.
func strictlyWorseShape(signal, answered string) bool {
	now, nowOK := detection.FaultRank(detection.SignalType(signal))
	was, wasOK := detection.FaultRank(detection.SignalType(answered))
	return nowOK && wasOK && now > was
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
