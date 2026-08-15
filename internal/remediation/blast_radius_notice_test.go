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

// F-11: report-worker needed ~120M. The plan proposed 48Mi to 96Mi at confidence
// 0.88, asserting the increase resolves the OOM, and the pod went straight back
// to OOMKilled. 96Mi was not a judgement, it was the ceiling.
package remediation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func rawJSON(s string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(s)}
}

// memoryAction builds a persona-update action changing the memory limit.
func memoryAction(from, to string) *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		Spec: dorguv1.RemediationActionSpec{
			Explanation: "Increase memory limit to stop the OOM kills.",
			Confidence:  "0.88",
			Action: dorguv1.RemediationActionDetail{
				Type:          dorguv1.ActionTypePersonaUpdate,
				Patch:         rawJSON(`{"spec":{"resources":{"limits":{"memory":"` + to + `"}}}}`),
				PrePatchState: rawJSON(`{"spec":{"resources":{"limits":{"memory":"` + from + `"}}}}`),
			},
		},
	}
}

func TestDiscloseBlastRadiusClamp_AtTheCap(t *testing.T) {
	action := memoryAction("48Mi", "96Mi")

	require.True(t, discloseBlastRadiusClamp(action))

	assert.Contains(t, action.Spec.Explanation, "Clamped by the 2x blast-radius guardrail")
	assert.Contains(t, action.Spec.Explanation, "spec.resources.limits.memory")
	assert.Contains(t, action.Spec.Explanation, "a second increase may be required")
	assert.Equal(t, "0.75", action.Spec.Confidence, "confidence must be damped when the fix was truncated")
}

func TestDiscloseBlastRadiusClamp_BelowTheCap(t *testing.T) {
	// 48Mi to 72Mi is 1.5x: the diagnosis chose this, not the guardrail.
	action := memoryAction("48Mi", "72Mi")

	require.False(t, discloseBlastRadiusClamp(action))

	assert.Equal(t, "Increase memory limit to stop the OOM kills.", action.Spec.Explanation)
	assert.Equal(t, "0.88", action.Spec.Confidence)
}

func TestDiscloseBlastRadiusClamp_AnnotatesPlanSummaryAndStep(t *testing.T) {
	action := &dorguv1.RemediationAction{
		Spec: dorguv1.RemediationActionSpec{
			PlanSource:  dorguv1.PlanSourceAIAnthropic,
			PlanSummary: "report-worker is killed on startup because its memory limit is below its working set.",
			Explanation: "AI remediation plan (2 steps)",
			Confidence:  "0.88",
			Steps: []dorguv1.RemediationStep{
				{
					Order:          1,
					ID:             "step-1",
					Type:           dorguv1.StepTypePersonaUpdate,
					Description:    "Raise the memory limit to 96Mi.",
					Rationale:      "The container needs more than 48Mi.",
					Risk:           "low",
					AutoExecutable: true,
					Patch:          rawJSON(`{"spec":{"resources":{"limits":{"memory":"96Mi"}}}}`),
					PrePatchState:  rawJSON(`{"spec":{"resources":{"limits":{"memory":"48Mi"}}}}`),
				},
				{
					Order:          2,
					ID:             "step-2",
					Type:           dorguv1.StepTypeManual,
					Description:    "Watch for further OOM kills.",
					Risk:           "low",
					AutoExecutable: false,
				},
			},
		},
	}

	require.True(t, discloseBlastRadiusClamp(action))

	assert.Contains(t, action.Spec.PlanSummary, "Clamped by the 2x blast-radius guardrail")
	assert.Contains(t, action.Spec.Explanation, "Clamped by the 2x blast-radius guardrail")
	assert.Contains(t, action.Spec.Steps[0].Rationale, "Clamped by the 2x blast-radius guardrail")
	assert.Contains(t, action.Spec.Steps[0].Rationale, "The container needs more than 48Mi.",
		"the original rationale must survive")
	assert.Empty(t, action.Spec.Steps[1].Rationale, "advisory steps are not annotated")
	assert.Equal(t, "0.75", action.Spec.Confidence)
}

func TestDiscloseBlastRadiusClamp_IsIdempotent(t *testing.T) {
	action := memoryAction("48Mi", "96Mi")
	require.True(t, discloseBlastRadiusClamp(action))
	first := action.Spec.Explanation

	// A second pass must not stack the caveat, though the confidence damping is
	// applied once per call, so only the text is compared.
	discloseBlastRadiusClamp(action)
	assert.Equal(t, first, action.Spec.Explanation)
}

func TestDiscloseBlastRadiusClamp_NoSnapshot(t *testing.T) {
	// Without a pre-patch snapshot there is no ratio to judge, so say nothing
	// rather than guess.
	action := memoryAction("48Mi", "96Mi")
	action.Spec.Action.PrePatchState = nil

	assert.False(t, discloseBlastRadiusClamp(action))
	assert.Equal(t, "0.88", action.Spec.Confidence)
}

func TestDiscloseBlastRadiusClamp_MultipleFields(t *testing.T) {
	action := &dorguv1.RemediationAction{
		Spec: dorguv1.RemediationActionSpec{
			Explanation: "Raise both limits.",
			Confidence:  "0.90",
			Action: dorguv1.RemediationActionDetail{
				Type:          dorguv1.ActionTypePersonaUpdate,
				Patch:         rawJSON(`{"spec":{"resources":{"limits":{"memory":"96Mi","cpu":"200m"}}}}`),
				PrePatchState: rawJSON(`{"spec":{"resources":{"limits":{"memory":"48Mi","cpu":"100m"}}}}`),
			},
		},
	}

	require.True(t, discloseBlastRadiusClamp(action))
	assert.Contains(t, action.Spec.Explanation,
		"spec.resources.limits.cpu and spec.resources.limits.memory")
}

// No em dashes in user-facing strings (house style).
func TestBlastRadiusCaveat_HouseStyle(t *testing.T) {
	assert.NotContains(t, blastRadiusCaveat([]string{"spec.resources.limits.memory"}), "—")
}
