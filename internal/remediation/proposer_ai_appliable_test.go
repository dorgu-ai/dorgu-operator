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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// mustJSON wraps a literal patch for a CRD field in these tests.
func mustJSON(raw string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(raw)}
}

// cleanRoomOOMPlan is the plan the AI planner actually produced in clean-room
// run #4 for the docs' own demo-oom.yaml, reproduced field for field from the
// CRD the tester dumped: two workload-apply steps describing the memory fix, a
// persona-update step with no patch, and therefore nothing appliable anywhere.
//
//	step 1 type=workload-apply  auto=False patch={}
//	step 2 type=workload-apply  auto=False patch={}
//	step 3 type=persona-update  auto=False patch={}
func cleanRoomOOMPlan() *planner.RemediationPlan {
	return &planner.RemediationPlan{
		RootCause:  "the container's 64Mi memory limit is below its ~90MB working set, so the kernel OOM-kills it on every start",
		Confidence: 0.86,
		Steps: []planner.PlannedStep{
			{
				Order:       1,
				Type:        "workload-apply",
				Description: "Increase the memory limit from 64Mi to 128Mi on the Deployment",
				Rationale:   "the working set is about 90MB, so 64Mi cannot hold it",
				Risk:        "low",
			},
			{
				Order:       2,
				Type:        "workload-apply",
				Description: "Increase the memory request from 32Mi to 64Mi on the Deployment",
				Rationale:   "keep the request proportionate to the new limit",
				Risk:        "low",
			},
			{
				Order:       3,
				Type:        "persona-update",
				Description: "Update the ApplicationPersona to reflect the corrected memory limits",
				Rationale:   "keep the recorded intent in step with the workload",
				Risk:        "low",
			},
		},
	}
}

