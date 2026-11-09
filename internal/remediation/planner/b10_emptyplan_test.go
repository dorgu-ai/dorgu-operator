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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/llm/llmtest"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics/metricstest"
)

// B10 (e), D1.1 finding 7: the AI planner returned an empty plan again (Kroki, 1 of 5 calls;
// D0.8 had Stirling-PDF). The reply was valid JSON with `steps: []`, which parsed as a plan,
// was counted as a success, and was cached for the whole llm-cache-ttl (30 minutes), so
// the situation fell back to the rule-based plan each cycle without the model being asked
// again. The operator logs no model reply, so the cause of an empty reply cannot be read
// back; what is cheap and safe is to treat it as the unusable reply it is: asked once more,
// counted as invalid output, and remembered for the short failure TTL, not the long one.

func emptyReply() *llm.Response {
	return planReply(map[string]any{"rootCause": "nothing to do", "confidence": 0.4, "steps": []map[string]any{}})
}

func TestEmptyPlan_AReplyWithNoStepsIsRetriedAndThenMalformed(t *testing.T) {
	invalid := func() float64 {
		return metricstest.Counter(t, "dorgu_llm_invalid_output_total", map[string]string{"provider": "stub", "purpose": llm.PurposePlanning})
	}
	before := invalid()
	client := llmtest.New(func(llm.Request) (*llm.Response, error) { return emptyReply(), nil })

	plan, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})

	require.Error(t, err)
	assert.Nil(t, plan)
	assert.True(t, errors.Is(err, ErrMalformedPlan), "got %v", err)
	assert.Equal(t, 2, client.Calls(), "asked once more")
	assert.Equal(t, 2.0, invalid()-before, "both unusable replies are counted as invalid output")
}

func TestEmptyPlan_ARealPlanOnTheSecondAttemptIsUsed(t *testing.T) {
	calls := 0
	client := llmtest.New(func(llm.Request) (*llm.Response, error) {
		calls++
		if calls == 1 {
			return emptyReply(), nil
		}
		return planReply(map[string]any{
			"rootCause": "ok", "confidence": 0.7,
			"steps": []map[string]any{{"order": 1, "type": "manual", "description": "look", "rationale": "why", "risk": "low"}},
		}), nil
	})

	plan, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})

	require.NoError(t, err)
	require.Len(t, plan.Steps, 1)
	assert.Equal(t, 2, client.Calls())
}

func TestEmptyPlan_ThePromptAsksForAtLeastOneStep(t *testing.T) {
	assert.Contains(t, planSystemPrompt, "at least one step")
	assert.Contains(t, planSystemPrompt, "never an empty steps list")
}
