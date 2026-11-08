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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	// DefaultMaxRemediationsPerHour is the default rate limit per persona.
	DefaultMaxRemediationsPerHour = 5

	// FailedCooldown is the quiet period after a failed remediation.
	FailedCooldown = 30 * time.Minute

	// MaxBlastRadiusMultiplier caps resource increases to 2x.
	MaxBlastRadiusMultiplier = 2.0

	// MaxResourceDecrease caps resource decreases to 50%.
	MaxResourceDecrease = 0.5

	phaseApproved  = "Approved"
	phaseApplying  = "Applying"
	phaseVerifying = "Verifying"
	phaseFailed    = "Failed"

	// conditionApplied mirrors the controller's Applied condition type, whose
	// reason distinguishes a rejected-before-apply action from a real failure.
	conditionApplied = "Applied"
)

// guardrailsStage is the set of persona-wide and per-change limits: the
// deny-list, the hourly rate limit, one remediation at a time, the quiet period
// after a failure, and the blast-radius cap on how far one change may move a
// value.
//
// Sibling remediations are selected by their spec's target persona, never by
// labels: a label is written by whoever created the object, so a creator who
// leaves the labels off was invisible to every limit.
type guardrailsStage struct{ g *Gate }

func (guardrailsStage) Name() string { return StageGuardrails }

func (s guardrailsStage) Run(ctx context.Context, cand *Candidate, d *Decision) error {
	hadChange := hasAutoApplicable(d)

	if err := s.personaWide(ctx, cand, d); err != nil {
		return err
	}

	s.dropAbsentFields(d)
	s.blastRadius(d)

	if cand.Final {
		s.completeness(cand, d)
	}

	// An approval that clamps away every change it came to apply has nothing left
	// to approve. At proposal the plan may still be repaired by the proposer.
	if d.Phase == PhaseApprovalRecheck && hadChange && d.Result != ResultRefuse && !hasAutoApplicable(d) {
		d.refuse(StageGuardrails, GuardrailNothingLeft,
			"every change in this plan is now outside the guardrails measured against the live workload, so nothing is left to apply", nil)
	}
	return nil
}

// completeness is the invariant behind CR-01: no plan is written that applies
// nothing when a resource change was diagnosed. It is asserted on the final shape
// of the plan, not trusted to the code that built it, because a plan that diagnoses
// a resource problem and changes no resource reads as a fix and is not one.
func (s guardrailsStage) completeness(cand *Candidate, d *Decision) {
	target := cand.Facts.TargetPath
	if target == "" || cand.Facts.AdvisoryAllowed {
		return
	}
	if AppliesDiagnosedChange(d.Steps, d.Action, target) {
		d.pass(StageGuardrails, GuardrailNothingApplies, "the plan applies the diagnosed change", nil)
		return
	}
	d.refuse(StageGuardrails, GuardrailNothingApplies,
		fmt.Sprintf("refusing to persist a plan that applies nothing for a diagnosed change to %s: no step carries an appliable persona-update patch for it", target),
		map[string]string{"target": target})
}

// AppliesDiagnosedChange reports whether some auto-executable persona-update step
// patches the field the diagnosis calls for.
//
// It asks about the field, not merely about appliability. A plan that raises the
// memory REQUEST while the diagnosis calls for the memory LIMIT is appliable and
// still leaves the OOM in place.
func AppliesDiagnosedChange(steps []dorguv1.RemediationStep, action dorguv1.RemediationActionDetail, target string) bool {
	for i := range steps {
		step := &steps[i]
		if step.AutoExecutable && step.Type == dorguv1.StepTypePersonaUpdate &&
			step.Patch != nil && PatchTouchesPath(step.Patch.Raw, target) {
			return true
		}
	}

	// When Steps is populated it is the plan of record and the legacy Action is
	// derived from it. Otherwise the Action is the plan, the shape the rule-based
	// path produces.
	if len(steps) > 0 {
		return false
	}
	return action.Type == dorguv1.ActionTypePersonaUpdate && action.Patch != nil &&
		PatchTouchesPath(action.Patch.Raw, target)
}

