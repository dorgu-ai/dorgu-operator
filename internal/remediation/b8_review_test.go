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

// B8 review (pr61), F-5: the fallback after a Validate/ActionClass refusal.

func fallbackProposer(t *testing.T, plan *planner.RemediationPlan) *Proposer {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	return NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(&planCounter{plan: plan}))
}

func propose(t *testing.T, p *Proposer) *ProposalResult {
	t.Helper()
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalCrashLoopBackOff
	diag.SuggestedAction = rulesInvestigate
	res, err := p.Propose(context.Background(), diag, newTestIncident(defaultNamespace, "crash", "my-app", "CrashLoopBackOff"))
	require.NoError(t, err)
	return res
}

// No patch may survive the drop, whatever shape the model gave the persona-update
// (nested, dotted key, a patch smuggled on an advisory step) and whatever type label
// the steps carry.
func TestReview_NoPatchSurvivesTheDrop(t *testing.T) {
	plan := &planner.RemediationPlan{
		RootCause: "exits at start", Confidence: 0.6,
		Steps: []planner.PlannedStep{
			{Order: 1, Type: dorguv1.StepTypePersonaUpdate, Description: "raise", Rationale: "r", Risk: "low",
				Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"384Mi"}},"resources.requests.memory":"300Mi","policies":{"selfHealing":{"mode":"autonomous"}}}}`)},
			{Order: 2, Type: dorguv1.StepTypeManual, Description: "read the logs", Rationale: "r", Risk: "low",
				Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"2Gi"}}}}`)},
			{Order: 3, Type: dorguv1.StepTypeConfigChange, Description: "check env", Rationale: "r", Risk: "low",
				Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"cpu":"8"}}}}`)},
		},
	}
	p := fallbackProposer(t, plan)
	res := propose(t, p)

	require.True(t, res.Proposed, "got skip %q (%s/%s)", res.SkipReason, res.Stage, res.Reason)
	ra := res.Action
	assert.Equal(t, dorguv1.ActionTypeNotification, ra.Spec.Action.Type)
	assert.Nil(t, ra.Spec.Action.Patch)
	assert.Nil(t, ra.Spec.Action.PrePatchState)
	for _, s := range ra.Spec.Steps {
		assert.Nil(t, s.Patch, "step %d (%s) carries a patch", s.Order, s.Type)
		assert.Nil(t, s.PrePatchState)
		assert.False(t, s.AutoExecutable)
	}
	raw, _ := json.Marshal(ra.Spec)
	for _, leaked := range []string{"384Mi", "300Mi", "2Gi", "autonomous"} {
		assert.NotContains(t, string(raw), leaked, "a value from a patch survived into the proposal")
	}
}

// The model's prose for the advisory steps can still advertise the change the gate
// refused ("after raising memory to 384Mi, ..."). The persisted RA is what a human
// approves from: CR5-01 says no value a guardrail refused may survive into a surface a
// client renders.
func TestReview_DroppedPersonaValueIsNotAdvertisedByTheAdvisoryText(t *testing.T) {
	plan := &planner.RemediationPlan{
		RootCause: "memory raised to 384Mi did not help", Confidence: 0.6,
		Steps: []planner.PlannedStep{
			{Order: 1, Type: dorguv1.StepTypePersonaUpdate, Description: "raise memory to 384Mi", Rationale: "r", Risk: "low",
				Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`)},
			{Order: 2, Type: dorguv1.StepTypeManual, Description: "With memory already raised to 384Mi, read the logs.",
				Rationale: "the persona now says 384Mi", Risk: "low"},
		},
	}
	p := fallbackProposer(t, plan)
	res := propose(t, p)

	require.True(t, res.Proposed)
	raw, _ := json.Marshal(res.Action.Spec)
	assert.False(t, strings.Contains(string(raw), "384Mi"),
		"the persona change was dropped, yet the proposal still tells the reader memory is 384Mi: %s", raw)
}

// A command on an advisory step is never reached by the first gate pass (ActionClass
// refuses before the command rules run). The second pass must apply them in full.
func TestReview_AdvisoryCommandsAreRuledOnAfterTheDrop(t *testing.T) {
	for name, cmd := range map[string]string{
		"chained":      "kubectl logs deploy/my-app; kubectl delete deploy my-app",
		"substitution": "kubectl logs $(kubectl get pods -o name)",
		"pipe":         "kubectl get secret x -o yaml | base64 -d",
		"write-verb":   "kubectl delete deployment my-app -n " + defaultNamespace,
	} {
		t.Run(name, func(t *testing.T) {
			plan := memoryRaisePlusAdvice()
			plan.Steps[1].Command = cmd
			p := fallbackProposer(t, plan)
			res := propose(t, p)
			if !res.Proposed {
				return // refused: fine
			}
			for _, s := range res.Action.Spec.Steps {
				assert.NotContains(t, s.Command, ";", "step %d", s.Order)
				assert.NotContains(t, s.Command, "$(")
				assert.NotContains(t, s.Command, "|")
				assert.NotContains(t, s.Command, "delete")
			}
		})
	}
}
