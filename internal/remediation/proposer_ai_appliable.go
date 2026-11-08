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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
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
	if target == "" || gate.AppliesDiagnosedChange(action.Spec.Steps, action.Spec.Action, target) {
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
	step.Patch = gate.MergePatches(step.Patch, &apiextensionsv1.JSON{Raw: patch})
	step.PrePatchState = gate.MergePatches(step.PrePatchState, &apiextensionsv1.JSON{Raw: prePatch})
	// The operator applies persona-update steps itself, so a command beside one
	// only invites the reader to make the same change twice.
	step.Command = ""

	recordDerivedValues(step, gate.PatchLeafValues(patch))

	step.Description = gate.DorguAuthoredDescription(explanation, step.Safety)
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
		entry.Message = gate.BlastRadiusClampedMessage(*entry)
		settled[entry.Field] = true
	}

	for _, field := range gate.SortedStringKeys(derived) {
		if settled[field] {
			continue
		}
		step.Safety = append(step.Safety, dorguv1.StepSafety{
			Rule:      dorguv1.SafetyRulePlanValidation,
			Verdict:   dorguv1.SafetyVerdictDerived,
			Field:     field,
			Permitted: derived[field],
			Message:   gate.DerivedMessage(field, derived[field]),
		})
	}
}

// derivedRationale explains a step whose value Dorgu supplied.
func derivedRationale(safety []dorguv1.StepSafety) string {
	for _, s := range safety {
		if s.Verdict == dorguv1.SafetyVerdictClamped {
			return gate.GuardedRationale()
		}
	}
	return "The plan named this resource change without a patch Dorgu could apply, so Dorgu sized it with the same calculation the rule-based path uses, against the workload it observed."
}
