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
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

// Condition reasons shared between the controller that writes them and the
// safety checker that reads them.
const (
	// ReasonAdvisoryOnly marks a plan that settled on approval without applying
	// anything, because it contains no auto-applicable step.
	ReasonAdvisoryOnly = "AdvisoryOnly"

	// ReasonPreconditionRejected marks a plan the executor refused before
	// touching the cluster. Nothing was applied, so it is not an apply failure.
	ReasonPreconditionRejected = "PreconditionRejected"
)

// HasAutoApplicableChange reports whether anything in this plan can be applied by
// the operator: an auto-executable persona-update step carrying a patch, or the
// legacy single Action when it is a persona-update with a patch.
//
// A plan without one is advisory. Approving it used to run it into the executor,
// which rejected the action type, marked the action Failed, and tripped the
// 30-minute failure cooldown for the app: a blackout earned by following the
// tool's own printed instruction (F-03).
func (r *RemediationAction) HasAutoApplicableChange() bool {
	if r == nil {
		return false
	}

	for _, step := range r.Spec.Steps {
		if step.AutoExecutable && step.Type == StepTypePersonaUpdate && hasPatch(step.Patch) {
			return true
		}
	}

	if len(r.Spec.Steps) > 0 {
		// The Steps[] plan is the plan of record; the legacy Action is derived
		// from it, so it cannot contribute a change the steps do not have.
		return false
	}

	return r.Spec.Action.Type == ActionTypePersonaUpdate && hasPatch(r.Spec.Action.Patch)
}

// hasPatch reports whether a patch field carries a payload.
func hasPatch(patch *apiextensionsv1.JSON) bool {
	return patch != nil && len(patch.Raw) > 0
}

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

// MaxStepCommandLength bounds RemediationStep.Command, matching the CRD's
// MaxLength so a command rejected by the API server never reaches it.
const MaxStepCommandLength = 1024

// stepCommandPrefix is the only command form a step may suggest. Restricting to
// kubectl keeps the suggestion inspectable: the reader already knows what
// kubectl does, and no other binary can be smuggled in.
const stepCommandPrefix = "kubectl "

// stepCommandForbidden are the characters that let a pasted one-liner do more
// than the command it appears to be: chaining, piping, redirecting, command
// substitution, and variable expansion.
const stepCommandForbidden = ";&|<>`$\n\r"

// SanitizeStepCommand returns cmd if it is safe to print as a copy-paste
// suggestion, and "" otherwise.
//
// A step command may be authored by a language model, so it is untrusted input
// on its way to a human's shell. The bar it has to clear is deliberately blunt:
// one line, a kubectl invocation, no shell metacharacters, within the CRD's
// length bound. A command that fails any of these is dropped rather than
// rewritten, because a half-corrected command is worse than none.
//
// This is a display guard, not an execution sandbox. Nothing in Dorgu runs the
// result; the human running it is the one deciding.
func SanitizeStepCommand(cmd string) string {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > MaxStepCommandLength {
		return ""
	}
	if !strings.HasPrefix(trimmed, stepCommandPrefix) {
		return ""
	}
	if strings.ContainsAny(trimmed, stepCommandForbidden) {
		return ""
	}
	return trimmed
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
