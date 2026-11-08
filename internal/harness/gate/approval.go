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
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/workload"
)

// TicketTTL is how long an apply ticket stays valid. It is operator-owned: a
// human approves in seconds and the operator applies in the same pass, so ten
// minutes is a long margin; a ticket that outlives it describes a cluster nobody
// measured.
const TicketTTL = 10 * time.Minute

// DorguFieldManager is the field manager the dorgu CLI patches a Deployment
// under. A write by it is the approved change itself, not somebody else moving
// the workload under the ticket.
const DorguFieldManager = "dorgu"

// ApprovalCandidate builds the candidate the approval re-check runs on from the
// persisted RemediationAction.
//
// The plan is spec.steps[] (a legacy action-only RemediationAction is read as a
// one-step plan); spec.action is not consulted. Everything else the gate relies
// on at approval comes from operator-written status: what the operator observed
// when it proposed (status.gate from a Proposal run that cleared) and that the
// spec is still the generation it cleared (the Proposed condition). Nothing the
// author of the spec controls can supply, widen or skip those (M2).
func ApprovalCandidate(action *dorguv1.RemediationAction) Candidate {
	cand := Candidate{
		Target:      action.Spec.PersonaRef,
		Namespace:   action.Namespace,
		Name:        action.Name,
		Source:      action.Spec.PlanSource,
		IncidentRef: action.Spec.IncidentRef,
		Steps:       action.EffectiveSteps(),
		ApprovedAt:  action.Status.ApprovedAt.DeepCopy(),
		Final:       true,
	}
	cand.prior, cand.priorGuard, cand.priorProblem = proposalRecord(action)
	return cand
}

// proposalRecord returns what the operator recorded when it proposed the action,
// or the guardrail and reason it cannot be relied on.
func proposalRecord(action *dorguv1.RemediationAction) (*dorguv1.GateObserved, string, string) {
	status := action.Status.Gate
	if status == nil || status.Phase != string(PhaseProposal) ||
		(status.Result != string(ResultPass) && status.Result != string(ResultClamp)) ||
		status.Observed == nil || status.Observed.ObservedAt == nil {
		return nil, GuardrailNoProposalRecord,
			"there is no record that the operator's gate cleared this plan when it was proposed (status.gate), so there is nothing to compare the live cluster to; only a plan the operator proposed can be approved"
	}

	var cleared *metav1.Condition
	for i := range action.Status.Conditions {
		c := &action.Status.Conditions[i]
		if c.Type == dorguv1.ConditionProposed && c.Status == metav1.ConditionTrue {
			cleared = c
		}
	}
	if cleared == nil {
		return nil, GuardrailNoProposalRecord,
			"the operator did not record clearing this plan at proposal, so it cannot be approved"
	}
	if cleared.ObservedGeneration != action.Generation {
		return nil, GuardrailSpecChanged,
			fmt.Sprintf("the spec was edited after the operator cleared it (cleared generation %d, now %d), so what was approved is not what the gate ruled on",
				cleared.ObservedGeneration, action.Generation)
	}
	return status.Observed, "", ""
}

// approvalFacts strips from an approval candidate everything a caller could use
// to steer the re-check: the dedup facts and the completeness waivers belong to
// proposal. The re-check is Final whatever the caller said (M1).
func approvalFacts(cand Candidate) Candidate {
	cand.Final = true
	cand.Facts = Facts{}
	return cand
}

// Ticket turns an allowed approval decision into the apply ticket: the persona
// patches of the plan's auto-executable steps, as the gate clamped them, merged
// in step order into the one patch that may land, bound to the live workload the
// decision measured.
func (d *Decision) Ticket() (*dorguv1.ApplyTicket, error) {
	if !d.Allowed() {
		return nil, errors.New("a refused decision carries no ticket")
	}
	patch := mergedPersonaPatch(d.Steps)
	if patch == nil {
		return nil, errors.New("the plan has no persona-update left to apply")
	}

	ticket := &dorguv1.ApplyTicket{
		Patch:     patch,
		Verdicts:  append([]dorguv1.GateVerdict(nil), d.Verdicts...),
		ExpiresAt: metav1.NewTime(d.EvaluatedAt.Add(TicketTTL)),
	}
	if d.Observed != nil {
		ticket.WorkloadUID = d.Observed.WorkloadUID
		ticket.ObservedResourceVersion = d.Observed.WorkloadResourceVersion
	}
	if d.WorkloadRef != nil {
		ticket.Container = d.WorkloadRef.Container
	}
	return ticket, nil
}