// memhogWorld builds the clean-room scenario: a persona and a live Deployment
// that both record a 64Mi limit and a 32Mi request, matching demo-oom.yaml.
func memhogWorld(t *testing.T) (*Proposer, *dorguv1.IncidentMemory) {
	t.Helper()

	persona := personaWithLimits(groundedPersona, "200m", "64Mi")
	deploy := liveDeployment(groundedPersona,
		map[corev1.ResourceName]string{corev1.ResourceMemory: "64Mi", corev1.ResourceCPU: "200m"},
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi", corev1.ResourceCPU: "50m"})
	incident := newTestIncident(defaultNamespace, "oom", groundedPersona, "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	return NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger()), incident
}

// TestProposer_AIPath_CR01_UnappliablePlanIsMadeAppliable is the clean-room
// blocker. Following the quickstart verbatim with AI enabled produced a
// notification-type action whose every step was advisory, `approve` printed "No
// resource change to apply", and the pod was still crash-looping 42 minutes
// later. The same app healed on the first try with the planner turned off.
func TestProposer_AIPath_CR01_UnappliablePlanIsMadeAppliable(t *testing.T) {
	p, incident := memhogWorld(t)
	p.planner = &stubPlanner{plan: cleanRoomOOMPlan()}

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	a := result.Action
	assert.Equal(t, dorguv1.PlanSourceAIAnthropic, a.Spec.PlanSource, "the AI plan is kept, not discarded")
	require.True(t, a.HasAutoApplicableChange(),
		"an AI plan that diagnoses a resource change must be appliable")

	// The persona-update step is the one that carries the patch, because it is
	// the only kind the CRD lets the operator apply.
	var applied *dorguv1.RemediationStep
	for i := range a.Spec.Steps {
		if a.Spec.Steps[i].AutoExecutable {
			require.Nil(t, applied, "exactly one step applies")
			applied = &a.Spec.Steps[i]
		}
	}
	require.NotNil(t, applied, "no step in the plan could be applied")
	assert.Equal(t, dorguv1.StepTypePersonaUpdate, applied.Type)
	require.NotNil(t, applied.Patch)

	// 64Mi live, critical severity: the rule engine's own 2x calculation.
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`, string(applied.Patch.Raw))
	require.NotNil(t, applied.PrePatchState)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"64Mi"}}}}`, string(applied.PrePatchState.Raw))

	// Dorgu supplied the value, and says so as structured data rather than prose.
	require.Len(t, applied.Safety, 1)
	assert.Equal(t, dorguv1.SafetyRulePlanValidation, applied.Safety[0].Rule)
	assert.Equal(t, dorguv1.SafetyVerdictDerived, applied.Safety[0].Verdict)
	assert.Equal(t, "spec.resources.limits.memory", applied.Safety[0].Field)
	assert.Equal(t, "128Mi", applied.Safety[0].Permitted)

	// The back-compat Action the executor reads is a persona-update, not a
	// notification, so `dorgu remediation approve` has something to apply.
	assert.Equal(t, dorguv1.ActionTypePersonaUpdate, a.Spec.Action.Type)
	require.NotNil(t, a.Spec.Action.Patch)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`, string(a.Spec.Action.Patch.Raw))

	require.NoError(t, a.ValidateAutoExecutable())
}

// TestProposer_AIPath_CR01_InvariantHoldsAcrossPlanShapes is the guard the fix
// exists for: whatever a model returns, no RemediationAction is created whose
// steps are all non-executable when a resource change was diagnosed.
func TestProposer_AIPath_CR01_InvariantHoldsAcrossPlanShapes(t *testing.T) {
	resourcePatch := func(v string) json.RawMessage {
		return json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"` + v + `"}}}}`)
	}

	tests := []struct {
		name string
		plan *planner.RemediationPlan
	}{
		{
			name: "the clean-room shape: workload-apply steps and a patchless persona-update",
			plan: cleanRoomOOMPlan(),
		},
		{
			name: "no persona-update step at all",
			plan: &planner.RemediationPlan{
				RootCause: "oom", Confidence: 0.7,
				Steps: []planner.PlannedStep{
					{Order: 1, Type: "workload-apply", Description: "raise the limit", Rationale: "r", Risk: "low"},
					{Order: 2, Type: "manual", Description: "watch it", Rationale: "r", Risk: "low"},
				},
			},
		},
		{
			name: "persona-update carrying prose where a patch should be",
			plan: &planner.RemediationPlan{
				RootCause: "oom", Confidence: 0.7,
				Steps: []planner.PlannedStep{
					{Order: 1, Type: "persona-update", Description: "raise the limit", Rationale: "r", Risk: "low",
						Patch: json.RawMessage(`not json`)},
				},
			},
		},
		{
			name: "resource patch parked on a workload-apply step, where it can never be applied",
			plan: &planner.RemediationPlan{
				RootCause: "oom", Confidence: 0.7,
				Steps: []planner.PlannedStep{
					{Order: 1, Type: "workload-apply", Description: "raise the limit", Rationale: "r", Risk: "low",
						Patch: resourcePatch("128Mi")},
				},
			},
		},
		{
			name: "every step refused by the blast-radius guardrail",
			plan: &planner.RemediationPlan{
				RootCause: "oom", Confidence: 0.7,
				Steps: []planner.PlannedStep{
					{Order: 1, Type: "persona-update", Description: "raise the limit a lot", Rationale: "r", Risk: "high",
						Patch: resourcePatch("4Gi")},
				},
			},
		},
		{
			name: "an all-advisory plan that never mentions the resource change",
			plan: &planner.RemediationPlan{
				RootCause: "oom", Confidence: 0.7,
				Steps: []planner.PlannedStep{
					{Order: 1, Type: "manual", Description: "page on-call", Rationale: "r", Risk: "high"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, incident := memhogWorld(t)
			p.planner = &stubPlanner{plan: tt.plan}

			diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
			result, err := p.Propose(context.Background(), diag, incident)
			require.NoError(t, err)
			require.True(t, result.Proposed)

			require.NoError(t, assertAppliableWhenResourceDiagnosed(result.Action, diag, false),
				"a resource change was diagnosed, so something in the plan has to be able to change a resource")
			require.True(t, result.Action.HasAutoApplicableChange())
			require.NoError(t, result.Action.ValidateAutoExecutable())
		})
	}
}

// TestProposer_AIPath_CR01_NoPhantomPersonaUpdateStep covers the other half of
// the requirement: nothing is persisted that presents itself as a fix without
// being one. Here the live container sets no memory limit, so raising one would
// introduce a field the workload has never had (F-05) and both the plan and the
// rule engine decline. The plan survives as honest advice, but the empty
// persona-update step that rendered as "(no changes)" does not.
func TestProposer_AIPath_CR01_NoPhantomPersonaUpdateStep(t *testing.T) {
	persona := personaWithLimits(groundedPersona, "", "")
	deploy := liveDeployment(groundedPersona, nil,
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"})
	incident := newTestIncident(defaultNamespace, "oom", groundedPersona, "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	p := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger(),
		WithPlanner(&stubPlanner{plan: cleanRoomOOMPlan()}))

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed, "the model's advice is still worth recording")

	a := result.Action
	for _, step := range a.Spec.Steps {
		assert.NotEqual(t, dorguv1.StepTypePersonaUpdate, step.Type,
			"a persona-update step with no patch applies nothing and instructs nobody")
	}
	require.Len(t, a.Spec.Steps, 2, "the two advisory workload steps survive; the empty one does not")
	assert.Equal(t, int32(1), a.Spec.Steps[0].Order, "the surviving steps are renumbered from 1")
	assert.Equal(t, "step-2", a.Spec.Steps[1].ID)

	assert.False(t, a.HasAutoApplicableChange())
	assert.Equal(t, dorguv1.ActionTypeNotification, a.Spec.Action.Type)
	assert.Contains(t, a.Spec.Explanation, "all advisory (nothing is applied for you)",
		"the plan says plainly that it will not fix this")
}

// TestProposer_AIPath_CR04_ClampIsStructuredNotProse is the blast-radius report.
// The guardrail worked; how it reported itself did not. The model asserted that
// a 16x change was "well within a 2x ceiling", the real verdict was spliced onto
// the front of that same rationale string, the step rendered "(no changes)", and
// the headline diff still advertised the refused 128Mi.
func TestProposer_AIPath_CR04_ClampIsStructuredNotProse(t *testing.T) {
	// The Helm-owned podinfo case: the persona still records the intended 128Mi
	// while the live workload has drifted down to 8Mi.
	persona := personaWithLimits(groundedPersona, "", "128Mi")
	deploy := liveDeployment(groundedPersona,
		map[corev1.ResourceName]string{corev1.ResourceMemory: "8Mi"},
		map[corev1.ResourceName]string{corev1.ResourceMemory: "8Mi"})
	incident := newTestIncident(defaultNamespace, "oom", groundedPersona, "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	plan := &planner.RemediationPlan{
		RootCause:  "the memory limit drifted down to 8Mi and the container cannot start",
		Confidence: 0.8,
		Steps: []planner.PlannedStep{{
			Order: 1,
			Type:  "persona-update",
			Description: "Update the ApplicationPersona to reflect corrected memory limits and requests, " +
				"raising the limit from 8Mi to 128Mi and request from 8Mi to 64Mi, aligned with the persona's " +
				"original intent and well within a 2x ceiling relative to real-world viability.",
			Rationale: "the persona's recorded intent is 128Mi",
			Risk:      "low",
			Patch:     json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"128Mi"},"requests":{"memory":"64Mi"}}}}`),
		}},
	}

	p := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger(), WithPlanner(&stubPlanner{plan: plan}))

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	step := result.Action.Spec.Steps[0]

	// The verdict is structured data the CLI can render, filter and alert on.
	byField := make(map[string]dorguv1.StepSafety, len(step.Safety))
	for _, s := range step.Safety {
		byField[s.Field] = s
	}
	require.Len(t, byField, 2, "both refused fields are reported, not whichever the map yielded first")

	limits := byField["spec.resources.limits.memory"]
	assert.Equal(t, dorguv1.SafetyRuleBlastRadius, limits.Rule)
	assert.Equal(t, dorguv1.SafetyVerdictClamped, limits.Verdict)
	assert.Equal(t, "8Mi", limits.Baseline, "the cap is measured against the live value, not the persona's")
	assert.Equal(t, "128Mi", limits.Requested)
	assert.Equal(t, "16Mi", limits.Permitted)
	assert.Equal(t, "16.0x", limits.Ratio)
	assert.Equal(t, "2.0x", limits.MaxRatio)

	requests := byField["spec.resources.requests.memory"]
	assert.Equal(t, dorguv1.SafetyVerdictRejected, requests.Verdict)
	assert.Equal(t, "64Mi", requests.Requested)
	assert.Empty(t, requests.Permitted, "nothing replaces a refused field")

	// The patch is what will actually happen, so no diff can advertise 128Mi.
	require.NotNil(t, step.Patch)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"16Mi"}}}}`, string(step.Patch.Raw))
	assert.NotContains(t, string(step.Patch.Raw), "128Mi")
	assert.NotContains(t, string(step.Patch.Raw), "requests")

	// The verdict is nowhere in the model's prose, and the model's claim about
	// the guardrail is gone with the description Dorgu rewrote.
	assert.NotContains(t, step.Rationale, "[safety:blast-radius]",
		"the guardrail's verdict is no longer smuggled into the rationale")
	for _, text := range []string{step.Description, step.Rationale} {
		assert.NotContains(t, strings.ToLower(text), "well within a 2x ceiling",
			"the model never characterises the guardrail's verdict")
	}
	assert.Contains(t, step.Description, "Blast-radius guardrail",
		"Dorgu states the verdict in the step's own words")
	assert.Contains(t, step.Description, "16Mi")

	// The plan still says out loud that the permitted value may not be enough.
	assert.Contains(t, result.Action.Spec.Explanation, "Clamped by the 2x blast-radius guardrail")
}

