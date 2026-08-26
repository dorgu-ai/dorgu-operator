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

package remediation

import (
	"encoding/json"
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

// This file holds the two rules that decide whether an AI plan is allowed to
// exist, and the Dorgu-authored text that goes with them.
//
// CR-01, clean-room run #4: with the AI planner enabled exactly as every install
// page instructs, 0 of 9 planned remediations could be applied. The planner
// described the memory fix as `workload-apply` steps, which the CRD forbids from
// ever being auto-executable, and gave the one `persona-update` step no patch.
// The action was therefore persisted as `notification`, `dorgu remediation
// approve` printed "No resource change to apply", and the pod was still in
// CrashLoopBackOff 42 minutes later. Turning the planner off healed the same app
// on the first try, which is what makes this the planner's bug and not the
// loop's.
//
// The response here is deliberately not "prompt the model harder". A plan is
// checked against what the operator can actually do, and a plan that cannot heal
// a resource problem it just diagnosed either gets the rule engine's own patch
// or does not get written at all.
//
// Whether a resource change was diagnosed at all, and which persona-spec field
// it lands on, comes from diagnosisTargetPath: the same signal-to-dimension
// mapping the rule engine uses, so the question has exactly one answer across
// both planning paths. An empty path means no resource change was diagnosed.

// assertAppliableWhenResourceDiagnosed is the invariant CR-01 is about: no
// RemediationAction is created whose steps are all non-executable when a
// resource change was diagnosed.
//
// It is checked immediately before the Create, against the object being written,
// so it holds regardless of which path built the plan or what a future change
// does to the steps in between. A violation is an error rather than a silent
// downgrade: the caller falls back to the rule-based proposal.
//
// ruleEngineDeclined is the one exemption, and it is the difference between "the
// plan failed to produce a fix" and "there is no fix of this shape to produce".
// When Dorgu's own rule engine refuses to size a change (F-05: the live
// container sets no CPU limit, so raising one would introduce a field the
// workload has never had) the rule-based path would produce nothing either, so
// an advisory plan that says exactly that is the honest outcome and is worth
// more to the reader than no plan at all.
func assertAppliableWhenResourceDiagnosed(
	action *dorguv1.RemediationAction,
	diag diagnosis.Diagnosis,
	ruleEngineDeclined bool,
) error {
	target := diagnosisTargetPath(diag)
	if ruleEngineDeclined || target == "" || appliesDiagnosedChange(action, target) {
		return nil
	}
	return fmt.Errorf(
		"refusing to persist a plan that applies nothing for a diagnosed change to %s: no step carries an appliable persona-update patch for it",
		target)
}

// appliesDiagnosedChange reports whether some auto-executable persona-update
// step patches the field the diagnosis calls for.
//
// It asks about the field, not merely about appliability. A plan that raises the
// memory REQUEST while the diagnosis calls for the memory LIMIT is appliable and
// still leaves the OOM in place, and HasAutoApplicableChange cannot tell the two
// apart.
func appliesDiagnosedChange(action *dorguv1.RemediationAction, target string) bool {
	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		if step.AutoExecutable && step.Type == dorguv1.StepTypePersonaUpdate &&
			step.Patch != nil && patchTouchesPath(step.Patch.Raw, target) {
			return true
		}
	}

	// Mirrors HasAutoApplicableChange: when Steps is populated it is the plan of
	// record, and the legacy single Action is derived from it. Otherwise the
	// Action is the plan, which is the shape the rule-based path produces.
	if len(action.Spec.Steps) > 0 {
		return false
	}
	return action.Spec.Action.Type == dorguv1.ActionTypePersonaUpdate &&
		action.Spec.Action.Patch != nil &&
		patchTouchesPath(action.Spec.Action.Patch.Raw, target)
}

