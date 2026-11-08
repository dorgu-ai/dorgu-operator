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

package planner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/llm/llmtest"
)

// planReply is the structured reply the real providers hand back for a plan.
func planReply(input map[string]any) *llm.Response {
	raw, _ := json.Marshal(input)
	return &llm.Response{JSON: raw, Usage: llm.Usage{InputTokens: 900, OutputTokens: 300}}
}

func planWith(client llm.Client) *LLMPlanner {
	p, err := New(client)
	if err != nil {
		panic(err)
	}
	return p
}

func TestPlanRemediation_WellFormed(t *testing.T) {
	client := llmtest.New(func(llm.Request) (*llm.Response, error) {
		return planReply(map[string]any{
			"rootCause":  "memory limit too low for workload peak",
			"confidence": 0.9,
			"steps": []map[string]any{
				{
					"order":       1,
					"type":        "persona-update",
					"description": "Raise memory limit",
					"rationale":   "peak exceeds current limit",
					"risk":        "low",
					"patch":       `{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`,
				},
				{
					"order":       2,
					"type":        "restart",
					"description": "Re-apply workload",
					"rationale":   "pick up new limit",
					"risk":        "medium",
				},
			},
		}), nil
	})

	plan, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, "memory limit too low for workload peak", plan.RootCause)
	assert.InDelta(t, 0.9, plan.Confidence, 0.001)
	require.Len(t, plan.Steps, 2)

	assert.Equal(t, int32(1), plan.Steps[0].Order)
	assert.Equal(t, "persona-update", plan.Steps[0].Type)
	require.NotEmpty(t, plan.Steps[0].Patch)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`, string(plan.Steps[0].Patch))

	assert.Equal(t, "restart", plan.Steps[1].Type)
	assert.Empty(t, plan.Steps[1].Patch, "advisory step carries no patch")

	// The request is the planning request: structured, with the plan tool.
	require.Equal(t, 1, client.Calls())
	req := client.Requests()[0]
	assert.Equal(t, llm.PurposePlanning, req.Purpose)
	assert.Equal(t, planSystemPrompt, req.System)
	assert.Equal(t, planMaxTokens, req.MaxTokens)
	assert.Equal(t, planTimeout, req.Timeout)
	require.NotNil(t, req.Schema)
	assert.Equal(t, planToolName, req.Schema.Name)
	assert.Equal(t, planToolSchema(), req.Schema.Definition)
}

func TestPlanRemediation_MalformedRetriesThenErrors(t *testing.T) {
	// No structured reply at all: prose, which is malformed for our purposes.
	client := llmtest.New(func(llm.Request) (*llm.Response, error) {
		return llmtest.TextReply("I cannot produce JSON"), nil
	})

	plan, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})
	require.Error(t, err)
	assert.Nil(t, plan)
	assert.True(t, errors.Is(err, ErrMalformedPlan), "expected ErrMalformedPlan, got %v", err)
	assert.Equal(t, 2, client.Calls(), "should retry once (2 total attempts)")
}

func TestPlanRemediation_UnparseableStructuredReplyIsMalformed(t *testing.T) {
	client := llmtest.New(func(llm.Request) (*llm.Response, error) {
		return &llm.Response{JSON: json.RawMessage(`{"rootCause": 7}`)}, nil
	})

	_, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})
	assert.True(t, errors.Is(err, ErrMalformedPlan), "got %v", err)
}

func TestPlanRemediation_MalformedThenValidSucceeds(t *testing.T) {
	calls := 0
	client := llmtest.New(func(llm.Request) (*llm.Response, error) {
		calls++
		if calls == 1 {
			return llmtest.TextReply("oops"), nil
		}
		return planReply(map[string]any{
			"rootCause":  "ok",
			"confidence": 0.7,
			"steps": []map[string]any{
				{"order": 1, "type": "manual", "description": "investigate", "rationale": "n/a", "risk": "low"},
			},
		}), nil
	})

	plan, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, 2, client.Calls())
	require.Len(t, plan.Steps, 1)
	assert.Equal(t, "manual", plan.Steps[0].Type)
}

func TestPlanRemediation_RequestErrorNoRetry(t *testing.T) {
	boom := errors.New("API returned status 500")
	client := llmtest.Failing(boom)

	plan, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})
	require.Error(t, err)
	assert.Nil(t, plan)
	assert.ErrorIs(t, err, boom)
	assert.False(t, errors.Is(err, ErrMalformedPlan), "a failed request is not a malformed-plan error")
	assert.Equal(t, 1, client.Calls(), "failed requests are not retried")
}

func TestPlanRemediation_InvalidPatchDropped(t *testing.T) {
	client := llmtest.New(func(llm.Request) (*llm.Response, error) {
		return planReply(map[string]any{
			"rootCause":  "x",
			"confidence": 0.5,
			"steps": []map[string]any{
				{"order": 1, "type": "persona-update", "description": "d", "rationale": "r", "risk": "low", "patch": "not-json{"},
			},
		}), nil
	})

	plan, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})
	require.NoError(t, err)
	require.Len(t, plan.Steps, 1)
	assert.Empty(t, plan.Steps[0].Patch, "invalid patch JSON is dropped")
}

func TestNew_RequiresAClient(t *testing.T) {
	_, err := New(nil)
	require.Error(t, err)

	p, err := New(llmtest.Failing(errors.New("x")))
	require.NoError(t, err)
	assert.NotNil(t, p)
}

// TestPlanRemediation_ObjectPatchAccepted covers the shape the tool schema does
// not ask for but the model sometimes returns: the merge patch as a bare JSON
// object rather than a JSON-encoded string.
//
// This used to fail the decode of the whole tool input, not just the one field,
// so a perfectly good plan was retried once, failed identically, and degraded to
// the rule-based path with its diagnosis thrown away.
func TestPlanRemediation_ObjectPatchAccepted(t *testing.T) {
	client := llmtest.New(func(llm.Request) (*llm.Response, error) {
		return planReply(map[string]any{
			"rootCause":  "oom",
			"confidence": 0.8,
			"steps": []map[string]any{{
				"order": 1, "type": "persona-update", "description": "d", "rationale": "r", "risk": "low",
				"patch": map[string]any{"spec": map[string]any{
					"resources": map[string]any{"limits": map[string]any{"memory": "512Mi"}},
				}},
			}},
		}), nil
	})

	plan, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})
	require.NoError(t, err)
	require.Len(t, plan.Steps, 1)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`, string(plan.Steps[0].Patch))
}

