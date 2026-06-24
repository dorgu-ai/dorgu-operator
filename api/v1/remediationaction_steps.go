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

package v1

import (
	"errors"
	"fmt"
)

// RemediationStep.Type values. Only StepTypePersonaUpdate may be AutoExecutable
// (see ValidateAutoExecutable); all other types are advisory.
const (
	StepTypePersonaUpdate = "persona-update"
	StepTypeWorkloadApply = "workload-apply"
	StepTypeRestart       = "restart"
	StepTypeScale         = "scale"
	StepTypeConfigChange  = "config-change"
	StepTypeManual        = "manual"
)

// Legacy RemediationActionDetail.Type values (the single-Action path). Note the
// legacy enum (persona-update;notification;git-pr) is narrower and partially
// disjoint from the step-type enum — legacyActionStepType maps between them.
const (
	ActionTypePersonaUpdate = StepTypePersonaUpdate
	ActionTypeNotification  = "notification"
	ActionTypeGitPR         = "git-pr"
)

// PlanSource values.
const (
	PlanSourceRuleBased   = "rule-based"
	PlanSourceAIAnthropic = "ai-anthropic"
)

// legacyActionStepType maps a legacy RemediationActionDetail.Type to a valid
// RemediationStep.Type. persona-update maps through unchanged; notification and
// git-pr are advisory and map to "manual" (they are applied by a human/platform,
// never auto-executed), keeping the synthesized step within the step-type enum.
func legacyActionStepType(actionType string) string {
	if actionType == ActionTypePersonaUpdate {
		return StepTypePersonaUpdate
	}
	return StepTypeManual
}

// EffectiveSteps returns the ordered plan as a uniform []RemediationStep so
// consumers (WS1 planner, a future executor) can read a single shape regardless
// of whether the plan was authored as Steps[] or the legacy single Action.
//
//   - When Steps is non-empty, a copy of Steps is returned (the plan of record).
//   - Otherwise a one-element slice is synthesized from the legacy Action, when
//     present (a non-empty Action.Type).
//   - When neither is set, an empty (nil) slice is returned.
//
// The synthesized step preserves the v1 step-safety invariant: it is
// AutoExecutable only when the legacy action is a persona-update.
func (r *RemediationAction) EffectiveSteps() []RemediationStep {
	if r == nil {
		return nil
	}

	if len(r.Spec.Steps) > 0 {
		// Deep copy per element: RemediationStep holds pointer fields (Patch,
		// PrePatchState), so a shallow copy would alias the source spec.
		out := make([]RemediationStep, len(r.Spec.Steps))
		for i := range r.Spec.Steps {
			r.Spec.Steps[i].DeepCopyInto(&out[i])
		}
		return out
	}

	legacy := r.Spec.Action
	if legacy.Type == "" {
		return nil
	}

	return []RemediationStep{
		{
			Order:          1,
			ID:             "legacy-action",
			Type:           legacyActionStepType(legacy.Type),
			Description:    r.Spec.Explanation,
			AutoExecutable: legacy.Type == ActionTypePersonaUpdate,
			Patch:          legacy.Patch.DeepCopy(),
			PrePatchState:  legacy.PrePatchState.DeepCopy(),
		},
	}
}

// ValidateAutoExecutable enforces the v1 step-safety invariant: a step may be
// AutoExecutable only when its Type is persona-update. This keeps the operator's
// non-negotiable guarantee that it never writes workloads — any non-persona
// step is advisory and must be applied by a human/CLI/platform.
//
// It returns a joined error describing every offending step (so a caller — e.g.
// an admission webhook — can surface all violations at once); nil when every
// step satisfies the invariant.
func (r *RemediationAction) ValidateAutoExecutable() error {
	if r == nil {
		return nil
	}
	var errs []error
	for _, step := range r.Spec.Steps {
		if step.AutoExecutable && step.Type != StepTypePersonaUpdate {
			errs = append(errs, fmt.Errorf(
				"step %d (id=%q, type=%q) is autoExecutable but only %q steps may be auto-executed",
				step.Order, step.ID, step.Type, StepTypePersonaUpdate,
			))
		}
	}
	return errors.Join(errs...)
}
