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
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
	"github.com/dorgu-ai/dorgu-operator/internal/workload"
)

// proposeWithPlanner runs the AI planning path: ask the planner for an ordered
// plan, map it to RemediationStep[], put it through the gate, and persist a
// RemediationAction.
//
// The model is the author of the plan's words and of its candidate steps. It is
// never the author of what survives: the gate validates the steps, measures them
// against the live workload and removes what a guardrail refuses.
//
// It returns (nil, err) to signal the caller to fall back to the rule-based
// proposer: used when context can't be built, the planner errors, or the plan is
// empty. A gate refusal produces a (skip-result, nil) instead, matching the
// rule-based path.
func (p *Proposer) proposeWithPlanner(
	ctx context.Context,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
	pre *gate.Decision,
) (*ProposalResult, error) {
	workloadRef := pre.WorkloadRef

	rc, err := planner.BuildContext(ctx, p.client, diag, incident)
	if err != nil {
		return nil, fmt.Errorf("building remediation context: %w", err)
	}

	// Hand the planner the live workload as ground truth. Without it the model
	// reasons from a persona that may be months stale, which is how it came to
	// state a 96Mi limit for a 32Mi container and to invent an image tag while
	// the real prior tag sat in an annotation Dorgu had written itself.
	rc.Workload = planner.NewWorkloadContext(workloadRef, observedDeployment(pre.Observation), rc.AppPersona)

	plan, err := p.planner.PlanRemediation(ctx, *rc)
	if err != nil {
		return nil, fmt.Errorf("planning remediation: %w", err)
	}
	if plan == nil || len(plan.Steps) == 0 {
		return nil, fmt.Errorf("planner returned an empty plan")
	}

	steps := mapPlannedSteps(plan, rc.AppPersona)
	return p.gateAndPersistPlan(ctx, diag, incident, plan, steps, rc, workloadRef, true, nil)
}

