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
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/llm/llmtest"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
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

// --- L-6: a model that keeps answering with nothing is not asked every few minutes ---------

// clockedCache returns a CachedPlanner over a fake clock the test can move.
func clockedCache(inner Planner, ttl time.Duration) (*CachedPlanner, func(time.Duration)) {
	p := NewCachedPlanner(inner, ttl, logr.Discard())
	now := time.Now()
	p.cache.SetClock(func() time.Time { return now })
	return p, func(d time.Duration) { now = now.Add(d) }
}

func TestEmptyPlan_TwoInARowAreRememberedForTheFullTTL(t *testing.T) {
	unusable := func() float64 { return planCache(t, metrics.LLMCacheUnusable) }
	before := unusable()
	inner := &countingInner{err: ErrEmptyPlan}
	p, advance := clockedCache(inner, 30*time.Minute)

	_, err := p.PlanRemediation(context.Background(), baseContext())
	require.ErrorIs(t, err, ErrEmptyPlan)
	require.Equal(t, 1, inner.calls)

	// The first empty plan is remembered for the failure TTL only.
	advance(llm.FailureTTL + time.Minute)
	_, _ = p.PlanRemediation(context.Background(), baseContext())
	require.Equal(t, 2, inner.calls, "after the short TTL it is asked again")
	assert.Equal(t, 1.0, unusable()-before, "the second consecutive empty plan is the one that sticks")

	// The second is remembered for the full TTL: not asked again after the short one passes.
	advance(llm.FailureTTL + time.Minute)
	_, err = p.PlanRemediation(context.Background(), baseContext())
	require.ErrorIs(t, err, ErrEmptyPlan)
	assert.Equal(t, 2, inner.calls, "no call: the unusable answer is cached for the normal TTL")

	advance(30 * time.Minute)
	_, _ = p.PlanRemediation(context.Background(), baseContext())
	assert.Equal(t, 3, inner.calls, "and it is asked again when that TTL ends")
}

func TestEmptyPlan_AUsablePlanEndsTheRun(t *testing.T) {
	inner := &countingInner{err: ErrEmptyPlan}
	p, advance := clockedCache(inner, 30*time.Minute)
	_, _ = p.PlanRemediation(context.Background(), baseContext())
	advance(llm.FailureTTL + time.Minute)

	inner.err, inner.plan = nil, cachedTestPlan()
	_, err := p.PlanRemediation(context.Background(), baseContext())
	require.NoError(t, err)

	// A later empty reply for the same situation is a first one again.
	advance(31 * time.Minute)
	inner.err, inner.plan = ErrEmptyPlan, nil
	_, _ = p.PlanRemediation(context.Background(), baseContext())
	advance(llm.FailureTTL + time.Minute)
	_, _ = p.PlanRemediation(context.Background(), baseContext())
	assert.Equal(t, 4, inner.calls, "short TTL again, so it was asked again")
}

func TestEmptyPlan_OtherFailuresKeepTheShortTTL(t *testing.T) {
	inner := &countingInner{err: errors.New("the provider is down")}
	p, advance := clockedCache(inner, 30*time.Minute)
	for range 3 {
		_, _ = p.PlanRemediation(context.Background(), baseContext())
		advance(llm.FailureTTL + time.Minute)
	}
	assert.Equal(t, 3, inner.calls)
}

func TestEmptyPlan_TheTrackedRunsAreBounded(t *testing.T) {
	p := NewCachedPlanner(&countingInner{}, time.Hour, logr.Discard())
	for i := range maxTrackedEmpties + 5 {
		p.countEmpty(fmt.Sprint(i))
	}
	assert.LessOrEqual(t, len(p.empties), maxTrackedEmpties)
}

// The planner reports the empty reply as its own error, which is still a malformed one.
func TestEmptyPlan_TheErrorIsAMalformedPlanToo(t *testing.T) {
	client := llmtest.New(func(llm.Request) (*llm.Response, error) { return emptyReply(), nil })
	_, err := planWith(client).PlanRemediation(context.Background(), RemediationContext{})
	assert.ErrorIs(t, err, ErrEmptyPlan)
	assert.ErrorIs(t, err, ErrMalformedPlan)
}
