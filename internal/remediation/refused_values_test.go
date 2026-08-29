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

// renderedSurfaces enumerates every string a client can print for a plan,
// minus the guardrail's own verdict messages.
//
// The exclusion is the whole point of the split. A verdict says "the plan asked
// to set limits.memory to 512Mi ... so this field is refused": that sentence has
// to name the refused value, because naming it is what the sentence is for.
// Every OTHER surface is an offer, and an offer may not carry a value a
// guardrail refused.
//
// prePatchState is excluded for the same reason from the other direction. It
// records what the persona holds now, which in the clean-room case genuinely
// WAS 512Mi, so a `512Mi -> 16Mi` diff is an accurate statement of a fact and
// removing it would break rollback.
func renderedSurfaces(action *dorguv1.RemediationAction) []string {
	var out []string

	add := func(label, text string, safety []dorguv1.StepSafety) {
		for _, s := range safety {
			if s.Message != "" {
				text = strings.ReplaceAll(text, s.Message, "")
			}
		}
		if strings.TrimSpace(text) != "" {
			out = append(out, label+": "+text)
		}
	}

	add("planSummary", action.Spec.PlanSummary, allStepSafety(action))
	add("explanation", action.Spec.Explanation, allStepSafety(action))
	out = append(out, patchSurfaces("action.patch", action.Spec.Action.Patch)...)

	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		prefix := fmt.Sprintf("steps[%d]", i)
		add(prefix+".description", step.Description, step.Safety)
		add(prefix+".rationale", step.Rationale, step.Safety)
		if step.Command != "" {
			out = append(out, prefix+".command: "+step.Command)
		}
		out = append(out, patchSurfaces(prefix+".patch", step.Patch)...)
	}
	return out
}

// allStepSafety flattens every verdict on the plan, for the action-level
// surfaces that quote them.
func allStepSafety(action *dorguv1.RemediationAction) []dorguv1.StepSafety {
	var out []dorguv1.StepSafety
	for i := range action.Spec.Steps {
		out = append(out, action.Spec.Steps[i].Safety...)
	}
	return out
}

// patchSurfaces renders a patch's leaves as "path=value" strings.
func patchSurfaces(label string, patch *apiextensionsv1.JSON) []string {
	if patch == nil || len(patch.Raw) == 0 {
		return nil
	}
	values := patchLeafValues(patch.Raw)
	out := make([]string, 0, len(values))
	for _, path := range sortedStringKeys(values) {
		out = append(out, fmt.Sprintf("%s %s=%s", label, path, values[path]))
	}
	return out
}

// assertNoSurfaceCarries fails naming the exact surface that leaked, because
// "somewhere in the plan" is not a debuggable failure for a bug whose whole
// nature is that it reappears one screen further down.
func assertNoSurfaceCarries(t *testing.T, action *dorguv1.RemediationAction, refused ...string) {
	t.Helper()
	for _, surface := range renderedSurfaces(action) {
		for _, value := range refused {
			assert.False(t, mentionsResourceValue(surface, value),
				"a guardrail refused %s and it is still offered here — %s", value, surface)
		}
	}
}