// gateAndPersistPlan puts a plan's steps through the gate and persists what it
// allows. advisoryFallback is true on the first pass only: if the gate refuses the
// plan at Validate/ActionClass (a persona-update the rules did not authorise, which
// is what the planner writes for a crash loop, a not-Ready container or a liveness
// kill), the persona-update steps are dropped and the advisory steps that remain
// are proposed through this same path, without asking the model again. A plan with
// nothing advisory in it stays refused, as before. (EKS QA 2026-11-09 F-5.)
func (p *Proposer) gateAndPersistPlan(
	ctx context.Context,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
	plan *planner.RemediationPlan,
	steps []dorguv1.RemediationStep,
	rc *planner.RemediationContext,
	workloadRef *dorguv1.WorkloadRef,
	advisoryFallback bool,
	dropped []refusedValue,
) (*ProposalResult, error) {
	action := p.buildPlanAction(diag, incident, plan, steps, workloadRef)

	// The gate rules on the steps. Blast-radius and absent-field refusals strip the
	// offending field and record the verdict as structured data; persona-wide
	// refusals (rate limit, concurrency, deny-list, cooldown) end the proposal.
	dec, err := p.gate.Run(ctx, candidateFor(action, diag, incident, false), gate.PhaseProposal)
	if err != nil {
		return nil, fmt.Errorf("gate: %w", err)
	}
	if !dec.Allowed() {
		refusal := skipped(dec)
		if advisoryFallback && refusal.Stage == gate.StageValidate && refusal.Reason == gate.GuardrailActionClass {
			if advisory := advisorySteps(steps); len(advisory) > 0 {
				p.logger.Info("the gate refused the AI plan's persona change; proposing its advisory steps instead",
					"incident", incident.Name, "advisorySteps", len(advisory))
				result, err := p.gateAndPersistPlan(ctx, diag, incident, plan, advisory, rc, workloadRef, false,
					droppedPersonaValues(steps))
				switch {
				case err != nil && !errors.Is(err, errPersistFailed):
					// The advisory pass failed. The first refusal is still the answer, so it is
					// recorded on the incident like any refusal (review L-6); the failure is a log line.
					p.logger.Info("could not propose the AI plan's advisory steps; the refusal of its persona change stands",
						"incident", incident.Name, "error", err.Error())
					return refusal, nil
				case err == nil && result != nil && result.Proposed:
					result.PlanRefusal = refusal
				}
				return result, err
			}
		}
		return refusal, nil
	}
	installDecision(action, dec)

	// CR-01: a plan that diagnoses a resource change and then applies nothing is
	// not a fix, however well it reads. Give the plan the deterministic patch the
	// rule engine would have produced, or refuse the plan and let the caller fall
	// back to the rule-based proposal.
	ruleEngineDeclined, err := p.ensureAppliableResourcePlan(action, diag, rc.AppPersona, workloadRef)
	if err != nil {
		return nil, err
	}

	// A persona-update step with no patch applies nothing and instructs nobody.
	// Whatever could be filled in has been by now, so anything still empty is
	// noise that reads as a fix.
	if dropped := dropPatchlessPersonaUpdates(action); dropped > 0 {
		p.logger.V(1).Info("dropped persona-update steps that carried no patch",
			"action", action.Name, "dropped", dropped)
		if len(action.Spec.Steps) == 0 {
			return nil, fmt.Errorf("AI plan had nothing left after removing persona-update steps with no patch")
		}
	}

	// Recompute the back-compat Action from the first still-auto-executable
	// persona-update step (the gate may have flagged the original choice).
	setBackCompatAction(action)

	// Shape the advisory half of the plan for whoever owns the Deployment. A
	// persona-update step is untouched: the operator patches the persona, which is
	// safe whoever owns the workload. The gate's final run below refuses a plan
	// that still carries a command writing to an owned workload.
	//
	// This runs AFTER the gate and after the back-compat Action is recomputed, not
	// before, because the owner instruction quotes the concrete values out of the
	// plan's patches. Shaping first told a Helm user to put 128Mi in their values
	// file while the guardrail was in the middle of refusing 128Mi, which is CR-04's
	// "the headline still advertised the rejected value" one screen further down.
	applyOwnershipShaping(action, workloadRef)

	// A workload that Helm, Argo CD, Flux or kustomize owns gets advice only, the
	// persona-update step included (B10 b). The gate has already measured that step
	// against the live workload, so the values it quotes are the permitted ones.
	adviseForOwner(action, workloadRef)

	// The explanation's auto/advisory split is only true once the gate has run and
	// the plan has been shaped.
	//
	// This must stay ABOVE discloseBlastRadiusClamp: that appends the cap caveat
	// to Explanation, and recomputing afterwards would erase it.
	action.Spec.Explanation = planExplanation(action.Spec.Steps)

	// The advisory fallback dropped a persona change the rules did not authorise. The
	// model's prose for the steps that stayed can still describe it ("with memory
	// raised to 384Mi, ..."): take those sentences out and say what happened.
	if len(dropped) > 0 {
		if scrubbed := scrubDroppedChange(action, dropped); scrubbed > 0 {
			p.logger.Info("removed text about the dropped persona change from the advisory plan",
				"action", action.Name, "surfaces", scrubbed)
		}
	}

	// The planner is told to keep changes within ~2x the live limit, so a plan
	// can arrive already pressed against the cap. Damp its confidence rather
	// than presenting a truncated fix as a confident one. Measured against the
	// live workload.
	//
	// The prose half of this disclosure only survives the CR5-03 scrub below when
	// a guardrail actually recorded a verdict, so the log says what is always
	// true rather than what used to be claimed.
	if discloseGroundedBlastRadiusClamp(action, workloadRef) {
		p.logger.Info("plan sits at the blast-radius cap; damping confidence",
			"action", action.Name, "confidence", action.Spec.Confidence)
	}

	// CR5-01: the single chokepoint. Everything above has now had its say, so
	// this is the last shape the plan takes before it is written, and no value a
	// guardrail refused may survive into any surface a client renders. It is
	// here rather than beside each producer because the same defect has already
	// been found in two producers.
	if scrubbed := scrubRefusedValues(action); scrubbed > 0 {
		metrics.RecordModelClaimsStripped(metrics.ClaimRefusedValue, scrubbed)
		p.logger.Info("removed guardrail-refused values from the plan before persisting it",
			"action", action.Name, "surfaces", scrubbed)
	}

	// CR5-03: and no surface may assert a guardrail ruled when none did. This is
	// the enforcement point for prompt rule G7, which asks the model not to
	// comment on caps and cannot make it so, and for Dorgu's own at-the-cap
	// disclosure, which called sitting at a ceiling being clamped by one.
	if scrubbed := scrubFabricatedGuardrailClaims(action); scrubbed > 0 {
		metrics.RecordModelClaimsStripped(metrics.ClaimFabricatedClamp, scrubbed)
		p.logger.Info("removed guardrail claims no recorded verdict supports",
			"action", action.Name, "surfaces", scrubbed)
	}

	// The seal: the gate rules once more on the object that is about to be
	// written, as final. Nothing reaches the API server that the gate would clamp
	// or refuse, whichever code above produced it: a step the proposer derived, a
	// command that survived shaping, a plan that applies nothing for a diagnosed
	// resource change (CR-01).
	sealed, refusal := p.seal(ctx, action, diag, incident, ruleEngineDeclined)
	if refusal != nil {
		return refusal, nil
	}

	// status.gate is the seal's decision: the first run ruled on a draft of this
	// plan, and the seal on the object about to be written (R7).
	return p.persist(ctx, action, sealed)
}

