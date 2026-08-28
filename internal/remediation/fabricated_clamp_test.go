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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// atCapWorld is clean-room run #5's demo OOM app: a live container with a 64Mi
// limit and a persona that agrees with it, so a plan asking for exactly 128Mi
// sits ON the 2x cap without any guardrail refusing anything.
func atCapWorld(t *testing.T) (*Proposer, *dorguv1.IncidentMemory) {
	t.Helper()

	persona := personaWithLimits(groundedPersona, "500m", "64Mi")
	persona.Spec.Resources.Requests = &dorguv1.ResourceValues{CPU: "100m", Memory: "64Mi"}
	deploy := liveDeployment(groundedPersona,
		map[corev1.ResourceName]string{corev1.ResourceMemory: "64Mi", corev1.ResourceCPU: "500m"},
		map[corev1.ResourceName]string{corev1.ResourceMemory: "64Mi", corev1.ResourceCPU: "100m"})
	incident := newTestIncident(defaultNamespace, "memhog", groundedPersona, "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	return NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger()), incident
}

// atCapPlan asks for exactly the doubling the guardrail permits, and says out
// loud that it chose to double. Nothing here is refused.
func atCapPlan() *planner.RemediationPlan {
	return &planner.RemediationPlan{
		RootCause:  "the 64Mi memory limit is too low for the workload's actual memory consumption",
		Confidence: 0.86,
		Steps: []planner.PlannedStep{
			{
				Order:       1,
				Type:        "persona-update",
				Description: "Raise the memory limit to 128Mi and the request to 128Mi",
				Rationale:   "Doubling both the limit and the request keeps them proportionate.",
				Risk:        "low",
				Patch: json.RawMessage(
					`{"spec":{"resources":{"limits":{"memory":"128Mi"},"requests":{"memory":"128Mi"}}}}`),
			},
			{
				Order:       2,
				Type:        "restart",
				Description: "Restart the Deployment so the new limit takes effect",
				Rationale:   "the pod picks the limit up on its next start",
				Risk:        "low",
			},
		},
	}
}

// TestProposer_CR503_NoClampIsAnnouncedWhenNothingWasClamped is clean-room run
// #5's CR5-03.
//
// The model asked for exactly 2x, no guardrail refused anything,
// `spec.steps[].safety` was null on every step, no GUARDRAIL column appeared —
// and the string "Clamped by the 2x blast-radius guardrail: … could not be
// raised further in one step" appeared three times anyway, in the explanation,
// the plan summary and a step rationale. In the same paragraph the model says
// it deliberately chose to double. The reader has no way to tell which of the
// two contradicting statements is computed, which is the exact trust failure
// the structured `safety` field was built to end.
func TestProposer_CR503_NoClampIsAnnouncedWhenNothingWasClamped(t *testing.T) {
	p, incident := atCapWorld(t)
	p.planner = &stubPlanner{plan: atCapPlan()}

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	action := result.Action

	// The premise: this plan sits at the cap and nothing was refused.
	for i := range action.Spec.Steps {
		require.Empty(t, action.Spec.Steps[i].Safety,
			"step %d: no guardrail ruled on this plan, so it carries no verdict", i+1)
	}
	assert.Equal(t, "128Mi", patchLimit(t, action.Spec.Action.Patch.Raw, "memory"),
		"the plan applies exactly what the model asked for")

	assertNoGuardrailClaim(t, action)

	// The disclosure that IS earned survives: a plan pressed against the cap is
	// still reported as less certain than one that chose its own number.
	assert.Equal(t, "0.73", action.Spec.Confidence,
		"confidence is still damped; only the false verdict goes")
}

// TestProposer_CR503_IncidentResolutionCannotInheritTheClaim pins the surface
// the claim outlived the incident on. IncidentMemory.spec.resolution.action is
// copied verbatim from spec.explanation, so a fabricated verdict there is
// written permanently into the organizational-memory artifact.
func TestProposer_CR503_IncidentResolutionCannotInheritTheClaim(t *testing.T) {
	p, incident := atCapWorld(t)
	p.planner = &stubPlanner{plan: atCapPlan()}

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)

	// This is exactly the assignment updateIncidentResolution makes.
	resolutionAction := result.Action.Spec.Explanation
	assert.NotContains(t, strings.ToLower(resolutionAction), "clamped")
	assert.NotContains(t, strings.ToLower(resolutionAction), "guardrail")
}

// TestProposer_CR503_ARealClampStillSaysSo is the other half of the rule. The
// scrub keys on the structured verdict, so where a guardrail did refuse a
// value the disclosure is earned and stays.
func TestProposer_CR503_ARealClampStillSaysSo(t *testing.T) {
	p, incident := driftWorld(t)
	p.planner = &stubPlanner{plan: cleanRoomDriftPlan()}

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)

	action := result.Action
	require.NotEmpty(t, allStepSafety(action), "this plan really was clamped")
	assert.Contains(t, strings.ToLower(action.Spec.PlanSummary), "clamp",
		"a clamp that happened is still disclosed")
}