// ensureAppliableResourcePlan gives an AI plan for a diagnosed resource change
// the patch it is missing, or refuses the plan so the caller falls back to
// rules.
//
// The model is treated as the author of the explanation, never as the author of
// the number that reaches the cluster. When the plan carries no patch Dorgu can
// apply, the value comes from calculateResourceChange: the same deterministic
// calculation the rule-based path uses, and the one that healed the identical
// app in the tester's A/B while the AI path was leaving it in CrashLoopBackOff.
//
// Dorgu fills in a step the plan already has; it does not invent one. A plan
// with no persona-update step in it did not propose a persona change at all, and
// splicing Dorgu's step into an AI-labelled plan would produce a plan no model
// wrote. That case falls back to the rule-based proposal instead, which records
// planSource: rule-based and is therefore auditable as what it is.
//
// It returns true when Dorgu's own rule engine declines to size a change, which
// makes an advisory plan a considered outcome rather than the CR-01 defect.
func (p *Proposer) ensureAppliableResourcePlan(
	action *dorguv1.RemediationAction,
	diag diagnosis.Diagnosis,
	persona *dorguv1.ApplicationPersona,
	ref *dorguv1.WorkloadRef,
) (ruleEngineDeclined bool, err error) {
	target := diagnosisTargetPath(diag)
	if target == "" || appliesDiagnosedChange(action, target) {
		return false, nil
	}

	change, err := p.calculateResourceChange(diag, persona, ref)
	if err != nil {
		return false, fmt.Errorf("sizing a resource change for an unappliable AI plan: %w", err)
	}
	if change.skipReason != "" || change.patch == nil {
		p.logger.V(1).Info("AI plan applies nothing and the rule engine will not size a change either; the plan stays advisory",
			"action", action.Name, "field", target, "reason", change.skipReason)
		return true, nil
	}

	step := personaUpdateStep(action)
	if step == nil {
		return false, fmt.Errorf(
			"AI plan for a diagnosed change to %s contains no persona-update step, the only kind the operator can apply",
			target)
	}

	patchJSON, err := json.Marshal(change.patch)
	if err != nil {
		return false, fmt.Errorf("marshalling derived patch: %w", err)
	}
	prePatchJSON, err := json.Marshal(change.prePatch)
	if err != nil {
		return false, fmt.Errorf("marshalling derived pre-patch state: %w", err)
	}

	installDerivedPatch(step, patchJSON, prePatchJSON, change.explanation)

	p.logger.Info("AI plan carried no appliable patch for a diagnosed resource change; Dorgu sized it deterministically",
		"action", action.Name, "field", target, "step", step.ID)
	return false, nil
}

// recordAbsentFields is F-05 expressed as structured data alongside its prose:
// a resource key the live container does not set was removed from the patch, so
// the step will not introduce it.
//
// Recording it matters beyond rendering. dropPatchlessPersonaUpdates uses the
// presence of a Safety entry to tell an empty step Dorgu has something to say
// about from an empty step that is simply noise, and this is a step of the first
// kind: the reader learns which field Dorgu declined to add, and why.
func recordAbsentFields(
	step *dorguv1.RemediationStep,
	requested *apiextensionsv1.JSON,
	dropped []string,
	ref *dorguv1.WorkloadRef,
) {
	values := patchLeafValues(requested.Raw)
	for _, field := range dropped {
		leaf := field[strings.LastIndex(field, ".")+1:]
		step.Safety = append(step.Safety, dorguv1.StepSafety{
			Rule:      dorguv1.SafetyRuleAbsentField,
			Verdict:   dorguv1.SafetyVerdictRejected,
			Field:     field,
			Requested: values[field],
			Message: fmt.Sprintf(
				"Left out: container %q on %s does not set %s today, so Dorgu will not introduce it as a side effect of another fix. Ask for it as its own change.",
				ref.Container, ref.Name, leaf),
		})
	}
}