// TestProposer_AIPath_WithinCapPlanIsLeftAlone pins the negative: when the model
// returns a patch the guardrails accept, Dorgu does not touch its number, its
// description or its rationale.
func TestProposer_AIPath_WithinCapPlanIsLeftAlone(t *testing.T) {
	p, incident := memhogWorld(t)
	p.planner = &stubPlanner{plan: &planner.RemediationPlan{
		RootCause:  "oom",
		Confidence: 0.9,
		Steps: []planner.PlannedStep{{
			Order:       1,
			Type:        "persona-update",
			Description: "Raise the memory limit from 64Mi to 96Mi",
			Rationale:   "the working set peaks near 90MB",
			Risk:        "low",
			Patch:       json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"96Mi"}}}}`),
		}},
	}}

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	step := result.Action.Spec.Steps[0]
	assert.True(t, step.AutoExecutable)
	assert.Empty(t, step.Safety, "no guardrail ruled, so there is no verdict to record")
	assert.Equal(t, "Raise the memory limit from 64Mi to 96Mi", step.Description)
	assert.Equal(t, "the working set peaks near 90MB", step.Rationale)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"96Mi"}}}}`, string(step.Patch.Raw))
}

// TestAssertAppliableWhenResourceDiagnosed covers the invariant function on its
// own, including the case it must stay quiet for: a diagnosis whose fix is not a
// resource change is allowed to be entirely advisory.
func TestAssertAppliableWhenResourceDiagnosed(t *testing.T) {
	advisory := &dorguv1.RemediationAction{
		Spec: dorguv1.RemediationActionSpec{
			Steps: []dorguv1.RemediationStep{{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual}},
		},
	}
	appliable := &dorguv1.RemediationAction{
		Spec: dorguv1.RemediationActionSpec{
			Steps: []dorguv1.RemediationStep{{
				Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
				Patch: mustJSON(`{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`),
			}},
		},
	}

	oom := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	nonResource := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	nonResource.Contributing[0].Signal.Type = detection.SignalImagePullBackOff

	assert.Error(t, assertAppliableWhenResourceDiagnosed(advisory, oom, false))
	assert.NoError(t, assertAppliableWhenResourceDiagnosed(appliable, oom, false))
	assert.NoError(t, assertAppliableWhenResourceDiagnosed(advisory, nonResource, false),
		"a non-resource diagnosis may be answered with advice alone")
	assert.NoError(t, assertAppliableWhenResourceDiagnosed(advisory, oom, true),
		"advisory is the considered outcome when the rule engine declines too")
}