// advisorySteps returns the plan's steps without its persona-updates, renumbered
// from 1: what is left to propose when the rules did not authorise a persona change.
func advisorySteps(steps []dorguv1.RemediationStep) []dorguv1.RemediationStep {
	out := make([]dorguv1.RemediationStep, 0, len(steps))
	for i := range steps {
		if steps[i].Type == dorguv1.StepTypePersonaUpdate {
			continue
		}
		step := steps[i].DeepCopy()
		step.Order = int32(len(out) + 1)
		step.ID = fmt.Sprintf("step-%d", step.Order)
		out = append(out, *step)
	}
	return out
}

// seal runs the gate on the final shape of a plan. It returns the sealing decision,
// or a refusal when that would be anything but a clean pass. The plan was already clamped by the first
// run, so a Clamp here means something after the gate changed it.
func (p *Proposer) seal(
	ctx context.Context,
	action *dorguv1.RemediationAction,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
	ruleEngineDeclined bool,
) (*gate.Decision, *ProposalResult) {
	cand := candidateFor(action, diag, incident, true)
	cand.Facts.AdvisoryAllowed = ruleEngineDeclined

	final, err := p.gate.Run(ctx, cand, gate.PhaseProposal)
	if err != nil {
		p.logger.Error(err, "the gate could not re-verify the final plan; not proposing it", "action", action.Name)
		return nil, proposerSkip("the gate could not re-verify the final plan", SkipGateUnavailable, nil)
	}
	switch {
	case !final.Allowed():
		return nil, skipped(final)
	case final.Result != gate.ResultPass:
		p.logger.Info("the final plan was changed after the gate ruled on it; not proposing it",
			"action", action.Name, "result", final.Result)
		return nil, proposerSkip("the final plan differed from what the gate cleared, so it was not proposed",
			SkipPlanChanged, final)
	}
	return final, nil
}

// mapPlannedSteps converts the planner's ordered steps into RemediationStep[].
// Only persona-update steps are auto-executable and carry a patch (plus a
// pre-patch snapshot of the current persona spec, the rollback target).
//
// Advisory steps may additionally carry a suggested kubectl command, which is
// sanitized here: the planner's output is model-authored, and this is the last
// point before it is persisted for the CLI to print to a human.
//
// Nothing here decides what is safe. The patch is exactly what the model asked
// for; the gate removes what a guardrail refuses (a resource key the workload
// does not set, a change past the cap, a field outside the allowlist).
func mapPlannedSteps(
	plan *planner.RemediationPlan,
	persona *dorguv1.ApplicationPersona,
) []dorguv1.RemediationStep {
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
			if pre := gate.SnapshotPrePatch(persona, ps.Patch); pre != nil {
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

// observedDeployment unwraps the live Deployment from an observation, tolerating
// the nil observation an unresolvable workload produces.
func observedDeployment(obs *workload.Observation) *appsv1.Deployment {
	if obs == nil {
		return nil
	}
	return obs.Deployment
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
	workloadRef *dorguv1.WorkloadRef,
) *dorguv1.RemediationAction {
	namespace := diag.PersonaRef.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	action := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateActionName(incident, "ai-remediation"),
			Namespace: namespace,
			Labels: map[string]string{
				"dorgu.io/persona-kind":      diag.PersonaRef.Kind,
				"dorgu.io/persona-name":      diag.PersonaRef.Name,
				"dorgu.io/persona-namespace": namespace,
			},
			Annotations: answersSignal(diag),
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{
				Name:      incident.Name,
				Namespace: incident.Namespace,
			},
			PersonaRef:  *diag.PersonaRef,
			TrustLevel:  2,
			Steps:       steps,
			WorkloadRef: workloadRef,
			PlanSource:  dorguv1.PlanSourceAIAnthropic,
			PlanSummary: plan.RootCause,
			Explanation: planExplanation(steps),
			Confidence:  formatConfidence(plan.Confidence),
			Approval: &dorguv1.ApprovalSpec{
				Required: true,
			},
		},
	}

	setBackCompatAction(action)
	return action
}

// setBackCompatAction derives the deprecated single Action from the plan:
// steps[] is the only plan, and the Action is its merged persona patch (or an
// advisory notification when nothing auto-executable remains), kept for one
// release for clients that still read it. The executor never applies it.
func setBackCompatAction(action *dorguv1.RemediationAction) {
	action.Spec.Action = gate.DeriveAction(action.Spec.Steps)
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
