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
	"fmt"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// knownStepTypes are the typed steps a plan may contain. Anything else is not a
// plan the harness can reason about.
var knownStepTypes = []string{
	dorguv1.StepTypePersonaUpdate,
	dorguv1.StepTypeWorkloadApply,
	dorguv1.StepTypeRestart,
	dorguv1.StepTypeScale,
	dorguv1.StepTypeConfigChange,
	dorguv1.StepTypeManual,
}

// validateStage enforces the shape of a plan before anything is read from the
// cluster: typed steps, an allowlisted persona patch, operator-owned
// verification, and verdicts only the gate may write.
type validateStage struct{ g *Gate }

func (validateStage) Name() string { return StageValidate }

func (validateStage) Run(_ context.Context, cand *Candidate, d *Decision) error {
	if cand.Target.Name == "" {
		d.refuse(StageValidate, GuardrailTarget,
			"the candidate names no target persona, so there is nothing to clear", nil)
		return nil
	}

	if !validateStepTypes(cand, d) {
		return nil
	}

	// What kind of change this is belongs to the rules, not to the plan's wording.
	if !validateActionClass(cand, d) {
		return nil
	}

	// Shape before content: a command or patch the gate cannot parse with
	// certainty is refused, not interpreted (R9, R10).
	if !validateStepCommands(d) || !validatePatchShapes(d) {
		return nil
	}

	resetAuthorVerdicts(d)
	enforceAutoExecutableInvariant(d)

	if !allowlistPersonaPatches(cand, d) {
		return nil
	}

	boundVerification(cand, d)
	return nil
}

// validateStepTypes refuses a plan with an unknown step type, or a persona-update
// aimed at anything but an ApplicationPersona. It reports whether the plan may
// continue.
func validateStepTypes(cand *Candidate, d *Decision) bool {
	hasPersonaUpdate := d.Action.Type == dorguv1.ActionTypePersonaUpdate && d.Action.Patch != nil

	for i := range d.Steps {
		step := &d.Steps[i]
		if !slices.Contains(knownStepTypes, step.Type) {
			// The offending value is author- or model-supplied, so it is not echoed.
			d.refuse(StageValidate, GuardrailStepType,
				fmt.Sprintf("step %d has a type that is not one of the typed steps the harness accepts", step.Order),
				map[string]string{"step": fmt.Sprint(step.Order)})
			return false
		}
		if step.Type == dorguv1.StepTypePersonaUpdate {
			hasPersonaUpdate = true
		}
	}

	if hasPersonaUpdate && cand.Target.Kind != kindApplicationPersona {
		d.refuse(StageValidate, GuardrailTarget,
			"a persona-update may only target an ApplicationPersona", map[string]string{"kind": cand.Target.Kind})
		return false
	}

	d.pass(StageValidate, GuardrailStepType, "every step is a typed step", nil)
	return true
}

// ClassResourceAdjustment is the rules' SuggestedAction for a diagnosis whose
// remedy is a persona resource change, the one class that may carry a
// persona-update. It is the value the rule engine writes; the gate does not
// import the diagnosis package, so it is named here.
const ClassResourceAdjustment = "resource-adjustment"

// validateActionClass refuses a plan whose action class is not the one the rules
// recommended (V2). The plan's steps are authored by a model on the AI path, and
// a model can word a plan however it likes; it cannot turn the rules' "fix the
// deployment" (an image-pull failure) into a persona-update memory patch that
// then clears every safety check on its own merits. A persona-update is admitted
// only when the rules recommended a resource adjustment. An advisory plan
// (manual, restart, scale, ...) applies nothing, so it is admitted whatever the
// rules said. The check is proposal-only: at approval the spec is pinned to the
// generation the proposal cleared. An empty recommendation is not a match.
func validateActionClass(cand *Candidate, d *Decision) bool {
	if d.Phase != PhaseProposal || !hasPersonaUpdate(d) {
		return true
	}
	if cand.Facts.SuggestedAction == ClassResourceAdjustment {
		return true
	}
	d.refuse(StageValidate, GuardrailActionClass,
		"the plan changes the persona, but the rules did not recommend a resource adjustment for this diagnosis; the rules own the kind of change",
		map[string]string{"rulesAction": truncate(cand.Facts.SuggestedAction, 64)})
	return false
}

// resetAuthorVerdicts discards every verdict the plan arrived with. A verdict is
// the gate's to write: one carried in by an author or a model (a forged "within
// the 2x cap") is data about what they want believed, not about what was
// measured. The verdicts that stand are the ones recorded below.
func resetAuthorVerdicts(d *Decision) {
	discarded := 0
	for i := range d.Steps {
		discarded += len(d.Steps[i].Safety)
		d.Steps[i].Safety = nil
	}
	if discarded > 0 {
		d.pass(StageValidate, GuardrailVerdictsReset,
			fmt.Sprintf("discarded %d verdict(s) the plan arrived with; the gate recomputes every one", discarded),
			map[string]string{"discarded": fmt.Sprint(discarded)})
	}
}