// assertNoGuardrailClaim fails when any model-authored surface of a plan with
// no recorded verdict asserts that a guardrail did something.
func assertNoGuardrailClaim(t *testing.T, action *dorguv1.RemediationAction) {
	t.Helper()

	surfaces := map[string]string{
		"planSummary": action.Spec.PlanSummary,
		"explanation": action.Spec.Explanation,
	}
	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		surfaces["steps["+step.ID+"].description"] = step.Description
		surfaces["steps["+step.ID+"].rationale"] = step.Rationale
	}

	for name, text := range surfaces {
		assert.False(t, assertsGuardrailVerdict(text),
			"%s claims a guardrail ruled, but spec.steps[].safety is empty — %s", name, text)
	}
}

// TestScrubFabricatedGuardrailClaims covers the scrub directly, including the
// model prose the prompt's G7 already forbids and the model produces anyway.
func TestScrubFabricatedGuardrailClaims(t *testing.T) {
	verdict := dorguv1.StepSafety{
		Rule:      dorguv1.SafetyRuleBlastRadius,
		Verdict:   dorguv1.SafetyVerdictClamped,
		Field:     "spec.resources.limits.memory",
		Requested: "512Mi",
		Permitted: "16Mi",
	}

	tests := []struct {
		name   string
		build  func() *dorguv1.RemediationAction
		assert func(t *testing.T, action *dorguv1.RemediationAction)
	}{
		{
			name: "Dorgu's own at-cap caveat, with no verdict behind it",
			build: func() *dorguv1.RemediationAction {
				a := planWith(dorguv1.RemediationStep{Order: 1, Type: dorguv1.StepTypePersonaUpdate})
				caveat := blastRadiusCaveat([]string{"spec.resources.limits.memory"})
				a.Spec.PlanSummary = appendNote("the limit is too low for the working set", caveat)
				a.Spec.Explanation = appendNote("AI remediation plan: 1 step, applied on approval", caveat)
				a.Spec.Steps[0].Rationale = appendNote("doubling keeps them proportionate", caveat)
				return a
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Equal(t, "the limit is too low for the working set.", a.Spec.PlanSummary)
				assert.Equal(t, "AI remediation plan: 1 step, applied on approval.", a.Spec.Explanation)
				assert.Equal(t, "doubling keeps them proportionate.", a.Spec.Steps[0].Rationale)
			},
		},
		{
			name: "the model's own cap commentary, which G7 forbids and does not prevent",
			build: func() *dorguv1.RemediationAction {
				a := planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypePersonaUpdate,
					Description: "Raise the limit to 128Mi. This is well within the 2x ceiling.",
					Rationale:   "The blast-radius guardrail permits this comfortably.",
				})
				return a
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Equal(t, "Raise the limit to 128Mi.", a.Spec.Steps[0].Description)
				assert.Empty(t, a.Spec.Steps[0].Rationale)
			},
		},
		{
			name: "a step with a verdict keeps its guardrail prose",
			build: func() *dorguv1.RemediationAction {
				a := planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypePersonaUpdate,
					Rationale: "Clamped by the 2x blast-radius guardrail: it could not be raised further.",
					Safety:    []dorguv1.StepSafety{verdict},
				})
				a.Spec.PlanSummary = "Clamped by the 2x blast-radius guardrail: it could not be raised further."
				return a
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Contains(t, a.Spec.Steps[0].Rationale, "Clamped")
				assert.Contains(t, a.Spec.PlanSummary, "Clamped")
			},
		},
		{
			name: "a neighbouring step may refer to the verdict another step earned",
			build: func() *dorguv1.RemediationAction {
				return planWith(
					dorguv1.RemediationStep{Order: 1, Type: dorguv1.StepTypePersonaUpdate,
						Safety: []dorguv1.StepSafety{verdict}},
					dorguv1.RemediationStep{Order: 2, Type: dorguv1.StepTypeWorkloadApply,
						Description: droppedCommandNote()},
				)
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Equal(t, droppedCommandNote(), a.Spec.Steps[1].Description,
					"the note explaining a dropped command points at a verdict that exists")
			},
		},
		{
			name: "a word that merely starts like one of the terms is not a claim",
			build: func() *dorguv1.RemediationAction {
				return planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypeManual,
					Rationale: "The node has spare capacity, so rescheduling is an option.",
				})
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Contains(t, a.Spec.Steps[0].Rationale, "spare capacity")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := tt.build()
			scrubFabricatedGuardrailClaims(action)
			tt.assert(t, action)
		})
	}
}