// dropPatchlessPersonaUpdates removes persona-update steps that carry no patch
// and no explanation for why, and returns how many went.
//
// A persona-update step is the operator's own write. With no patch there is
// nothing to write, and it is not advice either: "update the ApplicationPersona"
// is not an instruction a reader can carry out. In clean-room run #4 exactly
// this step rendered as "(no changes)" under a plan that read like a fix, which
// is the shape the whole finding is about.
//
// A step whose patch a guardrail emptied is kept, because it carries a Safety
// record saying which field was refused and why. That is the difference between
// a step that explains an absence and a step that is one.
//
// This runs after ensureAppliableResourcePlan, so a step that could have been
// given a patch already has one.
func dropPatchlessPersonaUpdates(action *dorguv1.RemediationAction) int {
	kept := make([]dorguv1.RemediationStep, 0, len(action.Spec.Steps))
	for _, step := range action.Spec.Steps {
		empty := step.Patch == nil || len(step.Patch.Raw) == 0
		if step.Type == dorguv1.StepTypePersonaUpdate && empty && len(step.Safety) == 0 {
			continue
		}
		kept = append(kept, step)
	}

	dropped := len(action.Spec.Steps) - len(kept)
	if dropped == 0 {
		return 0
	}

	for i := range kept {
		kept[i].Order = int32(i + 1) //nolint:gosec // bounded by the plan's own step count
		kept[i].ID = fmt.Sprintf("step-%d", i+1)
	}
	action.Spec.Steps = kept
	return dropped
}

// personaUpdateStep returns the step the derived patch belongs on, or nil when
// the plan has none.
//
// A step the blast-radius guardrail just refused is preferred, so the refusal
// and the value that replaces it are recorded together rather than leaving the
// reader to match a refusal on step 1 against a fix on step 4.
func personaUpdateStep(action *dorguv1.RemediationAction) *dorguv1.RemediationStep {
	for i := range action.Spec.Steps {
		if action.Spec.Steps[i].Type == dorguv1.StepTypePersonaUpdate && hasBlastRadiusRefusal(action.Spec.Steps[i].Safety) {
			return &action.Spec.Steps[i]
		}
	}
	for i := range action.Spec.Steps {
		if action.Spec.Steps[i].Type == dorguv1.StepTypePersonaUpdate {
			return &action.Spec.Steps[i]
		}
	}
	return nil
}

// hasBlastRadiusRefusal reports whether the blast-radius guardrail refused a
// field on this step.
//
// It deliberately does not count an absent-field refusal. That one says Dorgu
// will not introduce a key the workload lacks, and a derived patch is never for
// such a key: the rule engine refuses to size one for exactly the same reason.
func hasBlastRadiusRefusal(safety []dorguv1.StepSafety) bool {
	for _, s := range safety {
		if s.Rule == dorguv1.SafetyRuleBlastRadius && s.Verdict == dorguv1.SafetyVerdictRejected {
			return true
		}
	}
	return false
}

// installDerivedPatch puts Dorgu's own patch on a step and takes ownership of
// the words beside it.
//
// The derived leaves are merged over whatever the step still carries rather than
// replacing it. Anything still there has already passed the guardrails, and
// dropping a change the model got right in order to add one it got wrong would
// be its own small version of this bug.
//
// The description becomes the rule engine's explanation, which states the old
// value, the new value and where the old value was read from. The rationale
// becomes Dorgu's, because the model's rationale argues for a number that is no
// longer the number being applied. A field the guardrail had refused is upgraded
// from "rejected" to "clamped" and given the value that replaces it, so one
// entry carries the whole story: asked for X, ceiling is Y, applying Z.
func installDerivedPatch(step *dorguv1.RemediationStep, patch, prePatch []byte, explanation string) {
	step.Type = dorguv1.StepTypePersonaUpdate
	step.AutoExecutable = true
	step.Patch = mergePatches(step.Patch, &apiextensionsv1.JSON{Raw: patch})
	step.PrePatchState = mergePatches(step.PrePatchState, &apiextensionsv1.JSON{Raw: prePatch})
	// The operator applies persona-update steps itself, so a command beside one
	// only invites the reader to make the same change twice.
	step.Command = ""

	recordDerivedValues(step, patchLeafValues(patch))

	step.Description = dorguAuthoredDescription(explanation, step.Safety)
	step.Rationale = derivedRationale(step.Safety)
}