// TestDropPatchlessPersonaUpdates separates the two empty steps: one Dorgu has
// an explanation for and one that is only noise.
func TestDropPatchlessPersonaUpdates(t *testing.T) {
	action := &dorguv1.RemediationAction{Spec: dorguv1.RemediationActionSpec{
		Steps: []dorguv1.RemediationStep{
			{Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate,
				Description: "update the persona"},
			{Order: 2, ID: "step-2", Type: dorguv1.StepTypeWorkloadApply,
				Description: "raise the limit by hand"},
			{Order: 3, ID: "step-3", Type: dorguv1.StepTypePersonaUpdate,
				Description: "raise the cpu limit",
				Safety: []dorguv1.StepSafety{{
					Rule: dorguv1.SafetyRuleAbsentField, Verdict: dorguv1.SafetyVerdictRejected,
					Field: "spec.resources.limits.cpu", Message: "Left out: ...",
				}}},
			{Order: 4, ID: "step-4", Type: dorguv1.StepTypePersonaUpdate,
				Description: "raise the memory limit",
				Patch:       mustJSON(`{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`)},
		},
	}}

	assert.Equal(t, 1, dropPatchlessPersonaUpdates(action))
	require.Len(t, action.Spec.Steps, 3)
	assert.Equal(t, "raise the limit by hand", action.Spec.Steps[0].Description)
	assert.Equal(t, "raise the cpu limit", action.Spec.Steps[1].Description,
		"a step that explains why it is empty is kept")

	for i, step := range action.Spec.Steps {
		assert.Equal(t, int32(i+1), step.Order, "orders stay contiguous from 1")
		assert.Equal(t, "step-"+string(rune('1'+i)), step.ID)
	}

	assert.Equal(t, 0, dropPatchlessPersonaUpdates(action), "a second pass changes nothing")
}

