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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// C6b (review F-8, planner decision P2-D10): a value a person rolled back is refused.
//
// A completed rollback used to leave no trace the planner read, so the next incident
// proposed the same raise again. A person still had to approve it, but the proposal
// ignored the person's decision. The value a rollback undid is now treated as one
// Dorgu was told no on, through the same lens the CR5-01 refused-value scrub uses: it
// is removed from the plan, recorded on the step as a rejected value, and kept out of
// every surface a client renders.
//
// What is remembered, and from where. Only what the operator wrote:
//
//   - the rollback action's provenance (source rollback, written by the operator when
//     it converts a rollback proposal, immutable) AND the operator-only annotation
//     naming the action it undid (admission refuses every other writer of dorgu.io/
//     annotations). Either alone excuses nothing;
//   - the rollback's own phase (Completed) and its own apply ticket, which is what it
//     actually wrote;
//   - the original action's apply ticket (the values it wrote) and its recorded
//     baseline (status.gate.prePatchState), through the same eligibility check a
//     rollback request has to pass.
//
// A field is refused only when the rollback's ticket wrote that field back to the
// original's recorded baseline. A rollback that wrote anything else, or nothing,
// undid nothing and marks nothing. Nothing a proposer or a person can write counts:
// not a proposal, not spec.prePatchState, not a label.
//
// What is refused is the move, not the number. The decision a person made was about
// taking a field from the original's baseline to the value it wrote; a plan is
// withheld only when it would take the field from that same baseline (the live value
// now, the persona's when the workload did not resolve) to that same value. The same
// number reached from another starting point (a right-size down from 1Gi to 512Mi, a
// raise from 384Mi to 512Mi) is another decision and is held to the normal gate.
// Only an exact quantity matches (512Mi, 536870912 and 0.5Gi are one value; 513Mi is
// not): a near value is a different decision.
//
// How long it is remembered. For a fixed lifetime counted from the rollback's
// completion (the transition of its Verified condition, written by the operator):
// DefaultRolledBackValueTTL, 7 days, set by the operator flag
// --rolled-back-value-ttl and the chart value remediation.rolledBackValueTTL. The mark
// is in force for that long and lapses AT the boundary. It also ends when the rollback
// action is deleted, which only a cluster admin can do. Contrast the refused-value
// scrub, which remembers only the plan it is run on. A different value for the same
// field is not affected, and is held to the normal gate.

// GuardrailRolledBackValue is the guardrail that withholds a value a person rolled back.
const GuardrailRolledBackValue = "rolled-back-value"

// RolledBackValue is one value a person's completed rollback undid.
type RolledBackValue struct {
	// Field is the persona-spec path, e.g. spec.resources.limits.memory.
	Field string

	// Value is what the original action's ticket wrote there and the rollback undid.
	Value string

	// From is the value the original action moved the field away from (its recorded
	// baseline), which the rollback restored. The decision a person made was about the
	// move From to Value, so the mark applies to that move and to no other.
	From string

	// Until is when the mark lapses: the rollback's completion plus the lifetime.
	Until time.Time

	// Action is the original action, and Rollback the rollback of it. Both are names in
	// one namespace, for the sentence that tells a reader where the decision was made.
	Action   string
	Rollback string
}