// driftWorld is clean-room run #5's `drifted` app: a persona recording
// 512Mi/256Mi over a live container running 8Mi/8Mi, unmanaged, so the CLI is
// allowed to patch the Deployment and a pasted command actually lands.
func driftWorld(t *testing.T) (*Proposer, *dorguv1.IncidentMemory) {
	t.Helper()

	persona := personaWithLimits(groundedPersona, "500m", "512Mi")
	persona.Spec.Resources.Requests = &dorguv1.ResourceValues{CPU: "100m", Memory: "256Mi"}
	deploy := liveDeployment(groundedPersona,
		map[corev1.ResourceName]string{corev1.ResourceMemory: "8Mi", corev1.ResourceCPU: "500m"},
		map[corev1.ResourceName]string{corev1.ResourceMemory: "8Mi", corev1.ResourceCPU: "100m"})
	incident := newTestIncident(defaultNamespace, "drift", groundedPersona, "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	return NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger()), incident
}

// cleanRoomDriftPlan is the plan the model produced in clean-room run #5: one
// persona-update asking for both refused values, and one workload-apply step
// whose command re-offers both of them as a copy-paste kubectl invocation.
func cleanRoomDriftPlan() *planner.RemediationPlan {
	return &planner.RemediationPlan{
		RootCause:  "the container's 8Mi memory limit is far below its working set, so it OOM-kills on start",
		Confidence: 0.9,
		Steps: []planner.PlannedStep{
			{
				Order:       1,
				Type:        "persona-update",
				Description: "Raise the memory limit to 512Mi and the request to 256Mi",
				Rationale:   "the workload needs roughly 400MB at peak",
				Risk:        "low",
				Patch: json.RawMessage(
					`{"spec":{"resources":{"limits":{"memory":"512Mi"},"requests":{"memory":"256Mi"}}}}`),
			},
			{
				Order:       2,
				Type:        "workload-apply",
				Description: "Apply the corrected memory resources directly to the Deployment.",
				Rationale:   "This is a direct write and is safe because managedBy is 'unmanaged' — no external controller owns this workload.",
				Risk:        "low",
				Command: "kubectl set resources deployment/my-app --containers=app " +
					"--limits=memory=512Mi --requests=memory=256Mi -n default",
			},
		},
	}
}

// TestProposer_CR501_RefusedValuesNeverReachARenderedSurface is clean-room run
// #5's CR5-01, reproduced end to end.
//
// Two guardrails refused 512Mi and 256Mi against a live 8Mi baseline, printed
// the refusal, and then three lines below it Dorgu printed a ready-to-run
// kubectl command applying both, on the confirmation screen, with its own
// rationale calling the write "safe". One paste defeats the product's central
// safety claim, by a user doing exactly what the plan tells them.
func TestProposer_CR501_RefusedValuesNeverReachARenderedSurface(t *testing.T) {
	p, incident := driftWorld(t)
	p.planner = &stubPlanner{plan: cleanRoomDriftPlan()}

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	action := result.Action

	// The guardrails did fire and said so — that half was never broken.
	verdicts := allStepSafety(action)
	require.NotEmpty(t, verdicts, "the blast-radius guardrail must have ruled on this plan")

	assertNoSurfaceCarries(t, action, "512Mi", "256Mi")

	// And the plan still does something: 2x of the live 8Mi is what survives.
	require.True(t, action.HasAutoApplicableChange(),
		"scrubbing the refused values must not turn a real fix into an empty plan")
	assert.Equal(t, "16Mi", patchLimit(t, action.Spec.Action.Patch.Raw, "memory"))
}

// TestProposer_CR501_CommandIsDroppedNotRewritten pins the shape of the fix on
// the surface CR5-01 actually landed on. The prose stays, because it tells the
// reader something true; the command goes, because Dorgu cannot rebuild an
// arbitrary kubectl invocation safely and a half-corrected command is worse
// than none.
func TestProposer_CR501_CommandIsDroppedNotRewritten(t *testing.T) {
	p, incident := driftWorld(t)
	p.planner = &stubPlanner{plan: cleanRoomDriftPlan()}

	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)

	var advisory *dorguv1.RemediationStep
	for i := range result.Action.Spec.Steps {
		if result.Action.Spec.Steps[i].Type == dorguv1.StepTypeWorkloadApply {
			advisory = &result.Action.Spec.Steps[i]
		}
	}
	require.NotNil(t, advisory, "the advisory workload-apply step is still part of the plan")
	assert.Empty(t, advisory.Command, "the command carried two refused values, so it is gone")
	assert.Contains(t, advisory.Description, "refused",
		"the reader is told why the command is not there")
}

