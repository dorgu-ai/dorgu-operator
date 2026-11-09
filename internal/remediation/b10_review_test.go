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

// Independent review tests for operator PR #62 (B10), head d5406e2. Each test FAILS on
// that head and states the property the PR's own text claims.
//
// Drop in internal/remediation/ and run:
//   go test ./internal/remediation -run 'TestReviewB10' -count=1

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// A refused command that the display sanitizer lets through (quotes are not in its
// forbidden set) and the gate refuses (quotes are not plain bytes). It is the
// R9 shape: one command that is really a shell program.
const reviewRefused = "kubectl exec my-app-0 -n default -- sh -c 'rm -rf /var/lib/app/data'"

func reviewPlan(keptDescription, keptRationale string) *planner.RemediationPlan {
	return &planner.RemediationPlan{
		RootCause:  "the image tag does not exist",
		Confidence: 0.8,
		Steps: []planner.PlannedStep{
			{Order: 1, Type: dorguv1.StepTypeManual, Description: "Check which image the pod pulls.",
				Rationale: "the tag is wrong", Risk: "low", Command: "kubectl describe pod -l app=my-app -n default"},
			{Order: 2, Type: dorguv1.StepTypeManual, Description: "Clear the cache.", Rationale: "stale", Risk: "low",
				Command: reviewRefused},
			{Order: 3, Type: dorguv1.StepTypeManual, Description: keptDescription, Rationale: keptRationale, Risk: "low"},
		},
	}
}

// C-1: the cut is a literal match of the WHOLE refused command. Anything the model
// writes that a shell reads the same but that is not that exact string survives into
// kept fields, and mentionsAnyCommand (the fail-closed check) matches the same
// literal, so it does not fire either. CHANGELOG note 3 says "the refused command's
// text is removed from every field that is kept (a proposal fails closed if any of it
// is left)".
func TestReviewB10_RefusedCommandTextSurvivesInKeptFields(t *testing.T) {
	cases := map[string]struct{ description, rationale string }{
		"only the dangerous tail is quoted": {
			description: "Then run sh -c 'rm -rf /var/lib/app/data' inside the pod and fix the tag.",
			rationale:   "the tag must exist"},
		"same program, other quotes": {
			description: `Then run: kubectl exec my-app-0 -n default -- sh -c "rm -rf /var/lib/app/data" and fix the tag.`,
			rationale:   "the tag must exist"},
		"split across two fields": {
			description: "Run kubectl exec my-app-0 -n default -- and fix the tag.",
			rationale:   "the second half is sh -c 'rm -rf /var/lib/app/data'"},
		"a shell line continuation inside the quotation": {
			description: "Run kubectl exec my-app-0 -n default -- \\\n sh -c 'rm -rf /var/lib/app/data' and fix the tag.",
			rationale:   "the tag must exist"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result, _ := proposeImageError(t, reviewPlan(tc.description, tc.rationale))

			if !result.Proposed {
				return // failing closed is the acceptable outcome
			}
			raw, err := json.Marshal(result.Action.Spec)
			require.NoError(t, err)
			assert.NotContains(t, strings.ToLower(string(raw)), "rm -rf",
				"the proposal was written with the refused command's dangerous part still in a kept field")
		})
	}
}

// C-2: dropping step 2 renumbers step 3 to 2, so a kept step that points at a step by
// number now points at itself or at the wrong step. The note about dropped steps is only
// appended to the explanation, never to the step.
func TestReviewB10_AKeptStepStillPointsAtADroppedStepByNumber(t *testing.T) {
	result, _ := proposeImageError(t, reviewPlan(
		"After step 2 has run, correct the image tag in your manifest.",
		"Do the command from the step above first."))

	require.True(t, result.Proposed)
	for _, step := range result.Action.Spec.Steps {
		assert.NotContains(t, strings.ToLower(step.Description), "after step 2",
			"step %d refers to step 2, which is now itself (it was step 3) because the real step 2 was dropped", step.Order)
		assert.NotContains(t, strings.ToLower(step.Rationale), "step above",
			"step %d refers to the command in the step above, which was dropped", step.Order)
	}
}

// D-1: only the exact word is a placeholder; the same filler with one more word is
// written as the step's text (D1.1 finding 6: `[1] manual: placeholder`).
func TestReviewB10_APlaceholderWithAnExtraWordIsStillWritten(t *testing.T) {
	plan := &planner.RemediationPlan{RootCause: "the container exits with code 1 right after start", Confidence: 0.7,
		Steps: []planner.PlannedStep{
			{Order: 1, Type: dorguv1.StepTypeManual, Description: "Placeholder step", Rationale: "placeholder text", Risk: "low"},
		}}

	result, _ := proposeImageError(t, plan)

	require.True(t, result.Proposed)
	for _, step := range result.Action.Spec.Steps {
		assert.NotContains(t, strings.ToLower(step.Description), "placeholder")
		assert.NotContains(t, strings.ToLower(step.Rationale), "placeholder")
	}
}
