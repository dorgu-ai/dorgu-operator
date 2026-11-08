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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
// the workload under the ticket. It is matched exactly: "Dorgu", "dorgu-gitops"
// and "dorgu-operator" are other managers, and the operator never writes
// Deployments at all.
const DorguFieldManager = "dorgu"

// ClockSkewMargin is how far before a ticket's issue time a managedFields stamp
// still counts as "after the ticket". The stamps are the apiserver's clock and the
// issue time is the operator's: with the operator ahead, a write that really
// followed the ticket carries an earlier stamp. The margin errs toward calling a
// workload diverged, never toward missing a write; five seconds is well beyond the
// skew of NTP-synchronised nodes, and a write that close before the ticket is also
// in the resourceVersion the ticket recorded, so the cost of a false positive is
// one heal not being credited.
const ClockSkewMargin = 5 * time.Second

// kubeControllerManager is the field manager of the Deployment controller. It
// stamps the rollout revision annotation after every template change (the CLI's
// own patch included) and writes status through the status subresource. It never
// changes what a human or a pipeline asked for, so a write by it is not someone
// else moving the workload. Matched exactly.
const kubeControllerManager = "kube-controller-manager"

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
	cand.prior, cand.priorGuard, cand.priorProblem = operatorRecord(action, PhaseProposal)
	return cand
}

// TicketCandidate builds the candidate the apply-time re-run uses: the same plan
// and the same live re-measurement as the approval re-check, but the record it
// compares the live cluster to is the approval re-check's own (status.gate with
// phase ApprovalRecheck), which is what the operator wrote when it issued the
// ticket. The Proposed condition still has to pin the current spec generation.
func TicketCandidate(action *dorguv1.RemediationAction) Candidate {
	cand := ApprovalCandidate(action)
	cand.prior, cand.priorGuard, cand.priorProblem = operatorRecord(action, PhaseApprovalRecheck)
	return cand
}

