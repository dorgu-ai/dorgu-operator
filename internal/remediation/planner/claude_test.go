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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolUseResponse builds an Anthropic Messages API response carrying a single
// tool_use block with the given input.
func toolUseResponse(input map[string]any) claudeResponse {
	raw, _ := json.Marshal(input)
	return claudeResponse{
		StopReason: "tool_use",
		Content: []claudeContent{
			{Type: "tool_use", Name: planToolName, Input: raw},
		},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(v))
}

func TestPlanRemediation_WellFormed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request shape: forced tool use with the plan tool.
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "test-key", r.Header.Get("x-api-key"))
		assert.Equal(t, anthropicVersion, r.Header.Get("anthropic-version"))

		var req claudeRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "tool", req.ToolChoice.Type)
		assert.Equal(t, planToolName, req.ToolChoice.Name)
		require.Len(t, req.Tools, 1)
		assert.True(t, req.Tools[0].Strict)
		assert.Equal(t, planMaxTokens, req.MaxTokens)

		writeJSON(t, w, toolUseResponse(map[string]any{
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
		}))
	}))
	defer server.Close()

	p := &ClaudePlanner{apiKey: "test-key", model: defaultClaudeModel, client: server.Client(), baseURL: server.URL}

	plan, err := p.PlanRemediation(context.Background(), RemediationContext{})
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
}

func TestPlanRemediation_MalformedRetriesThenErrors(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		// No tool_use block — malformed for our purposes.
		writeJSON(t, w, claudeResponse{
			StopReason: "end_turn",
			Content:    []claudeContent{{Type: "text", Text: "I cannot produce JSON"}},
		})
	}))
	defer server.Close()

	p := &ClaudePlanner{apiKey: "test-key", model: defaultClaudeModel, client: server.Client(), baseURL: server.URL}

	plan, err := p.PlanRemediation(context.Background(), RemediationContext{})
	require.Error(t, err)
	assert.Nil(t, plan)
	assert.True(t, errors.Is(err, ErrMalformedPlan), "expected ErrMalformedPlan, got %v", err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "should retry once (2 total attempts)")
}

func TestPlanRemediation_MalformedThenValidSucceeds(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// First attempt: malformed (no tool_use).
			writeJSON(t, w, claudeResponse{Content: []claudeContent{{Type: "text", Text: "oops"}}})
			return
		}
		writeJSON(t, w, toolUseResponse(map[string]any{
			"rootCause":  "ok",
			"confidence": 0.7,
			"steps": []map[string]any{
				{"order": 1, "type": "manual", "description": "investigate", "rationale": "n/a", "risk": "low"},
			},
		}))
	}))
	defer server.Close()

	p := &ClaudePlanner{apiKey: "test-key", model: defaultClaudeModel, client: server.Client(), baseURL: server.URL}

	plan, err := p.PlanRemediation(context.Background(), RemediationContext{})
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
	require.Len(t, plan.Steps, 1)
	assert.Equal(t, "manual", plan.Steps[0].Type)
}

func TestPlanRemediation_HTTPErrorNoRetry(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer server.Close()

	p := &ClaudePlanner{apiKey: "test-key", model: defaultClaudeModel, client: server.Client(), baseURL: server.URL}

	plan, err := p.PlanRemediation(context.Background(), RemediationContext{})
	require.Error(t, err)
	assert.Nil(t, plan)
	assert.False(t, errors.Is(err, ErrMalformedPlan), "HTTP error is not a malformed-plan error")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "transport/HTTP errors are not retried")
}

func TestPlanRemediation_InvalidPatchDropped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, toolUseResponse(map[string]any{
			"rootCause":  "x",
			"confidence": 0.5,
			"steps": []map[string]any{
				{"order": 1, "type": "persona-update", "description": "d", "rationale": "r", "risk": "low", "patch": "not-json{"},
			},
		}))
	}))
	defer server.Close()

	p := &ClaudePlanner{apiKey: "test-key", model: defaultClaudeModel, client: server.Client(), baseURL: server.URL}

	plan, err := p.PlanRemediation(context.Background(), RemediationContext{})
	require.NoError(t, err)
	require.Len(t, plan.Steps, 1)
	assert.Empty(t, plan.Steps[0].Patch, "invalid patch JSON is dropped")
}

func TestNewClaudePlanner_RequiresKey(t *testing.T) {
	_, err := NewClaudePlanner("")
	require.Error(t, err)

	p, err := NewClaudePlanner("k")
	require.NoError(t, err)
	assert.Equal(t, defaultClaudeModel, p.model)
	p.SetModel("claude-custom")
	assert.Equal(t, "claude-custom", p.model)
	p.SetModel("") // empty is ignored
	assert.Equal(t, "claude-custom", p.model)
}

// TestPlanRemediation_ObjectPatchAccepted covers the shape the tool schema does
// not ask for but the model sometimes returns: the merge patch as a bare JSON
// object rather than a JSON-encoded string.
//
// This used to fail the decode of the whole tool input, not just the one field,
// so a perfectly good plan was retried once, failed identically, and degraded to
// the rule-based path with its diagnosis thrown away.
func TestPlanRemediation_ObjectPatchAccepted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, toolUseResponse(map[string]any{
			"rootCause":  "oom",
			"confidence": 0.8,
			"steps": []map[string]any{{
				"order": 1, "type": "persona-update", "description": "d", "rationale": "r", "risk": "low",
				"patch": map[string]any{"spec": map[string]any{
					"resources": map[string]any{"limits": map[string]any{"memory": "512Mi"}},
				}},
			}},
		}))
	}))
	defer server.Close()

	p := &ClaudePlanner{apiKey: "test-key", model: defaultClaudeModel, client: server.Client(), baseURL: server.URL}

	plan, err := p.PlanRemediation(context.Background(), RemediationContext{})
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