// RolledBackValues derives, from a persona's RemediationActions, every value a person's
// completed rollback undid and whose mark has not lapsed at now. It reads only
// operator-written records (see the file comment) and is pure: the same actions and the
// same clock always give the same answer.
func RolledBackValues(actions []dorguv1.RemediationAction, now time.Time, ttl time.Duration) []RolledBackValue {
	index := make(map[string]*dorguv1.RemediationAction, len(actions))
	for i := range actions {
		index[actions[i].Namespace+"/"+actions[i].Name] = &actions[i]
	}

	var out []RolledBackValue
	seen := make(map[string]bool)
	for i := range actions {
		rollback := &actions[i]
		originalName := rollbackOfFor(rollback)
		if originalName == "" || rollback.Status.Phase != phaseCompleted ||
			rollback.Status.ApplyTicket == nil || rollback.Status.ApplyTicket.Patch == nil {
			continue
		}
		until, lapsed := rolledBackUntil(rollback, now, ttl)
		if lapsed {
			continue
		}
		original := index[rollback.Namespace+"/"+originalName]
		if original == nil || !samePersona(original, rollback) || rollbackEligibility(original) != nil {
			continue
		}
		baseline, problem := RollbackBaseline(original)
		if problem != nil {
			continue
		}
		before := PatchLeafValues(baseline.Raw)
		wrote := PatchLeafValues(original.Status.ApplyTicket.Patch.Raw)
		restored := PatchLeafValues(rollback.Status.ApplyTicket.Patch.Raw)

		for _, field := range RolledBackFields(original) {
			if !sameQuantity(restored[field], before[field]) || wrote[field] == "" {
				continue
			}
			key := field + "=" + canonicalQuantity(before[field]) + ">" + canonicalQuantity(wrote[field])
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, RolledBackValue{
				Field: field, Value: wrote[field], From: before[field],
				Action: original.Name, Rollback: rollback.Name, Until: until,
			})
		}
	}
	slices.SortFunc(out, func(a, b RolledBackValue) int {
		return strings.Compare(a.Field+"="+a.From+">"+a.Value+"/"+a.Rollback, b.Field+"="+b.From+">"+b.Value+"/"+b.Rollback)
	})
	return out
}

// rolledBackUntil is when the mark a completed rollback leaves lapses (its completion
// plus ttl), and whether it already has at now. The mark lapses AT the boundary: it is
// in force for ttl and not a nanosecond longer. A rollback whose completion time
// cannot be read at all (an object with no creation time, which only a test builds)
// keeps its mark: forgetting a person's decision is the unsafe way to be wrong.
func rolledBackUntil(rollback *dorguv1.RemediationAction, now time.Time, ttl time.Duration) (until time.Time, lapsed bool) {
	completed := rollbackCompletedAt(rollback)
	if completed.IsZero() {
		return time.Time{}, false
	}
	until = completed.Add(ttl)
	return until, !now.Before(until)
}

// RolledBackValues lists the values a person rolled back for the candidate's persona,
// for a caller that must not size one of them again. A candidate that is itself a
// ticketed rollback is never withheld: what a rollback restores is the operator's own
// record, and it may be a value an earlier rollback refused.
func (g *Gate) RolledBackValues(ctx context.Context, cand *Candidate) ([]RolledBackValue, error) {
	if cand.RollbackOf != "" {
		return nil, nil
	}
	siblings, err := g.siblings(ctx, cand)
	if err != nil {
		return nil, err
	}
	return RolledBackValues(siblings, g.now(), g.RolledBackValueTTL()), nil
}

// MatchesRolledBack reports whether moving field from `from` (the value the change is
// measured against, live or persona) to value is a move a person rolled back. The
// decision was about the move, not the number: the same value reached from another
// starting point is a different decision. Quantities are compared by what they mean,
// so 536870912 is 512Mi, and only an exact quantity matches (a near value is another
// decision).
func MatchesRolledBack(rolled []RolledBackValue, field, from, value string) (RolledBackValue, bool) {
	if from == "" {
		return RolledBackValue{}, false // no starting point known: not the same move
	}
	for _, r := range rolled {
		if r.Field == field && sameQuantity(r.From, from) && sameQuantity(r.Value, value) {
			return r, true
		}
	}
	return RolledBackValue{}, false
}

// RolledBackMessage is Dorgu's sentence for a value withheld because a person rolled it
// back. Every part of it is from the operator's own records.
func RolledBackMessage(v RolledBackValue) string {
	until := ""
	if !v.Until.IsZero() {
		until = " until " + v.Until.UTC().Format("2006-01-02 15:04 UTC")
	}
	return fmt.Sprintf(
		"Dorgu left out %s %s: it was rolled back by a person (rollback %s of action %s), so Dorgu will not propose it again%s. "+
			"A different value is still allowed, and a person can still set this one by hand.",
		v.Field, v.Value, v.Rollback, v.Action, until)
}

// RolledBackRationale replaces the plan's reasoning on a step whose value was withheld.
func RolledBackRationale() string {
	return "Dorgu does not propose a value a person has rolled back. The outcome above is Dorgu's own record of that decision, not the plan's account of it."
}

