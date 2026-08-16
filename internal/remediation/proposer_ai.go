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
	"context"
	"encoding/json"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// proposeWithPlanner runs the AI planning path: build context, ask the planner
// for an ordered plan, map it to RemediationStep[], validate persona-update
// steps through the existing safety guardrails, and persist a RemediationAction.
//
// It returns (nil, err) to signal the caller to fall back to the rule-based
// proposer — used when context can't be built, the planner errors, or the plan
// is empty. Global safety violations (rate-limit, deny-list, concurrent) produce
// a (skip-result, nil) instead, matching the rule-based path.
func (p *Proposer) proposeWithPlanner(
	ctx context.Context,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
) (*ProposalResult, error) {
	rc, err := planner.BuildContext(ctx, p.client, diag, incident)
	if err != nil {
		return nil, fmt.Errorf("building remediation context: %w", err)
	}

	plan, err := p.planner.PlanRemediation(ctx, *rc)
	if err != nil {
		return nil, fmt.Errorf("planning remediation: %w", err)
	}
	if plan == nil || len(plan.Steps) == 0 {
		return nil, fmt.Errorf("planner returned an empty plan")
	}

	steps := mapPlannedSteps(plan, rc.AppPersona)

	action := p.buildPlanAction(diag, incident, plan, steps)

	// Validate persona-update steps through the existing safety guardrails.
	// Per-step blast-radius violations flag that step advisory; persona-wide
	// violations (rate-limit, concurrent, deny-list) skip the whole proposal.
	skip, err := p.applyStepSafety(ctx, action)
	if err != nil {
		return nil, fmt.Errorf("safety check: %w", err)
	}
	if skip != "" {
		return &ProposalResult{SkipReason: skip}, nil
	}

	// Recompute the back-compat Action from the first still-auto-executable
	// persona-update step (safety may have flagged the original choice).
	setBackCompatAction(action)

	// Defensive invariant guard: never persist an auto-executable non-persona
	// step (the operator must never write workloads).
	enforceAutoExecutableInvariant(action)

	// Safety and the invariant guard can both demote a step to advisory, so the
	// explanation's auto/advisory split is only true once every gate has run.
	//
	// This must stay ABOVE discloseBlastRadiusClamp: that appends the cap caveat
	// to Explanation, and recomputing afterwards would erase it.
	action.Spec.Explanation = planExplanation(action.Spec.Steps)

	// The planner is told to keep changes within ~2x an existing limit, so a plan
	// can arrive already pressed against the cap. Say so rather than presenting a
	// truncated fix as a confident one.
	if discloseBlastRadiusClamp(action) {
		p.logger.Info("plan sits at the blast-radius cap; disclosing the clamp in the plan summary",
			"action", action.Name, "confidence", action.Spec.Confidence)
	}

	if err := p.client.Create(ctx, action); err != nil {
		return nil, fmt.Errorf("creating RemediationAction: %w", err)
	}

	action.Status = dorguv1.RemediationActionStatus{Phase: phasePending}
	if err := p.client.Status().Update(ctx, action); err != nil {
		p.logger.Error(err, "RemediationAction created but status update failed", "name", action.Name)
	}

	return &ProposalResult{Action: action, Proposed: true}, nil
}

// mapPlannedSteps converts the planner's ordered steps into RemediationStep[].
// Only persona-update steps are auto-executable and carry a patch (plus a
// pre-patch snapshot of the current persona spec for rollback / blast-radius).
//
// Advisory steps may additionally carry a suggested kubectl command, which is
// sanitized here: the planner's output is model-authored, and this is the last
// point before it is persisted for the CLI to print to a human.
func mapPlannedSteps(plan *planner.RemediationPlan, persona *dorguv1.ApplicationPersona) []dorguv1.RemediationStep {
	steps := make([]dorguv1.RemediationStep, 0, len(plan.Steps))
	for _, ps := range plan.Steps {
		step := dorguv1.RemediationStep{
			Order:          ps.Order,
			ID:             fmt.Sprintf("step-%d", ps.Order),
			Type:           ps.Type,
			Description:    ps.Description,
			Rationale:      ps.Rationale,
			Risk:           ps.Risk,
			AutoExecutable: ps.Type == dorguv1.StepTypePersonaUpdate,
			Command:        advisoryCommand(ps),
		}

		if ps.Type == dorguv1.StepTypePersonaUpdate && len(ps.Patch) > 0 && json.Valid(ps.Patch) {
			step.Patch = &apiextensionsv1.JSON{Raw: append([]byte(nil), ps.Patch...)}
			if pre := snapshotPrePatch(persona, ps.Patch); pre != nil {
				step.PrePatchState = &apiextensionsv1.JSON{Raw: pre}
			}
		} else if ps.Type == dorguv1.StepTypePersonaUpdate {
			// A persona-update with no usable patch can't be auto-applied.
			step.AutoExecutable = false
		}

		steps = append(steps, step)
	}
	return steps
}