// personaWide runs the checks that depend on the persona and its sibling
// remediations rather than on the contents of the change.
func (s guardrailsStage) personaWide(ctx context.Context, cand *Candidate, d *Decision) error {
	namespace := cand.Target.Namespace
	if namespace == "" {
		namespace = cand.Namespace
	}
	if v := s.g.NamespaceDenied(ctx, namespace); v != nil {
		d.refuse(StageGuardrails, GuardrailDenyList, v.Message, nil)
	} else {
		d.pass(StageGuardrails, GuardrailDenyList, "namespace is not protected", nil)
	}

	siblings, err := s.g.siblings(ctx, cand)
	if err != nil {
		return err
	}

	if d.Phase == PhaseProposal {
		s.rateLimit(ctx, cand, d, siblings)
	}
	s.concurrent(cand, d, siblings)
	s.failedCooldown(cand, d, siblings)
	return nil
}

// rateLimit enforces the per-persona hourly cap. It fails closed: a policy that
// cannot be read is not the default policy, it is an unknown one.
func (s guardrailsStage) rateLimit(ctx context.Context, _ *Candidate, d *Decision, siblings []dorguv1.RemediationAction) {
	maxPerHour, err := s.g.maxRemediationsPerHour(ctx)
	if err != nil {
		s.g.logger.Error(err, "cannot read the ClusterPersona rate limit; refusing until it is readable")
		d.refuse(StageGuardrails, GuardrailRateLimit,
			"the rate limit could not be read, so the persona cannot be cleared", nil)
		return
	}

	cutoff := s.g.now().Add(-time.Hour)
	recent := 0
	for i := range siblings {
		if siblings[i].CreationTimestamp.After(cutoff) {
			recent++
		}
	}

	numbers := map[string]string{"recent": fmt.Sprint(recent), "max": fmt.Sprint(maxPerHour)}
	if recent >= int(maxPerHour) {
		d.refuse(StageGuardrails, GuardrailRateLimit,
			fmt.Sprintf("rate limit exceeded: %d remediations for persona in last hour (max %d)", recent, maxPerHour), numbers)
		return
	}
	d.pass(StageGuardrails, GuardrailRateLimit, "within the hourly limit", numbers)
}

// concurrent allows one remediation in flight per persona. At approval, two plans
// approved together are ordered by approval time (then name) so exactly one
// proceeds instead of each refusing the other.
func (s guardrailsStage) concurrent(cand *Candidate, d *Decision, siblings []dorguv1.RemediationAction) {
	for i := range siblings {
		sib := &siblings[i]
		switch sib.Status.Phase {
		case phaseApplying, phaseVerifying:
		case phaseApproved:
			if d.Phase == PhaseApprovalRecheck && !approvedBefore(sib, cand) {
				continue
			}
		default:
			continue
		}
		d.refuse(StageGuardrails, GuardrailConcurrent,
			fmt.Sprintf("concurrent remediation %s is in phase %s for persona %s/%s",
				sib.Name, sib.Status.Phase, personaKey(cand).Namespace, cand.Target.Name),
			map[string]string{"sibling": sib.Name, "phase": sib.Status.Phase})
		return
	}
	d.pass(StageGuardrails, GuardrailConcurrent, "no other remediation is in flight for the persona", nil)
}

// approvedBefore reports whether sibling's approval orders ahead of the
// candidate's.
func approvedBefore(sib *dorguv1.RemediationAction, cand *Candidate) bool {
	if cand.ApprovedAt == nil || sib.Status.ApprovedAt == nil {
		// Without both timestamps neither can claim to be first, so each defers.
		return true
	}
	if sib.Status.ApprovedAt.Before(cand.ApprovedAt) {
		return true
	}
	if cand.ApprovedAt.Before(sib.Status.ApprovedAt) {
		return false
	}
	return sib.Name < cand.Name
}

// failedCooldown holds a persona quiet after a remediation that actually went
// wrong. A plan the executor refused before touching the cluster changed nothing
// and does not count (F-03).
func (s guardrailsStage) failedCooldown(cand *Candidate, d *Decision, siblings []dorguv1.RemediationAction) {
	for i := range siblings {
		sib := &siblings[i]
		if sib.Status.Phase != phaseFailed || rejectedBeforeApply(sib) {
			continue
		}
		failedAt := sib.CreationTimestamp.Time
		if sib.Status.AppliedAt != nil {
			failedAt = sib.Status.AppliedAt.Time
		}
		if s.g.now().Sub(failedAt) < FailedCooldown {
			d.refuse(StageGuardrails, GuardrailFailedCooldown,
				fmt.Sprintf("failed remediation %s within %s cooldown period for persona %s/%s",
					sib.Name, FailedCooldown, personaKey(cand).Namespace, cand.Target.Name),
				map[string]string{"sibling": sib.Name, "cooldown": FailedCooldown.String()})
			return
		}
	}
	d.pass(StageGuardrails, GuardrailFailedCooldown, "no recent failed remediation for the persona", nil)
}