// recordDerivedValues reconciles the step's structured verdicts with the values
// Dorgu is about to apply.
func recordDerivedValues(step *dorguv1.RemediationStep, derived map[string]string) {
	settled := make(map[string]bool, len(derived))

	for i := range step.Safety {
		entry := &step.Safety[i]
		permitted, ok := derived[entry.Field]
		if !ok || entry.Rule != dorguv1.SafetyRuleBlastRadius || entry.Verdict != dorguv1.SafetyVerdictRejected {
			continue
		}
		entry.Verdict = dorguv1.SafetyVerdictClamped
		entry.Permitted = permitted
		entry.Message = blastRadiusClampedMessage(*entry)
		settled[entry.Field] = true
	}

	for _, field := range sortedStringKeys(derived) {
		if settled[field] {
			continue
		}
		step.Safety = append(step.Safety, dorguv1.StepSafety{
			Rule:      dorguv1.SafetyRulePlanValidation,
			Verdict:   dorguv1.SafetyVerdictDerived,
			Field:     field,
			Permitted: derived[field],
			Message:   derivedMessage(field, derived[field]),
		})
	}
}

// refuseStepFields applies the blast-radius guardrail's refusals to a step.
//
// CR-04: the guardrail fired correctly and was then reported incoherently. The
// verdict was a "[safety:blast-radius] …" prefix spliced onto the model's own
// rationale, so a computed refusal read as part of the model's reasoning, one
// line below the model's claim that the same 16x change was "well within a 2x
// ceiling". The step rendered "(no changes)" while the headline diff still
// advertised the refused value.
//
// So: the verdict goes in Safety as numbers, the refused field is removed from
// the patch so nothing can advertise a change that will not happen, and Dorgu
// writes the sentences on any step a guardrail has touched. A step keeps its
// remaining fields and stays auto-executable if any survive.
func refuseStepFields(step *dorguv1.RemediationStep, refusals []SafetyViolation) {
	if len(refusals) == 0 {
		return
	}

	fields := make([]string, 0, len(refusals))
	for _, v := range refusals {
		entry := dorguv1.StepSafety{
			Rule:    dorguv1.SafetyRuleBlastRadius,
			Verdict: dorguv1.SafetyVerdictRejected,
			Message: v.Message,
		}
		if d := v.BlastRadius; d != nil {
			entry.Field = d.Field
			entry.Baseline = d.Baseline
			entry.Requested = d.Requested
			entry.Ratio = formatRatio(d.Ratio)
			entry.MaxRatio = formatRatio(d.MaxRatio)
			entry.Message = blastRadiusRejectedMessage(entry)
			fields = append(fields, d.Field)
		}
		step.Safety = append(step.Safety, entry)
	}

	step.Patch = prunePatchPaths(step.Patch, fields)
	step.PrePatchState = prunePatchPaths(step.PrePatchState, fields)
	if step.Patch == nil {
		step.AutoExecutable = false
	}

	step.Description = dorguAuthoredDescription(describeStepPatch(step.Patch), step.Safety)
	step.Rationale = guardedRationale()
}