// advisoryCommand returns the sanitized copy-paste command for an advisory step.
//
// persona-update steps get none: the operator applies those itself, so printing
// a command beside them would invite a human to make the same change twice. A
// command that fails sanitization is dropped, so a step either shows a command
// that is safe to paste or shows none at all.
func advisoryCommand(ps planner.PlannedStep) string {
	if ps.Type == dorguv1.StepTypePersonaUpdate {
		return ""
	}
	return dorguv1.SanitizeStepCommand(ps.Command)
}

// buildPlanAction assembles the RemediationAction CRD for an AI plan. The
// back-compat single Action is set from the first auto-executable persona-update
// step (or left advisory if none); it is recomputed after safety in
// setBackCompatAction.
func (p *Proposer) buildPlanAction(
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
	plan *planner.RemediationPlan,
	steps []dorguv1.RemediationStep,
) *dorguv1.RemediationAction {
	namespace := diag.PersonaRef.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	healthCheckDuration := metav1.Duration{Duration: defaultHealthCheckAfter}

	action := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateActionName(incident, "ai-remediation"),
			Namespace: namespace,
			Labels: map[string]string{
				"dorgu.io/persona-kind":      diag.PersonaRef.Kind,
				"dorgu.io/persona-name":      diag.PersonaRef.Name,
				"dorgu.io/persona-namespace": namespace,
			},
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{
				Name:      incident.Name,
				Namespace: incident.Namespace,
			},
			PersonaRef:  *diag.PersonaRef,
			TrustLevel:  2,
			Steps:       steps,
			PlanSource:  dorguv1.PlanSourceAIAnthropic,
			PlanSummary: plan.RootCause,
			Explanation: planExplanation(steps),
			Confidence:  formatConfidence(plan.Confidence),
			Approval: &dorguv1.ApprovalSpec{
				Required: true,
			},
			Rollback: &dorguv1.RemediationRollbackSpec{
				Enabled:          true,
				HealthCheckAfter: &healthCheckDuration,
				MaxRetries:       1,
			},
		},
	}

	setBackCompatAction(action)
	return action
}

// applyStepSafety validates each persona-update step against the safety
// guardrails. Blast-radius violations flag the offending step advisory
// (AutoExecutable=false, annotated). Persona-wide violations (rate-limit,
// concurrent, deny-list) return a non-empty skip reason for the whole proposal.
func (p *Proposer) applyStepSafety(ctx context.Context, action *dorguv1.RemediationAction) (string, error) {
	globalSeen := make(map[string]struct{})
	var globalReasons []string

	checkedAny := false
	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		if !step.AutoExecutable || step.Type != dorguv1.StepTypePersonaUpdate {
			continue
		}
		checkedAny = true

		probe := probeActionForStep(action, step)
		result, err := p.safety.Check(ctx, probe)
		if err != nil {
			return "", err
		}
		for _, v := range result.Violations {
			if v.Rule == "blast-radius" {
				flagStepAdvisory(step, v.Message)
				continue
			}
			key := v.Rule + "|" + v.Message
			if _, ok := globalSeen[key]; !ok {
				globalSeen[key] = struct{}{}
				globalReasons = append(globalReasons, fmt.Sprintf("[%s] %s", v.Rule, v.Message))
			}
		}
	}

	// All-advisory plans still consume the rate limit / deny-list gate.
	if !checkedAny {
		probe := probeAdvisoryAction(action)
		result, err := p.safety.Check(ctx, probe)
		if err != nil {
			return "", err
		}
		for _, v := range result.Violations {
			key := v.Rule + "|" + v.Message
			if _, ok := globalSeen[key]; !ok {
				globalSeen[key] = struct{}{}
				globalReasons = append(globalReasons, fmt.Sprintf("[%s] %s", v.Rule, v.Message))
			}
		}
	}

	if len(globalReasons) > 0 {
		return fmt.Sprintf("safety check failed: %v", globalReasons), nil
	}
	return "", nil
}

// probeActionForStep builds a throwaway RemediationAction whose single Action is
// the given persona-update step, so the existing safety.Check (which inspects
// Spec.Action) validates that step's patch and the persona-wide guardrails.
func probeActionForStep(action *dorguv1.RemediationAction, step *dorguv1.RemediationStep) *dorguv1.RemediationAction {
	probe := action.DeepCopy()
	probe.Spec.Steps = nil
	probe.Spec.Action = dorguv1.RemediationActionDetail{
		Type:          dorguv1.StepTypePersonaUpdate,
		Patch:         step.Patch.DeepCopy(),
		PrePatchState: step.PrePatchState.DeepCopy(),
	}
	return probe
}

// probeAdvisoryAction builds a throwaway action with no auto-applicable change,
// so safety.Check still evaluates the persona-wide guardrails (rate limit,
// concurrency, deny-list) for an all-advisory plan.
func probeAdvisoryAction(action *dorguv1.RemediationAction) *dorguv1.RemediationAction {
	probe := action.DeepCopy()
	probe.Spec.Steps = nil
	probe.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
	return probe
}