// rejectedBeforeApply reports whether a Failed action was refused before anything
// was written to the cluster, as recorded on its Applied condition.
func rejectedBeforeApply(action *dorguv1.RemediationAction) bool {
	if action.Status.AppliedAt != nil {
		return false
	}
	for _, c := range action.Status.Conditions {
		if c.Type == conditionApplied && c.Reason == dorguv1.ReasonPreconditionRejected {
			return true
		}
	}
	return false
}

// dropAbsentFields removes every leaf that would introduce a resource key the
// live container does not set (F-05): approving a memory fix must never quietly
// add a CPU limit.
func (s guardrailsStage) dropAbsentFields(d *Decision) {
	if !Resolved(d.WorkloadRef) {
		return
	}

	for i := range d.Steps {
		step := &d.Steps[i]
		if step.Type != dorguv1.StepTypePersonaUpdate || step.Patch == nil {
			continue
		}
		patch, dropped := DropAbsentResourceKeys(step.Patch, d.WorkloadRef)
		if len(dropped) == 0 {
			continue
		}
		recordAbsentFields(step, step.Patch, dropped, d.WorkloadRef)
		step.Rationale = AppendNote(step.Rationale, AbsentKeyNote(d.WorkloadRef, dropped))
		step.Patch = patch
		step.PrePatchState = PrunePatchPaths(step.PrePatchState, dropped)
		if step.Patch == nil {
			step.AutoExecutable = false
		}
		d.clamp(StageGuardrails, GuardrailAbsentField,
			fmt.Sprintf("step %d: left out %d field(s) the live container does not set", step.Order, len(dropped)),
			map[string]string{"step": fmt.Sprint(step.Order), "fields": fmt.Sprint(len(dropped))})
	}

	// The executor applies Action.Patch whatever the steps say, so it is measured
	// too (R1). With no steps it is the whole plan and an unmeasurable change is
	// refused; beside steps it is narrowed like a step.
	if actionAppliable(d) {
		patch, dropped := DropAbsentResourceKeys(d.Action.Patch, d.WorkloadRef)
		if len(dropped) == 0 {
			return
		}
		if patch == nil && len(d.Steps) == 0 {
			d.refuse(StageGuardrails, GuardrailAbsentField,
				"the change would introduce a resource field the live container has never had", nil)
			return
		}
		d.Action.PrePatchState = PrunePatchPaths(d.Action.PrePatchState, dropped)
		narrowAction(d, patch)
		d.clamp(StageGuardrails, GuardrailAbsentField,
			fmt.Sprintf("left out %d field(s) the live container does not set", len(dropped)), nil)
	}
}

// narrowAction installs the surviving patch on the legacy Action. With nothing
// left the Action becomes advisory, so the executor has nothing to apply.
func narrowAction(d *Decision, patch *apiextensionsv1.JSON) {
	if patch == nil {
		d.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
		return
	}
	d.Action.Patch = patch
}

// blastRadius measures every persona-update change against the live baseline, and
// the Action the executor applies as well as the steps (R1). On a plan with steps
// an offending field is removed and recorded; on a legacy single action with no
// steps, which has nothing to fall back to, the whole change is refused.
//
// A field with no baseline cannot pass (R3): a value nothing was measured against
// is not within any cap.
func (s guardrailsStage) blastRadius(d *Decision) {
	measured := false
	for i := range d.Steps {
		step := &d.Steps[i]
		if !step.AutoExecutable || step.Type != dorguv1.StepTypePersonaUpdate || step.Patch == nil {
			continue
		}
		measured = true
		s.dropUnmeasured(d, step)
		if step.Patch == nil {
			continue
		}
		violations := blastRadiusViolations(step.Patch.Raw, baselineValues(step.Patch.Raw, d))
		if len(violations) == 0 {
			continue
		}
		refuseStepFields(step, violations)
		for _, v := range violations {
			d.clamp(StageGuardrails, GuardrailBlastRadius,
				fmt.Sprintf("step %d: %s", step.Order, v.Message), blastNumbers(v))
		}
	}
	if actionAppliable(d) {
		measured = true
		s.measureAction(d)
	}
	if measured && !hasVerdict(d, GuardrailBlastRadius, "") && !hasVerdict(d, GuardrailNoBaseline, "") {
		d.pass(StageGuardrails, GuardrailBlastRadius, "every change is within the blast-radius cap of the live value", nil)
	}
}