// mergedPersonaPatch merges, in step order, the patches of every auto-executable
// persona-update step. Nil when there are none.
func mergedPersonaPatch(steps []dorguv1.RemediationStep) *apiextensionsv1.JSON {
	ordered := slices.Clone(steps)
	slices.SortStableFunc(ordered, func(a, b dorguv1.RemediationStep) int { return int(a.Order) - int(b.Order) })

	var merged *apiextensionsv1.JSON
	for i := range ordered {
		step := &ordered[i]
		if !step.AutoExecutable || step.Type != dorguv1.StepTypePersonaUpdate || step.Patch == nil || len(step.Patch.Raw) == 0 {
			continue
		}
		merged = MergePatches(merged, step.Patch)
	}
	return merged
}

// DeriveAction is the back-compat spec.action for a plan: the merged persona
// patch of its auto-executable steps and the merged baseline they were measured
// against, or a notification when the plan applies nothing. spec.action is
// derived from steps[] and is never what the executor applies.
func DeriveAction(steps []dorguv1.RemediationStep) dorguv1.RemediationActionDetail {
	patch := mergedPersonaPatch(steps)
	if patch == nil {
		return dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
	}

	ordered := slices.Clone(steps)
	slices.SortStableFunc(ordered, func(a, b dorguv1.RemediationStep) int { return int(a.Order) - int(b.Order) })
	var pre *apiextensionsv1.JSON
	for i := range ordered {
		step := &ordered[i]
		if step.AutoExecutable && step.Type == dorguv1.StepTypePersonaUpdate && step.Patch != nil && len(step.Patch.Raw) > 0 {
			pre = MergePatches(pre, step.PrePatchState)
		}
	}
	return dorguv1.RemediationActionDetail{
		Type:          dorguv1.ActionTypePersonaUpdate,
		Patch:         patch,
		PrePatchState: pre,
	}
}

// TicketProblem is why a ticket cannot be applied. Guardrail is the machine
// reason; Reason is a sentence for a human and never contains model text.
type TicketProblem struct {
	Guardrail string
	Reason    string
}

func (p *TicketProblem) Error() string { return p.Reason }

// CheckTicket is the apply-time check: the action may be applied only with a
// ticket that is present, well-formed, unexpired, and whose workload is still the
// one it was measured against (same UID, and not changed by anyone else since).
//
// A nil problem with a nil error means the ticket stands. An error means the
// live workload could not be read, which the caller must treat as a refusal.
// It reads the cluster uncached: the ticket's resourceVersion is only worth
// comparing to a read that is not itself stale.
func (g *Gate) CheckTicket(ctx context.Context, action *dorguv1.RemediationAction) (*TicketProblem, error) {
	if g == nil {
		return &TicketProblem{GuardrailTicket, "no gate is configured, so no ticket can be honoured"}, nil
	}
	ticket := action.Status.ApplyTicket
	if ticket == nil {
		return &TicketProblem{GuardrailTicket, "there is no apply ticket: nothing is applied without one"}, nil
	}
	if problem := ticketPatchProblem(ticket); problem != "" {
		return &TicketProblem{GuardrailPatchShape, problem}, nil
	}
	if ticket.ExpiresAt.IsZero() || !g.now().Before(ticket.ExpiresAt.Time) {
		return &TicketProblem{GuardrailTicket,
			fmt.Sprintf("the apply ticket expired at %s", ticket.ExpiresAt.UTC().Format(time.RFC3339))}, nil
	}
	if ticket.WorkloadUID == "" {
		return &TicketProblem{GuardrailNoProposalRecord, "the apply ticket is not bound to a workload"}, nil
	}

	problem, err := g.workloadMoved(ctx, action)
	if err != nil {
		return nil, err
	}
	if problem != nil {
		return problem, nil
	}
	return nil, nil
}

// WorkloadDivergence reports whether the live workload still is what the ticket
// was written against: a Deployment replaced (UID changed), gone, or changed by
// someone other than Dorgu since the ticket. A nil result means it did not
// diverge. An error means the workload could not be read.
func (g *Gate) WorkloadDivergence(ctx context.Context, action *dorguv1.RemediationAction) (*TicketProblem, error) {
	if g == nil || action.Status.ApplyTicket == nil {
		return nil, errors.New("no gate or no ticket to compare the workload to")
	}
	return g.workloadMoved(ctx, action)
}

