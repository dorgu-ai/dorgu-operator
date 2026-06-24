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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// stubPlanner is a test double for planner.Planner.
type stubPlanner struct {
	plan *planner.RemediationPlan
	err  error
}

func (s *stubPlanner) PlanRemediation(_ context.Context, _ planner.RemediationContext) (*planner.RemediationPlan, error) {
	return s.plan, s.err
}

func threeStepPlan() *planner.RemediationPlan {
	return &planner.RemediationPlan{
		RootCause:  "memory limit too low; container OOMKilled at peak load",
		Confidence: 0.88,
		Steps: []planner.PlannedStep{
			{
				Order:       1,
				Type:        "persona-update",
				Description: "Raise memory limit from 256Mi to 384Mi",
				Rationale:   "peak usage exceeds the current limit",
				Risk:        "low",
				Patch:       json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`),
			},
			{
				Order:       2,
				Type:        "restart",
				Description: "Re-apply the workload to pick up the new limit",
				Rationale:   "advisory: the operator does not write workloads",
				Risk:        "medium",
			},
			{
				Order:       3,
				Type:        "manual",
				Description: "Verify memory headroom after rollout",
				Rationale:   "confirm the fix held",
				Risk:        "low",
			},
		},
	}
}

func TestProposer_AIPath_MapsPlanToSteps(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{plan: threeStepPlan()}))

	diag := newOOMDiagnosis("default", "my-app", detection.SeverityCritical)
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)
	require.NotNil(t, result.Action)

	a := result.Action
	require.Len(t, a.Spec.Steps, 3)
	assert.Equal(t, dorguv1.PlanSourceAIAnthropic, a.Spec.PlanSource)
	assert.Equal(t, "memory limit too low; container OOMKilled at peak load", a.Spec.PlanSummary)
	assert.Equal(t, "0.88", a.Spec.Confidence)

	// AutoExecutable only on the persona-update step.
	assert.True(t, a.Spec.Steps[0].AutoExecutable)
	assert.False(t, a.Spec.Steps[1].AutoExecutable)
	assert.False(t, a.Spec.Steps[2].AutoExecutable)

	// persona-update step carries patch + pre-patch snapshot.
	require.NotNil(t, a.Spec.Steps[0].Patch)
	require.NotNil(t, a.Spec.Steps[0].PrePatchState)
	var pre map[string]interface{}
	require.NoError(t, json.Unmarshal(a.Spec.Steps[0].PrePatchState.Raw, &pre))
	limits := pre["spec"].(map[string]interface{})["resources"].(map[string]interface{})["limits"].(map[string]interface{})
	assert.Equal(t, "256Mi", limits["memory"], "pre-patch snapshots the current value")

	// Back-compat single Action populated from the persona-update step.
	assert.Equal(t, "persona-update", a.Spec.Action.Type)
	require.NotNil(t, a.Spec.Action.Patch)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`, string(a.Spec.Action.Patch.Raw))

	// Invariant holds.
	require.NoError(t, a.ValidateAutoExecutable())
}

func TestProposer_AIPath_BlastRadiusFlagsStep(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	// 256Mi -> 1024Mi is 4x, well over the 2x blast-radius cap.
	plan := &planner.RemediationPlan{
		RootCause:  "oom",
		Confidence: 0.8,
		Steps: []planner.PlannedStep{
			{Order: 1, Type: "persona-update", Description: "huge bump", Rationale: "r", Risk: "high",
				Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"1024Mi"}}}}`)},
			{Order: 2, Type: "manual", Description: "advisory note", Rationale: "untouched", Risk: "low"},
		},
	}

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{plan: plan}))

	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed, "advisory plan is still persisted")

	a := result.Action
	require.Len(t, a.Spec.Steps, 2)
	// The over-cap persona-update step is flagged advisory.
	assert.False(t, a.Spec.Steps[0].AutoExecutable, "blast-radius step flagged advisory")
	assert.Contains(t, a.Spec.Steps[0].Rationale, "blast-radius")
	// The advisory manual step is untouched.
	assert.False(t, a.Spec.Steps[1].AutoExecutable)
	assert.Equal(t, "untouched", a.Spec.Steps[1].Rationale, "advisory step rationale untouched")
	// No safe auto step remains -> back-compat Action is advisory (notification).
	assert.Equal(t, dorguv1.ActionTypeNotification, a.Spec.Action.Type)
	require.NoError(t, a.ValidateAutoExecutable())
}

func TestProposer_AIPath_DenyListSkips(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("kube-system", "coredns", "256Mi", "500m")
	incident := newTestIncident("kube-system", "oom", "coredns", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{plan: threeStepPlan()}))

	result, err := p.Propose(context.Background(), newOOMDiagnosis("kube-system", "coredns", detection.SeverityCritical), incident)
	require.NoError(t, err)
	assert.False(t, result.Proposed)
	assert.Contains(t, result.SkipReason, "deny-list")
}

func TestProposer_AIPath_FallbackOnPlannerError(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{err: errors.New("model down")}))

	// Planner errors -> fall back to the rule-based single-action path.
	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)
	assert.Empty(t, result.Action.Spec.Steps, "rule-based path produces a single Action, not Steps")
	assert.Equal(t, "persona-update", result.Action.Spec.Action.Type)
	assert.NotEqual(t, dorguv1.PlanSourceAIAnthropic, result.Action.Spec.PlanSource)
}

func TestProposer_AIPath_FallbackOnEmptyPlan(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{plan: &planner.RemediationPlan{}}))

	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)
	assert.Empty(t, result.Action.Spec.Steps, "empty plan falls back to rules")
}

func TestProposer_NoPlanner_UsesRules(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger()) // no planner

	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)
	assert.Empty(t, result.Action.Spec.Steps)
	assert.Equal(t, "persona-update", result.Action.Spec.Action.Type)
}

func TestProposer_AIPath_AllAdvisoryPlanPersists(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	// No persona-update step -> nothing auto-executable; the all-advisory plan is
	// still persisted, and the persona-wide guardrails are still evaluated.
	plan := &planner.RemediationPlan{
		RootCause:  "needs human investigation",
		Confidence: 0.6,
		Steps: []planner.PlannedStep{
			{Order: 1, Type: "manual", Description: "page on-call", Rationale: "r", Risk: "high"},
			{Order: 2, Type: "config-change", Description: "tune readiness probe", Rationale: "r", Risk: "medium"},
		},
	}

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{plan: plan}))

	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)
	require.Len(t, result.Action.Spec.Steps, 2)
	for _, s := range result.Action.Spec.Steps {
		assert.False(t, s.AutoExecutable)
	}
	assert.Equal(t, dorguv1.ActionTypeNotification, result.Action.Spec.Action.Type)
}

func TestFormatConfidence_Clamps(t *testing.T) {
	assert.Equal(t, "0.00", formatConfidence(-0.5))
	assert.Equal(t, "1.00", formatConfidence(1.5))
	assert.Equal(t, "0.88", formatConfidence(0.88))
}

func TestProposer_AIPath_InvariantNeverAutoExecsWorkload(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	// A malicious/buggy plan marks a workload-apply step as if it were a persona
	// update; the mapping must never make it auto-executable.
	plan := &planner.RemediationPlan{
		RootCause:  "x",
		Confidence: 0.5,
		Steps: []planner.PlannedStep{
			{Order: 1, Type: "workload-apply", Description: "apply deployment", Rationale: "r", Risk: "high",
				Patch: json.RawMessage(`{"spec":{"replicas":3}}`)},
			{Order: 2, Type: "persona-update", Description: "bump mem", Rationale: "r", Risk: "low",
				Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`)},
		},
	}

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{plan: plan}))

	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	for _, step := range result.Action.Spec.Steps {
		if step.AutoExecutable {
			assert.Equal(t, "persona-update", step.Type, "only persona-update may be auto-executable")
		}
	}
	require.NoError(t, result.Action.ValidateAutoExecutable())
}