// enforceAutoExecutableInvariant clears AutoExecutable on any step that is not a
// persona-update. The operator never writes workloads, whatever a plan says.
func enforceAutoExecutableInvariant(d *Decision) {
	for i := range d.Steps {
		step := &d.Steps[i]
		if step.AutoExecutable && step.Type != dorguv1.StepTypePersonaUpdate {
			step.AutoExecutable = false
			d.clamp(StageValidate, GuardrailAutoExecutable,
				fmt.Sprintf("step %d is not a persona-update, so it cannot be applied by the operator", step.Order),
				map[string]string{"step": fmt.Sprint(step.Order)})
		}
	}
}

// allowlistPersonaPatches strips every leaf of a persona-update patch that is not
// a container resource quantity: persona-update may touch resources.* only, and
// only with a positive quantity. A null (a merge-patch delete), a boolean, or a
// path under policies.* is removed and recorded. It reports whether the plan may
// continue.
func allowlistPersonaPatches(_ *Candidate, d *Decision) bool {
	stripped := false
	for i := range d.Steps {
		step := &d.Steps[i]
		if step.Type != dorguv1.StepTypePersonaUpdate || step.Patch == nil {
			continue
		}
		patch, refusals := stripDisallowedLeaves(step.Patch)
		if len(refusals) == 0 {
			continue
		}
		stripped = true
		step.Patch = patch
		step.PrePatchState = PrunePatchPaths(step.PrePatchState, fieldsOf(refusals))
		if step.Patch == nil {
			step.AutoExecutable = false
		}
		step.Safety = append(step.Safety, refusals...)
		d.clamp(StageValidate, GuardrailFieldAllowlist,
			fmt.Sprintf("step %d: removed %d change(s) outside the resource allowlist; a persona-update may only set CPU or memory quantities under resources",
				step.Order, len(refusals)),
			map[string]string{"step": fmt.Sprint(step.Order), "removed": fmt.Sprint(len(refusals))})
	}

	if d.Action.Type == dorguv1.ActionTypePersonaUpdate && d.Action.Patch != nil {
		patch, refusals := stripDisallowedLeaves(d.Action.Patch)
		if len(refusals) > 0 {
			if patch == nil {
				d.refuse(StageValidate, GuardrailFieldAllowlist,
					"the change touches nothing a persona-update may set; a persona-update may only set CPU or memory quantities under resources",
					map[string]string{"removed": fmt.Sprint(len(refusals))})
				return false
			}
			stripped = true
			d.Action.Patch = patch
			d.Action.PrePatchState = PrunePatchPaths(d.Action.PrePatchState, fieldsOf(refusals))
			d.clamp(StageValidate, GuardrailFieldAllowlist,
				fmt.Sprintf("removed %d change(s) outside the resource allowlist", len(refusals)),
				map[string]string{"removed": fmt.Sprint(len(refusals))})
		}
	}

	if !stripped {
		d.pass(StageValidate, GuardrailFieldAllowlist, "every persona change is inside the resource allowlist", nil)
	}
	return true
}

// stripDisallowedLeaves removes every leaf of a merge patch that is not a
// positive CPU or memory quantity at an allowlisted resource path, returning the
// surviving patch (nil when nothing survives) and a record of each removal.
func stripDisallowedLeaves(patch *apiextensionsv1.JSON) (*apiextensionsv1.JSON, []dorguv1.StepSafety) {
	var removed []string
	var records []dorguv1.StepSafety

	values := PatchLeafValues(patch.Raw)
	for _, path := range PatchLeafPaths(patch.Raw) {
		if reason := disallowedLeaf(path, values); reason != "" {
			removed = append(removed, path)
			records = append(records, dorguv1.StepSafety{
				Rule:      dorguv1.SafetyRulePlanValidation,
				Verdict:   dorguv1.SafetyVerdictRejected,
				Field:     path,
				Requested: values[path],
				Message:   reason,
			})
		}
	}
	if len(removed) == 0 {
		return patch, nil
	}
	slices.Sort(removed)
	slices.SortFunc(records, func(a, b dorguv1.StepSafety) int {
		if a.Field < b.Field {
			return -1
		}
		if a.Field > b.Field {
			return 1
		}
		return 0
	})
	return PrunePatchPaths(patch, removed), records
}