// mergePatches returns overlay's leaves merged over base's, with overlay
// winning at any path both set. A nil or empty base yields overlay unchanged.
func mergePatches(base, overlay *apiextensionsv1.JSON) *apiextensionsv1.JSON {
	if base == nil || len(base.Raw) == 0 {
		return overlay
	}
	if overlay == nil || len(overlay.Raw) == 0 {
		return base
	}

	var baseMap, overlayMap map[string]any
	if err := json.Unmarshal(base.Raw, &baseMap); err != nil {
		return overlay
	}
	if err := json.Unmarshal(overlay.Raw, &overlayMap); err != nil {
		return base
	}

	// mergeMaps is the executor's own JSON merge-patch implementation, reused so
	// what the plan records and what the executor later applies agree by
	// construction rather than by two similar functions staying in step.
	raw, err := json.Marshal(mergeMaps(baseMap, overlayMap))
	if err != nil {
		return overlay
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

// prunePatchPaths returns the patch with the given dot-joined leaf paths
// removed, pruning any map left empty, and nil when nothing survives.
func prunePatchPaths(patch *apiextensionsv1.JSON, paths []string) *apiextensionsv1.JSON {
	if patch == nil || len(patch.Raw) == 0 || len(paths) == 0 {
		return patch
	}

	var root map[string]any
	if err := json.Unmarshal(patch.Raw, &root); err != nil {
		return patch
	}
	for _, path := range paths {
		deleteNestedPath(root, strings.Split(path, "."))
	}
	if len(root) == 0 {
		return nil
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return patch
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

// formatRatio renders a multiplier the way the guardrail's own messages do.
func formatRatio(ratio float64) string {
	return fmt.Sprintf("%.1fx", ratio)
}

// dorguAuthoredDescription is the sentence shown for a step Dorgu has ruled on:
// what the step does, then every verdict that shaped it.
func dorguAuthoredDescription(base string, safety []dorguv1.StepSafety) string {
	parts := make([]string, 0, len(safety)+1)
	if base == "" {
		base = "This step applies nothing."
	}
	parts = append(parts, base)
	for _, s := range safety {
		parts = append(parts, s.Message)
	}
	return strings.Join(parts, " ")
}

// describeStepPatch states what a persona patch sets, field by field.
func describeStepPatch(patch *apiextensionsv1.JSON) string {
	if patch == nil || len(patch.Raw) == 0 {
		return ""
	}
	values := patchLeafValues(patch.Raw)
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, field := range sortedStringKeys(values) {
		parts = append(parts, fmt.Sprintf("%s to %s", field, values[field]))
	}
	return "Set " + humanFieldList(parts) + " on the ApplicationPersona."
}

// guardedRationale replaces the model's reasoning on a step a guardrail has
// changed. The model argued for a value that is no longer the value in the
// patch, and leaving that argument in place beside Dorgu's verdict is what made
// the guardrail read as one more opinion.
func guardedRationale() string {
	return "Dorgu decides what its guardrails permit, so the outcome above is Dorgu's own arithmetic against the workload it observed, not the plan's account of it."
}

// derivedRationale explains a step whose value Dorgu supplied.
func derivedRationale(safety []dorguv1.StepSafety) string {
	for _, s := range safety {
		if s.Verdict == dorguv1.SafetyVerdictClamped {
			return guardedRationale()
		}
	}
	return "The plan named this resource change without a patch Dorgu could apply, so Dorgu sized it with the same calculation the rule-based path uses, against the workload it observed."
}

// blastRadiusRejectedMessage states a refusal with nothing left in its place.
func blastRadiusRejectedMessage(s dorguv1.StepSafety) string {
	return fmt.Sprintf(
		"Blast-radius guardrail: the plan asked to set %s to %s, which is %s the current %s. The ceiling is %s, so this field is refused and nothing will be applied for it.",
		s.Field, s.Requested, s.Ratio, s.Baseline, s.MaxRatio)
}

// blastRadiusClampedMessage states a refusal and the value Dorgu applies
// instead, including the caveat that the permitted value may not be enough.
func blastRadiusClampedMessage(s dorguv1.StepSafety) string {
	return fmt.Sprintf(
		"Blast-radius guardrail: the plan asked to set %s to %s, which is %s the current %s. The ceiling is %s, so Dorgu will apply %s instead, which may not be enough on its own.",
		s.Field, s.Requested, s.Ratio, s.Baseline, s.MaxRatio, s.Permitted)
}

// derivedMessage states that Dorgu supplied a value the plan did not.
func derivedMessage(field, permitted string) string {
	return fmt.Sprintf(
		"The plan named a change to %s but carried no patch Dorgu could apply, so Dorgu sized it to %s from the workload it observed.",
		field, permitted)
}