func samePersona(a, b *dorguv1.RemediationAction) bool {
	ns := func(ra *dorguv1.RemediationAction) string {
		if ra.Spec.PersonaRef.Namespace != "" {
			return ra.Spec.PersonaRef.Namespace
		}
		return ra.Namespace
	}
	return a.Spec.PersonaRef.Kind == b.Spec.PersonaRef.Kind &&
		a.Spec.PersonaRef.Name == b.Spec.PersonaRef.Name && ns(a) == ns(b)
}

// canonicalQuantity is a quantity's canonical form, so two spellings of one value
// dedupe; a value that is not a quantity is kept as written.
func canonicalQuantity(value string) string {
	qty, err := resource.ParseQuantity(value)
	if err != nil {
		return value
	}
	return qty.String()
}

// withholdRolledBack removes every value a person rolled back from the plan, and says
// so. It runs with the persona-wide checks, on the siblings they already read, so it is
// part of every run of the gate: the proposer's preflight and seal, the intake's, the
// approval re-check and the apply-time re-run. A plan proposed before a rollback
// completed therefore cannot be ticketed after it.
//
// A plan with nothing left to apply and no advice to give is refused: there is no
// proposal worth recording, and the refusal sentence reaches the incident. A plan that
// keeps advice is clamped, and its persona step explains the absence.
func (s guardrailsStage) withholdRolledBack(cand *Candidate, d *Decision, siblings []dorguv1.RemediationAction) {
	if cand.RollbackOf != "" {
		return
	}
	rolled := RolledBackValues(siblings, s.g.now(), s.g.RolledBackValueTTL())
	if len(rolled) == 0 {
		return
	}

	var withheld []RolledBackValue
	for i := range d.Steps {
		withheld = append(withheld, withholdFromStep(&d.Steps[i], rolled, d)...)
	}
	if actionAppliable(d) {
		hits := rolledBackLeaves(d.Action.Patch.Raw, rolled, baselineValues(d.Action.Patch.Raw, d))
		if len(hits) > 0 {
			paths := hitPaths(hits)
			d.Action.PrePatchState = PrunePatchPaths(d.Action.PrePatchState, paths)
			narrowAction(d, PrunePatchPaths(d.Action.Patch, paths))
			for _, h := range hits {
				withheld = append(withheld, h.rolled)
			}
		}
	}
	if len(withheld) == 0 {
		return
	}

	// A value on a step and on the derived action is one decision, said once.
	messages := make([]string, 0, len(withheld))
	for _, w := range withheld {
		if msg := RolledBackMessage(w); !slices.Contains(messages, msg) {
			messages = append(messages, msg)
		}
	}
	fields := make([]string, 0, len(withheld))
	for _, w := range withheld {
		if !slices.Contains(fields, w.Field) {
			fields = append(fields, w.Field)
		}
	}
	slices.Sort(fields)
	numbers := map[string]string{
		"field": withheld[0].Field, "value": withheld[0].Value,
		"action": withheld[0].Action, "rollback": withheld[0].Rollback,
		"fields": strings.Join(fields, ","),
	}
	reason := strings.Join(messages, " ")
	if hasAutoApplicable(d) {
		d.clamp(StageGuardrails, GuardrailRolledBackValue, reason, numbers).From(dorguv1.CeilingSourceDefault)
		return
	}
	// Nothing is left to apply. A proposal may keep advice in place of the withheld change
	// (a considered outcome); an approval or an apply-time re-run has nothing to approve
	// or apply, and says so for the reason that is true: a person's earlier decision, not
	// the live workload.
	if d.Phase == PhaseProposal && hasAdvice(d) {
		d.clamp(StageGuardrails, GuardrailRolledBackValue, reason, numbers).From(dorguv1.CeilingSourceDefault)
		return
	}
	d.refuse(StageGuardrails, GuardrailRolledBackValue, reason, numbers).From(dorguv1.CeilingSourceDefault)
}

// rolledBackHit is one patch leaf that carries a rolled-back value.
type rolledBackHit struct {
	path   string
	actual string
	rolled RolledBackValue
}

func hitPaths(hits []rolledBackHit) []string {
	paths := make([]string, 0, len(hits))
	for _, h := range hits {
		paths = append(paths, h.path)
	}
	return paths
}