// TestPlanPatch_UnmarshalJSON covers the decoder directly, including the values
// that must leave the patch empty rather than fail the surrounding step.
func TestPlanPatch_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "encoded string", in: `"{\"spec\":{\"a\":1}}"`, want: `{"spec":{"a":1}}`},
		{name: "bare object", in: `{"spec":{"a":1}}`, want: `{"spec":{"a":1}}`},
		{name: "null", in: `null`, want: ""},
		{name: "empty string", in: `""`, want: ""},
		{name: "string that is not JSON", in: `"not-json{"`, want: ""},
		{name: "number", in: `42`, want: ""},
		{name: "array", in: `[1,2]`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p planPatch
			require.NoError(t, json.Unmarshal([]byte(tt.in), &p), "a bad patch never fails the step around it")
			assert.Equal(t, tt.want, string(p.raw))
		})
	}
}

// TestPlanSystemPrompt_ForbidsCharacterisingTheGuardrail is CR-04's first
// defect, where the model asserted a 16x change was "well within a 2x ceiling"
// and the guardrail contradicted it in the next line. The guardrail's verdict is
// Dorgu's, and the prompt has to say so.
func TestPlanSystemPrompt_ForbidsCharacterisingTheGuardrail(t *testing.T) {
	assert.Contains(t, planSystemPrompt, "Never say whether a change is within, under, or outside any cap")
	assert.Contains(t, planSystemPrompt, "Say nothing about limits on your own proposal.")
}

// TestPlanSystemPrompt_RoutesResourceChangesThroughThePersona is CR-04's sibling
// in CR-01: the planner described the memory fix as workload-apply steps, which
// the CRD forbids from ever being auto-executable, so nothing could be applied.
func TestPlanSystemPrompt_RoutesResourceChangesThroughThePersona(t *testing.T) {
	assert.Contains(t, planSystemPrompt, "ALWAYS a\n   persona-update step carrying a patch")
	assert.Contains(t, planSystemPrompt, "Never express a resource change as\n   workload-apply")
	assert.Contains(t, planSystemPrompt, "A persona-update step with no patch applies nothing.")
}