// workloadMoved compares the live Deployment to the ticket.
func (g *Gate) workloadMoved(ctx context.Context, action *dorguv1.RemediationAction) (*TicketProblem, error) {
	ticket := action.Status.ApplyTicket
	key := ticketKey(action)
	if key.Name == "" || key.Namespace == "" {
		return &TicketProblem{GuardrailWorkloadGone, "the action names no persona, so its workload cannot be found"}, nil
	}

	obs, err := workload.Observe(ctx, g.reader, key.Namespace, key.Name)
	if err != nil {
		var amb *workload.AmbiguousError
		if errors.As(err, &amb) {
			return &TicketProblem{GuardrailWorkloadAmbiguous, "more than one Deployment now answers to this persona"}, nil
		}
		return nil, fmt.Errorf("reading the live workload for %s/%s: %w", key.Namespace, key.Name, err)
	}
	if obs == nil || obs.Deployment == nil {
		return &TicketProblem{GuardrailWorkloadGone, "the Deployment the ticket was written against no longer exists"}, nil
	}

	dep := obs.Deployment
	if string(dep.UID) != ticket.WorkloadUID {
		return &TicketProblem{GuardrailLiveWorkload, "the Deployment was replaced since the ticket was written (its UID changed)"}, nil
	}
	if dep.ResourceVersion != ticket.ObservedResourceVersion {
		issued := ticket.ExpiresAt.Add(-TicketTTL)
		if manager := changedByOthersSince(dep, issued); manager != "" {
			return &TicketProblem{GuardrailLiveWorkload,
				fmt.Sprintf("the Deployment was changed by %q since the ticket was written", manager)}, nil
		}
	}
	return nil, nil
}

// ticketKey is the persona (namespace, name) whose workload the ticket is about.
func ticketKey(action *dorguv1.RemediationAction) workloadKey {
	ns := action.Spec.PersonaRef.Namespace
	if ns == "" {
		ns = action.Namespace
	}
	return workloadKey{Namespace: ns, Name: action.Spec.PersonaRef.Name}
}

type workloadKey struct{ Namespace, Name string }

// changedByOthersSince names a field manager other than Dorgu's that wrote the
// Deployment (not a subresource such as status or scale) at or after since, or
// "". managedFields stamps whole seconds, so the comparison is made at that
// resolution.
func changedByOthersSince(dep *appsv1.Deployment, since time.Time) string {
	since = since.Truncate(time.Second)
	for i := range dep.ManagedFields {
		mf := &dep.ManagedFields[i]
		if mf.Subresource != "" || mf.Time == nil || mf.Manager == DorguFieldManager {
			continue
		}
		if !mf.Time.Time.Before(since) {
			return mf.Manager
		}
	}
	return ""
}

// ticketPatchProblem checks the ticket's patch is still a shape and content the
// executor may apply, whoever wrote it: an object, no dotted keys, and only
// positive CPU or memory quantities under resources. The gate wrote it, so this
// is defence in depth against a status writer that is not the gate.
func ticketPatchProblem(ticket *dorguv1.ApplyTicket) string {
	if ticket.Patch == nil || len(ticket.Patch.Raw) == 0 {
		return "the apply ticket carries no patch"
	}
	if reason := patchShapeProblem(ticket.Patch.Raw); reason != "" {
		return "the ticket's patch " + reason
	}
	values := PatchLeafValues(ticket.Patch.Raw)
	for _, path := range PatchLeafPaths(ticket.Patch.Raw) {
		if reason := disallowedLeaf(path, values); reason != "" {
			return "the ticket's patch is outside the resource allowlist at " + path
		}
	}
	return ""
}

// RollbackBaseline is the persona baseline a rollback restores: the live values
// the gate measured at approval (status.gate.prePatchState), never the spec's
// own prePatchState, which the author of a RemediationAction chooses.
//
// Rollback undoes a change that was applied, and a change is applied only under a
// ticket, so an action with no ticket has nothing to roll back. The ticket's
// expiry and resourceVersion are deliberately not checked: a heal that degraded
// ten minutes later must still roll back, and the Deployment has been patched by
// then.
func RollbackBaseline(action *dorguv1.RemediationAction) (*apiextensionsv1.JSON, *TicketProblem) {
	if action.Status.ApplyTicket == nil {
		return nil, &TicketProblem{GuardrailTicket, "no apply ticket was issued, so nothing was applied under one and there is nothing to roll back"}
	}
	if action.Status.Gate == nil || action.Status.Gate.PrePatchState == nil || len(action.Status.Gate.PrePatchState.Raw) == 0 {
		return nil, &TicketProblem{GuardrailNoBaseline, "the gate recorded no baseline to restore"}
	}
	baseline := action.Status.Gate.PrePatchState
	if reason := patchShapeProblem(baseline.Raw); reason != "" {
		return nil, &TicketProblem{GuardrailPatchShape, "the recorded baseline " + reason}
	}
	values := PatchLeafValues(baseline.Raw)
	for _, path := range PatchLeafPaths(baseline.Raw) {
		if reason := disallowedLeaf(path, values); reason != "" {
			return nil, &TicketProblem{GuardrailPatchShape, "the recorded baseline is outside the resource allowlist at " + path}
		}
	}
	return baseline, nil
}

// ReasonEnum renders a guardrail as the CamelCase reason of a condition
// (blast-radius becomes BlastRadius): condition reasons may not contain dashes.
func ReasonEnum(guardrail string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(guardrail, func(r rune) bool { return r == '-' || r == '_' || r == ' ' }) {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	if b.Len() == 0 {
		return "Refused"
	}
	return b.String()
}