// flagStepAdvisory demotes a persona-update step to advisory after a safety
// violation, recording why in the rationale.
func flagStepAdvisory(step *dorguv1.RemediationStep, reason string) {
	step.AutoExecutable = false
	note := fmt.Sprintf("[safety:blast-radius] %s", reason)
	if step.Rationale == "" {
		step.Rationale = note
	} else {
		step.Rationale = note + " — " + step.Rationale
	}
}

// setBackCompatAction points the legacy single Action at the first
// auto-executable persona-update step (the plan of record for the current
// executor). If no auto-executable persona-update step remains, the Action is
// left advisory (notification) so the executor never applies an unsafe patch.
func setBackCompatAction(action *dorguv1.RemediationAction) {
	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		if step.AutoExecutable && step.Type == dorguv1.StepTypePersonaUpdate {
			action.Spec.Action = dorguv1.RemediationActionDetail{
				Type:          dorguv1.StepTypePersonaUpdate,
				Patch:         step.Patch.DeepCopy(),
				PrePatchState: step.PrePatchState.DeepCopy(),
			}
			return
		}
	}
	action.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
}

// enforceAutoExecutableInvariant defensively clears AutoExecutable on any step
// that is not a persona-update, guaranteeing the operator never auto-applies a
// workload write regardless of what the model proposed.
func enforceAutoExecutableInvariant(action *dorguv1.RemediationAction) {
	for i := range action.Spec.Steps {
		if action.Spec.Steps[i].AutoExecutable && action.Spec.Steps[i].Type != dorguv1.StepTypePersonaUpdate {
			action.Spec.Steps[i].AutoExecutable = false
		}
	}
}

// planExplanation produces the one-line description of WHAT the plan does.
//
// It deliberately does not restate PlanSummary. PlanSummary is the root cause
// (why the incident happened); Explanation is the shape of the response (how
// many steps, how many Dorgu applies for you). They used to be the same
// sentence with a prefix, which meant `dorgu remediation diff` printed the same
// paragraph twice under two different headings (F-15).
func planExplanation(steps []dorguv1.RemediationStep) string {
	auto := 0
	for _, s := range steps {
		if s.AutoExecutable {
			auto++
		}
	}
	advisory := len(steps) - auto

	switch {
	case len(steps) == 0:
		return "AI remediation plan with no steps"
	case auto == 0:
		return fmt.Sprintf("AI remediation plan: %s, all advisory (nothing is applied for you)",
			pluralSteps(len(steps)))
	case advisory == 0:
		return fmt.Sprintf("AI remediation plan: %s, applied on approval", pluralSteps(len(steps)))
	default:
		return fmt.Sprintf("AI remediation plan: %s, %d applied on approval and %d advisory",
			pluralSteps(len(steps)), auto, advisory)
	}
}

// pluralSteps renders a step count with the right noun.
func pluralSteps(n int) string {
	if n == 1 {
		return "1 step"
	}
	return fmt.Sprintf("%d steps", n)
}

// formatConfidence renders a confidence score as the CRD's decimal-string
// format, clamped to [0,1].
func formatConfidence(c float64) string {
	if c < 0 {
		c = 0
	}
	if c > 1 {
		c = 1
	}
	return fmt.Sprintf("%.2f", c)
}

// snapshotPrePatch builds a pre-patch snapshot: the current persona-spec values
// at exactly the paths the patch touches. This gives safety.checkBlastRadius the
// old values to compare against and the executor a rollback target. Returns nil
// when no snapshot can be produced (e.g. persona unavailable or paths absent).
func snapshotPrePatch(persona *dorguv1.ApplicationPersona, patch json.RawMessage) json.RawMessage {
	if persona == nil {
		return nil
	}

	var patchMap map[string]any
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil
	}

	specBytes, err := json.Marshal(persona.Spec)
	if err != nil {
		return nil
	}
	var specMap map[string]any
	if err := json.Unmarshal(specBytes, &specMap); err != nil {
		return nil
	}
	// The patch is wrapped in {"spec": ...}; align the current object the same way.
	currentMap := map[string]any{"spec": specMap}

	snap := mirrorExistingPaths(patchMap, currentMap)
	if len(snap) == 0 {
		return nil
	}
	out, err := json.Marshal(snap)
	if err != nil {
		return nil
	}
	return out
}

// mirrorExistingPaths walks the patch structure and returns a map containing the
// current values at each leaf path of the patch that also exists in current.
// Paths absent from current are skipped (no prior value to record).
func mirrorExistingPaths(patch, current map[string]any) map[string]any {
	out := make(map[string]any)
	for key, patchVal := range patch {
		curVal, ok := current[key]
		if !ok {
			continue
		}
		patchChild, patchIsMap := patchVal.(map[string]any)
		curChild, curIsMap := curVal.(map[string]any)
		if patchIsMap && curIsMap {
			child := mirrorExistingPaths(patchChild, curChild)
			if len(child) > 0 {
				out[key] = child
			}
			continue
		}
		// Leaf (or shape mismatch): record the current value.
		out[key] = curVal
	}
	return out
}