// measureAction applies the blast-radius and no-baseline rules to Action.Patch.
func (s guardrailsStage) measureAction(d *Decision) {
	raw := d.Action.Patch.Raw
	standalone := len(d.Steps) == 0

	if unmeasured := unmeasuredFields(raw, baselineValues(raw, d)); len(unmeasured) > 0 {
		if standalone {
			d.refuse(StageGuardrails, GuardrailNoBaseline,
				fmt.Sprintf("no live or persona value exists for %s, so the change cannot be measured against anything",
					HumanFieldList(unmeasured)),
				map[string]string{"fields": fmt.Sprint(len(unmeasured))})
			return
		}
		narrowAction(d, PrunePatchPaths(d.Action.Patch, unmeasured))
		d.Action.PrePatchState = PrunePatchPaths(d.Action.PrePatchState, unmeasured)
		d.clamp(StageGuardrails, GuardrailNoBaseline,
			fmt.Sprintf("left out %d field(s) with nothing to measure them against", len(unmeasured)),
			map[string]string{"fields": fmt.Sprint(len(unmeasured))})
		if !actionAppliable(d) {
			return
		}
		raw = d.Action.Patch.Raw
	}

	violations := blastRadiusViolations(raw, baselineValues(raw, d))
	if standalone {
		for _, v := range violations {
			d.refuse(StageGuardrails, GuardrailBlastRadius, v.Message, blastNumbers(v))
		}
		return
	}
	if len(violations) == 0 {
		return
	}
	fields := make([]string, 0, len(violations))
	for _, v := range violations {
		if v.BlastRadius != nil {
			fields = append(fields, v.BlastRadius.Field)
		}
		d.clamp(StageGuardrails, GuardrailBlastRadius, v.Message, blastNumbers(v))
	}
	d.Action.PrePatchState = PrunePatchPaths(d.Action.PrePatchState, fields)
	narrowAction(d, PrunePatchPaths(d.Action.Patch, fields))
}

// dropUnmeasured removes, and records, the fields of a step that have no baseline
// to be measured against.
func (s guardrailsStage) dropUnmeasured(d *Decision, step *dorguv1.RemediationStep) {
	unmeasured := unmeasuredFields(step.Patch.Raw, baselineValues(step.Patch.Raw, d))
	if len(unmeasured) == 0 {
		return
	}
	values := PatchLeafValues(step.Patch.Raw)
	for _, field := range unmeasured {
		step.Safety = append(step.Safety, dorguv1.StepSafety{
			Rule:      dorguv1.SafetyRuleBlastRadius,
			Verdict:   dorguv1.SafetyVerdictRejected,
			Field:     field,
			Requested: values[field],
			Message:   "Left out: Dorgu has no live or recorded value for " + field + " to measure this change against.",
		})
	}
	step.Patch = PrunePatchPaths(step.Patch, unmeasured)
	step.PrePatchState = PrunePatchPaths(step.PrePatchState, unmeasured)
	if step.Patch == nil {
		step.AutoExecutable = false
	}
	d.clamp(StageGuardrails, GuardrailNoBaseline,
		fmt.Sprintf("step %d: left out %d field(s) with nothing to measure them against", step.Order, len(unmeasured)),
		map[string]string{"step": fmt.Sprint(step.Order), "fields": fmt.Sprint(len(unmeasured))})
}

// unmeasuredFields returns the resource fields of a patch that have no usable
// baseline: none recorded, unparseable, or zero. Sorted for stable output.
func unmeasuredFields(patch []byte, baseline map[string]string) []string {
	patchValues, err := ParseResourcePatch(patch)
	if err != nil {
		return nil
	}
	var out []string
	for field := range patchValues {
		if !IsResourcePath(field) {
			continue
		}
		base, ok := baseline[field]
		if !ok {
			out = append(out, field)
			continue
		}
		qty, err := resource.ParseQuantity(base)
		if err != nil || qty.Sign() <= 0 {
			out = append(out, field)
		}
	}
	slices.Sort(out)
	return out
}

// hasVerdict reports whether the decision already carries a verdict from the
// guardrail (any verdict kind when kind is empty).
func hasVerdict(d *Decision, guardrail, kind string) bool {
	for _, v := range d.Verdicts {
		if v.Guardrail == guardrail && (kind == "" || v.Verdict == kind) {
			return true
		}
	}
	return false
}