// disallowedLeaf says why a leaf of a persona-update patch is not allowed, or ""
// when it is.
func disallowedLeaf(path string, values map[string]string) string {
	if !IsResourcePath(path) {
		return fmt.Sprintf("Dorgu will not change %s: a persona-update may only set CPU or memory quantities under resources.", path)
	}
	value, isString := values[path]
	if !isString {
		return fmt.Sprintf("Dorgu will not apply %s: it is not a resource quantity (a null deletes the limit, and a deletion is not a remediation).", path)
	}
	qty, err := resource.ParseQuantity(value)
	if err != nil || qty.Sign() <= 0 {
		return fmt.Sprintf("Dorgu will not apply %s: %q is not a positive resource quantity.", path, value)
	}
	return ""
}

func fieldsOf(records []dorguv1.StepSafety) []string {
	fields := make([]string, 0, len(records))
	for _, r := range records {
		fields = append(fields, r.Field)
	}
	return fields
}

// boundVerification applies the operator's own verification and rollback
// settings. The author's request, if any, is compared to them only to say that
// it was ignored.
func boundVerification(cand *Candidate, d *Decision) {
	req := cand.RequestedVerify
	if req == nil {
		d.pass(StageValidate, GuardrailVerifyBounds, "operator-owned verification and rollback apply", nil)
		return
	}

	differs := !req.Enabled || req.MaxRetries != d.Verify.MaxRetries ||
		req.HealthCheckAfter == nil || req.HealthCheckAfter.Duration != d.Verify.HealthCheckAfter.Duration
	if !differs {
		d.pass(StageValidate, GuardrailVerifyBounds, "operator-owned verification and rollback apply", nil)
		return
	}

	numbers := map[string]string{
		"requestedRollbackEnabled":  fmt.Sprint(req.Enabled),
		"requestedHealthCheckAfter": "unset",
		"appliedHealthCheckAfter":   d.Verify.HealthCheckAfter.Duration.String(),
	}
	if req.HealthCheckAfter != nil {
		numbers["requestedHealthCheckAfter"] = req.HealthCheckAfter.Duration.String()
	}
	d.clamp(StageValidate, GuardrailVerifyBounds,
		"verification and rollback are operator-owned: the plan's own healthCheckAfter and rollback settings were ignored",
		numbers)
}

// validateStepCommands refuses a plan whose step carries a command that is not one
// plain kubectl invocation (R9). The gate checks this itself: it does not rely on
// the planner's sanitizer having run, because the model's text is not trusted to
// have been cleaned by the code that happens to precede the gate. It reports
// whether the plan may continue.
func validateStepCommands(d *Decision) bool {
	for i := range d.Steps {
		step := &d.Steps[i]
		if step.Command == "" || IsPlainKubectlCommand(step.Command) {
			continue
		}
		// The command is model-supplied and may be hostile, so it is not echoed.
		d.refuse(StageValidate, GuardrailStepCommand,
			fmt.Sprintf("step %d carries a command that is not a single plain kubectl invocation", step.Order),
			map[string]string{"step": fmt.Sprint(step.Order)})
		return false
	}
	return true
}

// validatePatchShapes refuses a persona patch that is not a JSON object, or that
// has a key containing a dot (R10). Leaf paths are dot-joined, so a key
// "limits.memory" is indistinguishable from the nested path limits -> memory:
// it was reported as removed while staying in the patch, or collided with a real
// path. Both ends of that are refused rather than interpreted. It reports whether
// the plan may continue.
func validatePatchShapes(d *Decision) bool {
	for i := range d.Steps {
		step := &d.Steps[i]
		if step.Type != dorguv1.StepTypePersonaUpdate || step.Patch == nil || len(step.Patch.Raw) == 0 {
			continue
		}
		if reason := patchShapeProblem(step.Patch.Raw); reason != "" {
			d.refuse(StageValidate, GuardrailPatchShape,
				fmt.Sprintf("step %d: the persona patch %s", step.Order, reason),
				map[string]string{"step": fmt.Sprint(step.Order)})
			return false
		}
	}
	if d.Action.Type == dorguv1.ActionTypePersonaUpdate && d.Action.Patch != nil && len(d.Action.Patch.Raw) > 0 {
		if reason := patchShapeProblem(d.Action.Patch.Raw); reason != "" {
			d.refuse(StageValidate, GuardrailPatchShape, "the persona patch "+reason, nil)
			return false
		}
	}
	return true
}

// patchShapeProblem says what is wrong with the shape of a merge patch, or "".
func patchShapeProblem(raw []byte) string {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "is not valid JSON"
	}
	object, ok := root.(map[string]any)
	if !ok || len(object) == 0 {
		return "is not a JSON object with content"
	}
	if hasDottedKey(object) {
		return "has a key containing a dot, which cannot be told apart from a nested path"
	}
	return ""
}

func hasDottedKey(node map[string]any) bool {
	for key, value := range node {
		if strings.Contains(key, ".") {
			return true
		}
		if child, ok := value.(map[string]any); ok && hasDottedKey(child) {
			return true
		}
	}
	return false
}