// TestPrunePatchPaths covers removing a refused field from a patch, including
// the case where nothing survives.
func TestPrunePatchPaths(t *testing.T) {
	both := mustJSON(`{"spec":{"resources":{"limits":{"memory":"128Mi"},"requests":{"memory":"64Mi"}}}}`)

	kept := prunePatchPaths(both, []string{"spec.resources.requests.memory"})
	require.NotNil(t, kept)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`, string(kept.Raw))

	assert.Nil(t, prunePatchPaths(both, []string{
		"spec.resources.limits.memory", "spec.resources.requests.memory",
	}), "an emptied patch is nil, not an empty object that reads as a change")

	assert.Equal(t, both, prunePatchPaths(both, nil), "no paths means no change")
	assert.Nil(t, prunePatchPaths(nil, []string{"spec.x"}))
}

// TestRefuseStepFields covers the refusal in isolation: structured verdict, the
// field gone from the patch, and Dorgu owning both sentences on the step.
func TestRefuseStepFields(t *testing.T) {
	step := &dorguv1.RemediationStep{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
		Description: "Raise the limit to 128Mi, well within a 2x ceiling",
		Rationale:   "the persona's recorded intent is 128Mi",
		Patch:       mustJSON(`{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`),
		PrePatchState: mustJSON(
			`{"spec":{"resources":{"limits":{"memory":"8Mi"}}}}`),
	}

	refuseStepFields(step, []SafetyViolation{{
		Rule:    "blast-radius",
		Message: "resource change for spec.resources.limits.memory exceeds maximum: 16.0x increase (max 2.0x)",
		BlastRadius: &BlastRadiusViolation{
			Field: "spec.resources.limits.memory", Baseline: "8Mi", Requested: "128Mi",
			Ratio: 16, MaxRatio: 2,
		},
	}})

	assert.False(t, step.AutoExecutable)
	assert.Nil(t, step.Patch, "a refused field leaves nothing to apply")
	assert.Nil(t, step.PrePatchState)

	require.Len(t, step.Safety, 1)
	assert.Equal(t, dorguv1.SafetyVerdictRejected, step.Safety[0].Verdict)
	assert.Equal(t, "16.0x", step.Safety[0].Ratio)
	assert.Equal(t, "2.0x", step.Safety[0].MaxRatio)
	assert.Empty(t, step.Safety[0].Permitted)

	assert.Equal(t, "This step applies nothing. "+step.Safety[0].Message, step.Description)
	assert.NotContains(t, step.Description, "well within a 2x ceiling")
	assert.NotContains(t, step.Rationale, "128Mi")
}

// TestProposer_AIPath_DerivedPatchKeepsWhatThePlanGotRight is the near-miss
// case: the plan raises the memory REQUEST, which is appliable and which the
// guardrails accept, while the diagnosis calls for the memory LIMIT. Appliable
// is not the same as appliable for the thing that is broken, so Dorgu adds the
// limit and keeps the request the plan already got right.
func TestProposer_AIPath_DerivedPatchKeepsWhatThePlanGotRight(t *testing.T) {
	p, incident := memhogWorld(t)
	p.planner = &stubPlanner{plan: &planner.RemediationPlan{
		RootCause:  "oom",
		Confidence: 0.8,
		Steps: []planner.PlannedStep{{
			Order: 1, Type: "persona-update", Description: "Raise the memory request", Rationale: "r", Risk: "low",
			Patch: json.RawMessage(`{"spec":{"resources":{"requests":{"memory":"48Mi"}}}}`),
		}},
	}}

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	step := result.Action.Spec.Steps[0]
	assert.True(t, step.AutoExecutable)
	assert.JSONEq(t,
		`{"spec":{"resources":{"limits":{"memory":"128Mi"},"requests":{"memory":"48Mi"}}}}`,
		string(step.Patch.Raw),
		"the diagnosed limit is added and the plan's own request change survives")
}

// TestMergePatches covers the merge in isolation, including the degenerate
// inputs that must not lose a patch.
func TestMergePatches(t *testing.T) {
	base := mustJSON(`{"spec":{"resources":{"requests":{"memory":"48Mi"}}}}`)
	overlay := mustJSON(`{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`)

	assert.JSONEq(t,
		`{"spec":{"resources":{"limits":{"memory":"128Mi"},"requests":{"memory":"48Mi"}}}}`,
		string(mergePatches(base, overlay).Raw))

	assert.JSONEq(t, `{"spec":{"a":"2"}}`,
		string(mergePatches(mustJSON(`{"spec":{"a":"1"}}`), mustJSON(`{"spec":{"a":"2"}}`)).Raw),
		"the overlay wins where both set a path")

	assert.Equal(t, overlay, mergePatches(nil, overlay))
	assert.Equal(t, base, mergePatches(base, nil))
	assert.Equal(t, overlay, mergePatches(mustJSON(`not json`), overlay))
}