// operatorRecord returns what the operator recorded about the action in the given
// phase, or the guardrail and reason it cannot be relied on.
func operatorRecord(action *dorguv1.RemediationAction, phase Phase) (*dorguv1.GateObserved, string, string) {
	status := action.Status.Gate
	if status == nil || status.Phase != string(phase) ||
		(status.Result != string(ResultPass) && status.Result != string(ResultClamp)) ||
		status.Observed == nil || status.Observed.ObservedAt == nil {
		return nil, GuardrailNoProposalRecord,
			fmt.Sprintf("there is no record that the operator's gate cleared this plan at %s (status.gate), so there is nothing to compare the live cluster to; only a plan the operator proposed can be approved", phase)
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

	issued := d.EvaluatedAt
	ticket := &dorguv1.ApplyTicket{
		Patch:     patch,
		Verdicts:  append([]dorguv1.GateVerdict(nil), d.Verdicts...),
		IssuedAt:  &issued,
		ExpiresAt: metav1.NewTime(d.EvaluatedAt.Add(TicketTTL)),
	}
	if d.Observed != nil {
		ticket.WorkloadUID = d.Observed.WorkloadUID
		ticket.ObservedResourceVersion = d.Observed.WorkloadResourceVersion
	}
	if d.Observation != nil && d.Observation.Deployment != nil {
		ticket.ObservedGeneration = d.Observation.Deployment.Generation
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

// CheckTicket is the apply-time check. A ticket is not a standing permission: it
// is honoured only when the gate, run again on the cluster as it is now, would
// issue exactly this ticket.
//
// In order, it requires: a ticket; an approval on record (status.approvedAt); a
// well-formed patch inside the resource allowlist; an issue time, an expiry that
// has not passed, and a lifetime no longer than TicketTTL; the same workload (UID)
// and one nobody else has changed since; the approval history the operator wrote
// (the Proposed condition at the current spec generation, and status.gate as the
// ApprovalRecheck that cleared); and then ApprovalRecheck itself, whose derived
// patch and container must equal the ticket's. Observe mode, a protected
// namespace, the blast-radius cap, rate limits and the rest are therefore
// re-judged at apply (F1, F3).
//
// What it cannot do: it cannot tell a ticket the operator wrote from one written
// by a client holding remediationactions/status, who can also forge the history.
// That gap is closed by admission policy (P2 task 2.5), not here; the re-derived
// patch means such a writer still cannot make the operator apply anything the
// spec's own plan would not produce.
//
// A nil problem with a nil error means the ticket stands. An error means the live
// cluster could not be read, which the caller must treat as a refusal. It reads
// the cluster uncached.
func (g *Gate) CheckTicket(ctx context.Context, action *dorguv1.RemediationAction) (*TicketProblem, error) {
	if g == nil {
		return &TicketProblem{GuardrailTicket, "no gate is configured, so no ticket can be honoured"}, nil
	}
	ticket := action.Status.ApplyTicket
	if ticket == nil {
		return &TicketProblem{GuardrailTicket, "there is no apply ticket: nothing is applied without one"}, nil
	}
	if action.Status.ApprovedAt == nil || action.Status.ApprovedAt.IsZero() {
		return &TicketProblem{GuardrailNotApproved, "the action has no approval on record (status.approvedAt), so a ticket on it was not issued for an approval"}, nil
	}
	if problem := ticketPatchProblem(ticket); problem != "" {
		return &TicketProblem{GuardrailPatchShape, problem}, nil
	}
	if problem := g.ticketTimingProblem(ticket); problem != nil {
		return problem, nil
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
	return g.rederiveTicket(ctx, action)
}

// ticketTimingProblem checks the ticket's own clock fields: it has an issue time,
// has not expired, and was not given a lifetime beyond what the gate hands out.
func (g *Gate) ticketTimingProblem(ticket *dorguv1.ApplyTicket) *TicketProblem {
	if ticket.IssuedAt == nil || ticket.IssuedAt.IsZero() {
		return &TicketProblem{GuardrailTicket, "the apply ticket carries no issue time"}
	}
	if ticket.ExpiresAt.IsZero() || !g.now().Before(ticket.ExpiresAt.Time) {
		return &TicketProblem{GuardrailTicket,
			fmt.Sprintf("the apply ticket expired at %s", ticket.ExpiresAt.UTC().Format(time.RFC3339))}
	}
	if ticket.ExpiresAt.Sub(ticket.IssuedAt.Time) > TicketTTL {
		return &TicketProblem{GuardrailTicket, "the apply ticket's lifetime is longer than the gate ever issues"}
	}
	return nil
}

// rederiveTicket runs ApprovalRecheck again on live state and requires that it
// would issue the very ticket on the action.
func (g *Gate) rederiveTicket(ctx context.Context, action *dorguv1.RemediationAction) (*TicketProblem, error) {
	decision, err := g.Run(ctx, TicketCandidate(action), PhaseApprovalRecheck)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed() {
		_, guardrail := decision.RefusedBy()
		return &TicketProblem{guardrail, decision.SkipReason()}, nil
	}
	derived, err := decision.Ticket()
	if err != nil {
		return &TicketProblem{GuardrailNoPlan, "the gate, run again, leaves nothing to apply"}, nil
	}

	ticket := action.Status.ApplyTicket
	switch {
	case !patchesEqual(derived.Patch, ticket.Patch):
		return &TicketProblem{GuardrailTicketMismatch, "the ticket's patch is not the one the gate derives from the plan on the live cluster now"}, nil
	case derived.Container != ticket.Container:
		return &TicketProblem{GuardrailTicketMismatch, "the ticket's container is not the one the gate derives from the live workload"}, nil
	case derived.WorkloadUID != ticket.WorkloadUID:
		return &TicketProblem{GuardrailTicketMismatch, "the ticket's workload is not the one the gate derives"}, nil
	}
	return nil, nil
}

// patchesEqual reports whether two merge patches say the same thing, whatever
// their key order or spacing.
func patchesEqual(a, b *apiextensionsv1.JSON) bool {
	if a == nil || b == nil {
		return a == b
	}
	var av, bv any
	if json.Unmarshal(a.Raw, &av) != nil || json.Unmarshal(b.Raw, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
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
//
// A resourceVersion that moved is explained away only by evidence: managedFields
// must show that every non-subresource write since the ticket was Dorgu's CLI.
// Missing evidence is not an explanation. A Deployment with no managedFields at
// all, or an entry with no timestamp, counts as changed by someone else (F5).
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
		if manager := changedByOthersSince(dep, ticketIssuedAt(ticket)); manager != "" {
			return &TicketProblem{GuardrailLiveWorkload,
				fmt.Sprintf("the Deployment was changed by %q since the ticket was written", manager)}, nil
		}
	}
	return nil, nil
}

// WorkloadGeneration is metadata.generation of the live Deployment the ticket is
// bound to, read uncached. It errors when the workload cannot be read or is not the
// ticket's: a generation of some other Deployment says nothing about this heal.
func (g *Gate) WorkloadGeneration(ctx context.Context, action *dorguv1.RemediationAction) (int64, error) {
	if g == nil || action.Status.ApplyTicket == nil {
		return 0, errors.New("no gate or no ticket to read the workload for")
	}
	key := ticketKey(action)
	obs, err := workload.Observe(ctx, g.reader, key.Namespace, key.Name)
	if err != nil {
		return 0, fmt.Errorf("reading the live workload for %s/%s: %w", key.Namespace, key.Name, err)
	}
	if obs == nil || obs.Deployment == nil {
		return 0, errors.New("the Deployment the ticket was written against no longer exists")
	}
	if string(obs.Deployment.UID) != action.Status.ApplyTicket.WorkloadUID {
		return 0, errors.New("the Deployment is not the one the ticket was written against")
	}
	return obs.Deployment.Generation, nil
}

// ticketIssuedAt is when the ticket was written. A ticket from before the issue
// time was recorded is dated from its expiry.
func ticketIssuedAt(ticket *dorguv1.ApplyTicket) time.Time {
	if ticket.IssuedAt != nil && !ticket.IssuedAt.IsZero() {
		return ticket.IssuedAt.Time
	}
	return ticket.ExpiresAt.Add(-TicketTTL)
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

// unknownWriter is what changedByOthersSince reports when it can tell the
// Deployment moved but not who moved it.
const unknownWriter = "an unknown writer (no usable managedFields)"

// changedByOthersSince names a field manager other than Dorgu's CLI that wrote the
// Deployment (not a subresource such as status or scale) at or after since, or "".
//
// Only the exact manager name DorguFieldManager is Dorgu's. The stamps are the
// apiserver's own, compared with a margin of ClockSkewMargin before since because
// the issue time is the operator's clock. managedFields stamps whole seconds, so
// the comparison is made at that resolution. No entries at all, or an entry with
// no stamp, is reported as an unknown writer: the caller only asks once the
// resourceVersion has moved, and a move nothing explains is not clean.
func changedByOthersSince(dep *appsv1.Deployment, since time.Time) string {
	cut := since.Add(-ClockSkewMargin).Truncate(time.Second)
	if len(dep.ManagedFields) == 0 {
		return unknownWriter
	}
	for i := range dep.ManagedFields {
		mf := &dep.ManagedFields[i]
		if mf.Subresource != "" || mf.Manager == DorguFieldManager || mf.Manager == kubeControllerManager {
			continue
		}
		if mf.Time == nil {
			return unknownWriter
		}
		if !mf.Time.Time.Before(cut) {
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

// RolledBackFields are the persona resource fields Dorgu wrote: the paths of the
// ticket's patch, sorted. It reads the ticket only, never spec.action or
// spec.workloadRef, which whoever can edit the spec can change (F8).
func RolledBackFields(action *dorguv1.RemediationAction) []string {
	if action.Status.ApplyTicket == nil || action.Status.ApplyTicket.Patch == nil {
		return nil
	}
	var fields []string
	for path := range PatchLeafValues(action.Status.ApplyTicket.Patch.Raw) {
		if IsResourcePath(path) {
			fields = append(fields, path)
		}
	}
	slices.Sort(fields)
	return fields
}

// RollbackField is one persona field a rollback did not restore.
type RollbackField struct {
	// Path is the persona-spec path, e.g. spec.resources.limits.memory.
	Path string
	// Wrote is the value Dorgu's ticket wrote there.
	Wrote string
	// Current is the value the persona holds now; empty when the field is gone.
	Current string
	// Reason is why it was left alone: ReasonRollbackHumanEdited or
	// ReasonRollbackNoBaseline.
	Reason string
}

// Rollback reasons recorded for a field that was not restored.
const (
	// ReasonRollbackHumanEdited: the persona's value is no longer what Dorgu wrote,
	// so somebody edited it after the heal and a rollback must not overwrite it.
	ReasonRollbackHumanEdited = "HumanEdited"

	// ReasonRollbackNoBaseline: the gate recorded no prior value for the field, so
	// there is nothing to restore it to.
	ReasonRollbackNoBaseline = "NoBaseline"
)

// RollbackPlan is what a rollback may do to the persona now.
type RollbackPlan struct {
	// Patch restores the fields still holding what Dorgu wrote. Nil when none.
	Patch *apiextensionsv1.JSON
	// Restored are the paths Patch restores.
	Restored []string
	// Skipped are the fields left as they are, with why.
	Skipped []RollbackField
}

// PlanRollback decides which fields of the persona a rollback restores: only those
// whose current value still equals what the ticket wrote. A field a human changed
// since the heal is left alone and reported (F4): Dorgu undoes its own change, not
// somebody's later one. The baseline is the gate's (RollbackBaseline).
func PlanRollback(action *dorguv1.RemediationAction, persona *dorguv1.ApplicationPersona) (*RollbackPlan, *TicketProblem) {
	baseline, problem := RollbackBaseline(action)
	if problem != nil {
		return nil, problem
	}
	if persona == nil {
		return nil, &TicketProblem{GuardrailPersona, "the persona to roll back is not there"}
	}
	specJSON, err := json.Marshal(map[string]any{"spec": persona.Spec})
	if err != nil {
		return nil, &TicketProblem{GuardrailPatchShape, "the persona could not be read as JSON: " + err.Error()}
	}
	current := PatchLeafValues(specJSON)
	wrote := PatchLeafValues(action.Status.ApplyTicket.Patch.Raw)
	before := PatchLeafValues(baseline.Raw)

	plan := &RollbackPlan{}
	root := make(map[string]any)
	for _, path := range RolledBackFields(action) {
		prior, hasPrior := before[path]
		switch {
		case !sameQuantity(current[path], wrote[path]):
			plan.Skipped = append(plan.Skipped, RollbackField{Path: path, Wrote: wrote[path], Current: current[path], Reason: ReasonRollbackHumanEdited})
		case !hasPrior:
			plan.Skipped = append(plan.Skipped, RollbackField{Path: path, Wrote: wrote[path], Current: current[path], Reason: ReasonRollbackNoBaseline})
		default:
			mergeNestedPath(root, strings.Split(path, "."), prior)
			plan.Restored = append(plan.Restored, path)
		}
	}
	if len(plan.Restored) > 0 {
		raw, err := json.Marshal(root)
		if err != nil {
			return nil, &TicketProblem{GuardrailPatchShape, "the restoring patch could not be built: " + err.Error()}
		}
		plan.Patch = &apiextensionsv1.JSON{Raw: raw}
	}
	return plan, nil
}

// sameQuantity reports whether two resource values are equal as quantities
// ("512Mi" and "0.5Gi" are), falling back to string equality for anything that is
// not one. An absent value equals nothing.
func sameQuantity(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return qa.Cmp(qb) == 0
}