func blastNumbers(v Violation) map[string]string {
	if v.BlastRadius == nil {
		return nil
	}
	return map[string]string{
		"field":     v.BlastRadius.Field,
		"baseline":  v.BlastRadius.Baseline,
		"requested": v.BlastRadius.Requested,
		"ratio":     fmt.Sprintf("%.2f", v.BlastRadius.Ratio),
		"max":       fmt.Sprintf("%.1f", v.BlastRadius.MaxRatio),
	}
}

// blastRadiusViolations reports EVERY field of the patch that breaches the cap
// against its baseline, in a stable field order. A field with no baseline is not
// measured: the change would introduce it, which the absent-field guardrail owns.
func blastRadiusViolations(patch []byte, baseline map[string]string) []Violation {
	patchValues, err := ParseResourcePatch(patch)
	if err != nil {
		return nil
	}

	fields := make([]string, 0, len(patchValues))
	for field := range patchValues {
		fields = append(fields, field)
	}
	slices.Sort(fields)

	var violations []Violation
	for _, field := range fields {
		newVal := patchValues[field]
		baseValue, ok := baseline[field]
		if !ok {
			continue
		}
		oldVal, err := resource.ParseQuantity(baseValue)
		if err != nil || oldVal.IsZero() {
			continue
		}

		newQty := newVal.AsApproximateFloat64()
		oldQty := oldVal.AsApproximateFloat64()
		if oldQty <= 0 {
			continue
		}

		ratio := newQty / oldQty
		detail := &BlastRadiusViolation{
			Field:     field,
			Baseline:  oldVal.String(),
			Requested: newVal.String(),
			Ratio:     ratio,
			MaxRatio:  MaxBlastRadiusMultiplier,
		}

		switch {
		case ratio > MaxBlastRadiusMultiplier:
			violations = append(violations, Violation{
				Rule:        RuleBlastRadius,
				Message:     fmt.Sprintf("resource change for %s exceeds maximum: %.1fx increase (max %.1fx)", field, ratio, MaxBlastRadiusMultiplier),
				BlastRadius: detail,
			})
		case ratio < MaxResourceDecrease:
			violations = append(violations, Violation{
				Rule:        RuleBlastRadius,
				Message:     fmt.Sprintf("resource change for %s exceeds maximum: %.0f%% decrease (max %.0f%%)", field, (1-ratio)*100, MaxResourceDecrease*100),
				BlastRadius: detail,
			})
		case newQty <= 0:
			violations = append(violations, Violation{
				Rule:        RuleBlastRadius,
				Message:     fmt.Sprintf("resource value for %s must be positive, got %s", field, newVal.String()),
				BlastRadius: detail,
			})
		}
	}
	return violations
}

// siblings returns every other RemediationAction whose SPEC targets the same
// persona, in any namespace and whatever its labels say, excluding the candidate
// itself. A read failure is returned: the gate cannot count what it cannot list.
func (g *Gate) siblings(ctx context.Context, cand *Candidate) ([]dorguv1.RemediationAction, error) {
	var list dorguv1.RemediationActionList
	if err := g.client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing RemediationActions: %w", err)
	}

	want := personaKey(cand)
	out := make([]dorguv1.RemediationAction, 0, len(list.Items))
	for i := range list.Items {
		ra := &list.Items[i]
		if cand.Name != "" && ra.Name == cand.Name && ra.Namespace == cand.Namespace {
			continue
		}
		if ra.Spec.PersonaRef.Kind != cand.Target.Kind || ra.Spec.PersonaRef.Name != want.Name {
			continue
		}
		ns := ra.Spec.PersonaRef.Namespace
		if ns == "" {
			ns = ra.Namespace
		}
		if ns != want.Namespace {
			continue
		}
		out = append(out, *ra)
	}
	return out, nil
}

// maxRemediationsPerHour reads the rate limit from the ClusterPersonas. A list
// error is returned, not defaulted: an unreadable limit is unknown, and unknown
// fails closed.
func (g *Gate) maxRemediationsPerHour(ctx context.Context) (int32, error) {
	var clusterPersonas dorguv1.ClusterPersonaList
	if err := g.client.List(ctx, &clusterPersonas); err != nil {
		return 0, fmt.Errorf("listing ClusterPersonas: %w", err)
	}

	for i := range clusterPersonas.Items {
		cp := &clusterPersonas.Items[i]
		if cp.Spec.Policies != nil && cp.Spec.Policies.SelfHealing != nil && cp.Spec.Policies.SelfHealing.MaxRemediationsPerHour > 0 {
			return cp.Spec.Policies.SelfHealing.MaxRemediationsPerHour, nil
		}
	}
	return DefaultMaxRemediationsPerHour, nil
}