// rolledBackLeaves finds the leaves of a merge patch that carry a rolled-back value.
func rolledBackLeaves(raw []byte, rolled []RolledBackValue, baselines map[string]string) []rolledBackHit {
	values := PatchLeafValues(raw)
	var hits []rolledBackHit
	for _, path := range SortedStringKeys(values) {
		if r, ok := MatchesRolledBack(rolled, path, baselines[path], values[path]); ok {
			hits = append(hits, rolledBackHit{path: path, actual: values[path], rolled: r})
		}
	}
	return hits
}

// withholdFromStep removes the rolled-back values from one persona-update step, records
// each as a rejected value, and takes over the words beside it.
func withholdFromStep(step *dorguv1.RemediationStep, rolled []RolledBackValue, d *Decision) []RolledBackValue {
	if step.Type != dorguv1.StepTypePersonaUpdate || step.Patch == nil {
		return nil
	}
	hits := rolledBackLeaves(step.Patch.Raw, rolled, baselineValues(step.Patch.Raw, d))
	if len(hits) == 0 {
		return nil
	}
	withheld := make([]RolledBackValue, 0, len(hits))
	for _, h := range hits {
		step.Safety = append(step.Safety, dorguv1.StepSafety{
			Rule:      dorguv1.SafetyRulePlanValidation,
			Verdict:   dorguv1.SafetyVerdictRejected,
			Field:     h.path,
			Requested: truncate(h.actual, 253),
			Message:   RolledBackMessage(h.rolled),
		})
		withheld = append(withheld, h.rolled)
	}
	paths := hitPaths(hits)
	step.Patch = PrunePatchPaths(step.Patch, paths)
	step.PrePatchState = PrunePatchPaths(step.PrePatchState, paths)
	if step.Patch == nil {
		step.AutoExecutable = false
	}
	step.Description = dorguv1.BoundText(DorguAuthoredDescription(describeStepPatch(step.Patch), step.Safety), dorguv1.MaxStepDescriptionLength)
	step.Rationale = RolledBackRationale()
	return withheld
}

// hasAdvice reports whether the plan still carries a step a person can act on that is
// not the operator's own persona write.
func hasAdvice(d *Decision) bool {
	for i := range d.Steps {
		if d.Steps[i].Type != dorguv1.StepTypePersonaUpdate {
			return true
		}
	}
	return false
}

// RolledBackRestOfPlanNote closes the refusal of a plan whose diagnosed change was
// withheld because a person rolled it back while another change still applied: the
// other change does not answer the incident, so it is not proposed in its place.
const RolledBackRestOfPlanNote = " The rest of this plan does not change what the incident is about, so none of it is proposed."

// RolledBackReason is Dorgu's sentence for the value the guardrail withheld for field in
// d, or "" when it withheld none. For a caller (the proposer) that must say the same
// thing as the gate does.
func RolledBackReason(d *Decision, field string) string { return rolledBackReason(d, field) }

// rolledBackReason is the sentence the guardrail recorded for field.
func rolledBackReason(d *Decision, field string) string {
	if d == nil {
		return ""
	}
	for _, v := range d.Verdicts {
		if v.Guardrail == GuardrailRolledBackValue && v.Verdict != dorguv1.GateVerdictPass &&
			slices.Contains(strings.Split(v.Numbers["fields"], ","), field) {
			return v.Reason
		}
	}
	return ""
}

// withheldRolledBack reports whether the guardrail withheld a value for field.
func withheldRolledBack(d *Decision, field string) bool {
	for _, v := range d.Verdicts {
		if v.Guardrail == GuardrailRolledBackValue && v.Verdict != dorguv1.GateVerdictPass &&
			slices.Contains(strings.Split(v.Numbers["fields"], ","), field) {
			return true
		}
	}
	return false
}

// rollbackCompletedAt is when a completed rollback finished: the transition of its
// Verified condition (which only the operator writes, and which is what moves the
// action to Completed), else the time it was applied, else its creation.
func rollbackCompletedAt(ra *dorguv1.RemediationAction) time.Time {
	for _, c := range ra.Status.Conditions {
		if c.Type == "Verified" && c.Status == "True" && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	if ra.Status.AppliedAt != nil && !ra.Status.AppliedAt.IsZero() {
		return ra.Status.AppliedAt.Time
	}
	return ra.CreationTimestamp.Time
}
