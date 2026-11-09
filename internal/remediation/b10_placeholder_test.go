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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// B10 (d), D1.1 finding 6: an AI plan for a crash loop arrived as one step whose description and
// rationale were both the word "placeholder" (the content was in the root cause), and
// `dorgu remediation diff` printed "[1] manual: placeholder". No step is emitted with
// that text: it is filled from the plan summary, or the step is dropped.

func proposeCrashLoop(t *testing.T, plan *planner.RemediationPlan) *ProposalResult {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(&planCounter{plan: plan}))
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalCrashLoopBackOff
	diag.SuggestedAction = rulesInvestigate
	incident := newTestIncident(defaultNamespace, "crash", "my-app", "CrashLoopBackOff")
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	return result
}

func assertNoPlaceholder(t *testing.T, action *dorguv1.RemediationAction) {
	t.Helper()
	raw, err := json.Marshal(action.Spec)
	require.NoError(t, err)
	assert.NotContains(t, strings.ToLower(string(raw)), "placeholder")
}

func TestPlaceholder_AStepWithOnlyPlaceholdersIsFilledFromThePlanSummary(t *testing.T) {
	const cause = "the container exits with code 1 right after start"
	result := proposeCrashLoop(t, &planner.RemediationPlan{RootCause: cause, Confidence: 0.6, Steps: []planner.PlannedStep{
		{Order: 1, Type: dorguv1.StepTypeManual, Description: "placeholder", Rationale: "placeholder", Risk: "low"},
	}})

	require.True(t, result.Proposed, "skip %q (%s/%s)", result.SkipReason, result.Stage, result.Reason)
	require.Len(t, result.Action.Spec.Steps, 1)
	assert.Contains(t, result.Action.Spec.Steps[0].Description, cause)
	assert.NotEmpty(t, result.Action.Spec.Steps[0].Rationale)
	assertNoPlaceholder(t, result.Action)
}

func TestPlaceholder_AStepIsDroppedWhenThereIsNothingToFillItWith(t *testing.T) {
	result := proposeCrashLoop(t, &planner.RemediationPlan{RootCause: "", Confidence: 0.6, Steps: []planner.PlannedStep{
		{Order: 1, Type: dorguv1.StepTypeManual, Description: "Placeholder.", Rationale: "r", Risk: "low"},
		{Order: 2, Type: dorguv1.StepTypeManual, Description: "Read the container logs.", Rationale: "the exit reason is there", Risk: "low"},
	}})

	require.True(t, result.Proposed, "skip %q (%s/%s)", result.SkipReason, result.Stage, result.Reason)
	require.Len(t, result.Action.Spec.Steps, 1)
	assert.Equal(t, "Read the container logs.", result.Action.Spec.Steps[0].Description)
	assert.Equal(t, int32(1), result.Action.Spec.Steps[0].Order, "renumbered")
	assertNoPlaceholder(t, result.Action)
}

func TestPlaceholder_ARationaleOfPlaceholderIsFilledToo(t *testing.T) {
	result := proposeCrashLoop(t, &planner.RemediationPlan{RootCause: "it fails at start", Confidence: 0.6, Steps: []planner.PlannedStep{
		{Order: 1, Type: dorguv1.StepTypeManual, Description: "Read the logs.", Rationale: " PLACEHOLDER ", Risk: "low"},
	}})

	require.True(t, result.Proposed)
	assert.Equal(t, "it fails at start", result.Action.Spec.Steps[0].Rationale)
	assertNoPlaceholder(t, result.Action)
}

// The word inside real prose is not a placeholder.
func TestPlaceholder_OnlyTheLiteralWordIsRepaired(t *testing.T) {
	steps := []dorguv1.RemediationStep{
		{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "Replace the placeholder image tag.", Rationale: "the tag is a placeholder"},
	}
	out := repairPlaceholderSteps(steps, "summary")
	assert.Equal(t, steps, out)
}

// The persist chokepoint repairs any producer, not only the AI mapper.
func TestPlaceholder_ThePersistChokepointNeverWritesOne(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())
	action := &dorguv1.RemediationAction{}
	action.Name, action.Namespace = "ra-x", defaultNamespace
	action.Spec.PlanSummary = "the real summary"
	action.Spec.Steps = []dorguv1.RemediationStep{{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "placeholder", Rationale: "placeholder"}}

	repaired, err := p.repairBeforePersist(action)

	require.NoError(t, err)
	assert.Equal(t, "the real summary", repaired.Spec.Steps[0].Description)
	assertNoPlaceholder(t, repaired)
}

func TestPlaceholder_PersistRepairEdgeCases(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())

	// A plan with no steps passes through (a legacy single-action plan).
	same, err := p.repairBeforePersist(&dorguv1.RemediationAction{})
	require.NoError(t, err)
	assert.Empty(t, same.Spec.Steps)

	// Nothing to fill a placeholder with, and nothing else in the plan: there is nothing to propose.
	_, err = p.repairBeforePersist(&dorguv1.RemediationAction{Spec: dorguv1.RemediationActionSpec{
		Steps: []dorguv1.RemediationStep{{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "placeholder"}},
	}})
	assert.ErrorIs(t, err, errOnlyPlaceholders)

	// A step that carries the persona patch is the change itself: it gets Dorgu's own words, not a drop.
	repaired := repairPlaceholderSteps([]dorguv1.RemediationStep{{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, Description: "placeholder", Patch: memPatchJSON("64Mi"),
	}}, "")
	require.Len(t, repaired, 1)
	assert.Equal(t, fallbackStepDescription, repaired[0].Description)
}

// The AI path with a plan of nothing but placeholders and no summary falls back to the rules.
func TestPlaceholder_AnAIPlanOfOnlyPlaceholdersFallsBackToTheRules(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	plan := &planner.RemediationPlan{Confidence: 0.5, Steps: []planner.PlannedStep{
		{Order: 1, Type: dorguv1.StepTypeManual, Description: "placeholder", Rationale: "placeholder", Risk: "low"},
	}}
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(&planCounter{plan: plan}))

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical),
		newTestIncident(defaultNamespace, "oom", "my-app", "OOMKilled"))

	require.NoError(t, err)
	require.True(t, result.Proposed)
	assert.Equal(t, dorguv1.PlanSourceRuleBased, result.Action.Spec.PlanSource, "the rule engine's plan, not an empty AI one")
	assertNoPlaceholder(t, result.Action)
}