// TestScrubRefusedValues covers the chokepoint directly, over the shapes a
// future code path could reintroduce the bug through. Each case is a surface
// CR5-01 or CF6-1 has already been found on once.
func TestScrubRefusedValues(t *testing.T) {
	rejected := dorguv1.StepSafety{
		Rule:      dorguv1.SafetyRuleBlastRadius,
		Verdict:   dorguv1.SafetyVerdictRejected,
		Field:     "spec.resources.requests.memory",
		Baseline:  "8Mi",
		Requested: "256Mi",
	}
	clamped := dorguv1.StepSafety{
		Rule:      dorguv1.SafetyRuleBlastRadius,
		Verdict:   dorguv1.SafetyVerdictClamped,
		Field:     "spec.resources.limits.memory",
		Baseline:  "8Mi",
		Requested: "512Mi",
		Permitted: "16Mi",
	}

	tests := []struct {
		name   string
		build  func() *dorguv1.RemediationAction
		assert func(t *testing.T, action *dorguv1.RemediationAction)
	}{
		{
			name: "a command on another step carrying a value refused on this one",
			build: func() *dorguv1.RemediationAction {
				a := planWith(
					dorguv1.RemediationStep{Order: 1, Type: dorguv1.StepTypePersonaUpdate,
						Safety: []dorguv1.StepSafety{rejected}},
					dorguv1.RemediationStep{Order: 2, Type: dorguv1.StepTypeWorkloadApply,
						Command: "kubectl set resources deployment/x --requests=memory=256Mi -n d"},
				)
				return a
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Empty(t, a.Spec.Steps[1].Command)
			},
		},
		{
			name: "a patch leaf still holding the refused value",
			build: func() *dorguv1.RemediationAction {
				return planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypePersonaUpdate,
					Patch:  mustJSON(`{"spec":{"resources":{"requests":{"memory":"256Mi"}}}}`),
					Safety: []dorguv1.StepSafety{rejected},
				})
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Nil(t, a.Spec.Steps[0].Patch, "nothing survives, so the patch is gone")
				assert.False(t, a.Spec.Steps[0].AutoExecutable)
			},
		},
		{
			name: "a patch keeping its permitted leaf while losing the refused one",
			build: func() *dorguv1.RemediationAction {
				return planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
					Patch: mustJSON(
						`{"spec":{"resources":{"limits":{"memory":"16Mi"},"requests":{"memory":"256Mi"}}}}`),
					Safety: []dorguv1.StepSafety{rejected, clamped},
				})
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				require.NotNil(t, a.Spec.Steps[0].Patch)
				values := patchLeafValues(a.Spec.Steps[0].Patch.Raw)
				assert.Equal(t, "16Mi", values["spec.resources.limits.memory"])
				assert.NotContains(t, values, "spec.resources.requests.memory")
				assert.True(t, a.Spec.Steps[0].AutoExecutable, "a step with a surviving field still applies")
			},
		},
		{
			name: "model prose naming the refused value in a description",
			build: func() *dorguv1.RemediationAction {
				return planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypeManual,
					Description: "Raise the request to 256Mi by hand. Then watch the pod restart.",
					Safety:      nil,
				}, dorguv1.RemediationStep{
					Order: 2, Type: dorguv1.StepTypePersonaUpdate,
					Safety: []dorguv1.StepSafety{rejected},
				})
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.NotContains(t, a.Spec.Steps[0].Description, "256Mi")
				assert.Contains(t, a.Spec.Steps[0].Description, "watch the pod restart",
					"the sentences that were fine are kept")
			},
		},
		{
			name: "model prose naming the refused value in the plan summary",
			build: func() *dorguv1.RemediationAction {
				a := planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypePersonaUpdate,
					Safety: []dorguv1.StepSafety{clamped},
				})
				a.Spec.PlanSummary = "The container needs more memory. Raising the limit to 512Mi resolves it."
				return a
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.NotContains(t, a.Spec.PlanSummary, "512Mi")
				assert.Contains(t, a.Spec.PlanSummary, "The container needs more memory.")
			},
		},
		{
			name: "the guardrail's own verdict message keeps the value it is about",
			build: func() *dorguv1.RemediationAction {
				entry := clamped
				entry.Message = blastRadiusClampedMessage(entry)
				return planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypePersonaUpdate,
					Description: dorguAuthoredDescription("Set spec.resources.limits.memory to 16Mi on the ApplicationPersona.",
						[]dorguv1.StepSafety{entry}),
					Safety: []dorguv1.StepSafety{entry},
				})
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Contains(t, a.Spec.Steps[0].Description, "512Mi",
					"a refusal that cannot name what it refused explains nothing")
				assertNoSurfaceCarries(t, a, "512Mi")
			},
		},
		{
			name: "the legacy back-compat action patch",
			build: func() *dorguv1.RemediationAction {
				a := planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypePersonaUpdate,
					Safety: []dorguv1.StepSafety{rejected},
				})
				a.Spec.Action = dorguv1.RemediationActionDetail{
					Type:  dorguv1.ActionTypePersonaUpdate,
					Patch: mustJSON(`{"spec":{"resources":{"requests":{"memory":"256Mi"}}}}`),
				}
				return a
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Nil(t, a.Spec.Action.Patch)
				assert.Equal(t, dorguv1.ActionTypeNotification, a.Spec.Action.Type,
					"an action with nothing left to apply is not a persona-update")
			},
		},
		{
			name: "a quantity written in a different unit is still the refused value",
			build: func() *dorguv1.RemediationAction {
				return planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypePersonaUpdate,
					Safety: []dorguv1.StepSafety{rejected},
				}, dorguv1.RemediationStep{
					Order: 2, Type: dorguv1.StepTypeWorkloadApply,
					// 256Mi expressed as bytes.
					Command: "kubectl set resources deployment/x --requests=memory=268435456 -n d",
				})
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Empty(t, a.Spec.Steps[1].Command)
			},
		},
		{
			name: "a value that merely contains the refused digits is left alone",
			build: func() *dorguv1.RemediationAction {
				return planWith(dorguv1.RemediationStep{
					Order: 1, Type: dorguv1.StepTypePersonaUpdate,
					Safety: []dorguv1.StepSafety{rejected},
				}, dorguv1.RemediationStep{
					Order: 2, Type: dorguv1.StepTypeManual,
					Command: "kubectl get deployment/x -n d",
					// 1256Mi is not 256Mi.
					Description: "Confirm the node has 1256Mi allocatable before retrying.",
				})
			},
			assert: func(t *testing.T, a *dorguv1.RemediationAction) {
				assert.Equal(t, "kubectl get deployment/x -n d", a.Spec.Steps[1].Command)
				assert.Contains(t, a.Spec.Steps[1].Description, "1256Mi")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := tt.build()
			scrubRefusedValues(action)
			tt.assert(t, action)
		})
	}
}

// planWith builds a minimal RemediationAction around the given steps.
func planWith(steps ...dorguv1.RemediationStep) *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		Spec: dorguv1.RemediationActionSpec{
			Steps:       steps,
			PlanSummary: "root cause",
			Explanation: "AI remediation plan",
		},
	}
}
