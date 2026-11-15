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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// B8 EKS QA F-5: for a crash loop, a not-Ready container or a liveness kill, the AI
// planner's plan is a memory raise plus advice. The rules did not recommend a
// resource adjustment, so the gate refused the WHOLE plan (Validate/ActionClass)
// and the user got an incident with no proposal at all, where 0.12.1 left an
// advisory notification. The persona-update the rules do not authorise is now
// dropped and the plan's advisory steps are proposed, through the gate as any plan
// is. The refusal of the first plan is still recorded and counted.

// rulesInvestigate is the rules' SuggestedAction for a crash loop: no resource change.
const rulesInvestigate = "investigate"

type planCounter struct {
	plan  *planner.RemediationPlan
	calls int
}

func (c *planCounter) PlanRemediation(context.Context, planner.RemediationContext) (*planner.RemediationPlan, error) {
	c.calls++
	return c.plan, nil
}

func memoryRaisePlusAdvice() *planner.RemediationPlan {
	return &planner.RemediationPlan{
		RootCause: "the container exits right after start", Confidence: 0.7,
		Steps: []planner.PlannedStep{
			{Order: 1, Type: dorguv1.StepTypePersonaUpdate, Description: "raise memory", Rationale: "maybe it is memory", Risk: "low",
				Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`)},
			{Order: 2, Type: dorguv1.StepTypeManual, Description: "Read the container logs for the exit reason.", Rationale: "the exit code is 1", Risk: "low"},
			{Order: 3, Type: dorguv1.StepTypeManual, Description: "Check the startup command and its dependencies.", Rationale: "it fails at start", Risk: "low"},
		},
	}
}

func TestActionClassRefusal_FallsBackToTheAdvisorySteps(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}).Build()
	cp := &planCounter{plan: memoryRaisePlusAdvice()}
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(cp))
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalCrashLoopBackOff
	diag.SuggestedAction = rulesInvestigate // the rules do not recommend a resource adjustment
	incident := newTestIncident(defaultNamespace, "crash", "my-app", "CrashLoopBackOff")

	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)

	require.True(t, result.Proposed, "the advisory steps are proposed; got skip %q (%s/%s)", result.SkipReason, result.Stage, result.Reason)
	ra := result.Action
	assert.Equal(t, dorguv1.ActionTypeNotification, ra.Spec.Action.Type, "advice, nothing to apply")
	require.Len(t, ra.Spec.Steps, 2, "the persona-update the rules do not authorise is dropped, the advice stays")
	for _, step := range ra.Spec.Steps {
		assert.Equal(t, dorguv1.StepTypeManual, step.Type)
		assert.False(t, step.AutoExecutable)
	}
	assert.Equal(t, int32(1), ra.Spec.Steps[0].Order, "steps are renumbered")
	assert.Equal(t, int32(2), ra.Spec.Steps[1].Order)

	// The refusal of the first plan is still recorded (I3): the result carries it so the
	// caller writes the condition and Event, and the gate counted the decision.
	require.NotNil(t, result.PlanRefusal, "the refused AI plan is reported")
	assert.Equal(t, gate.StageValidate, result.PlanRefusal.Stage)
	assert.Equal(t, gate.GuardrailActionClass, result.PlanRefusal.Reason)

	// The model was asked once.
	assert.Equal(t, 1, cp.calls)

	// And the fallback passed the gate like any plan: it has a gate record and is Pending.
	assert.Equal(t, "Pending", ra.Status.Phase)
	require.NotNil(t, ra.Status.Gate)
}

// A second look at the same incident does not ask the model again and does not make
// a second proposal: the first is the incident's outstanding one.
func TestActionClassRefusal_FallbackDoesNotLoop(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}).Build()
	cp := &planCounter{plan: memoryRaisePlusAdvice()}
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(cp))
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalCrashLoopBackOff
	diag.SuggestedAction = rulesInvestigate
	incident := newTestIncident(defaultNamespace, "crash", "my-app", "CrashLoopBackOff")

	first, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, first.Proposed)
	for range 3 {
		again, err := p.Propose(context.Background(), diag, incident)
		require.NoError(t, err)
		assert.False(t, again.Proposed, "the outstanding proposal answers the incident")
		assert.Equal(t, StageDeferred, again.Stage)
	}

	assert.Equal(t, 1, cp.calls, "the model is asked once, not once per cycle")
	var list dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Len(t, list.Items, 1)
}

// With nothing advisory in the plan there is nothing to fall back to: the refusal
// stands, exactly as before.
func TestActionClassRefusal_AMemoryOnlyPlanIsStillRefused(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}).Build()
	plan := memoryRaisePlusAdvice()
	plan.Steps = plan.Steps[:1]
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(&planCounter{plan: plan}))
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalCrashLoopBackOff
	diag.SuggestedAction = rulesInvestigate

	result, err := p.Propose(context.Background(), diag, newTestIncident(defaultNamespace, "crash", "my-app", "CrashLoopBackOff"))
	require.NoError(t, err)

	assert.False(t, result.Proposed)
	assert.Equal(t, gate.StageValidate, result.Stage)
	assert.Equal(t, gate.GuardrailActionClass, result.Reason)
	assert.Nil(t, result.PlanRefusal)
}

// L-6: if the second pass fails, the first refusal still stands as the result, so the
// caller records it on the incident (condition and Event) as it does for any refusal.
func TestActionClassRefusal_AFailedFallbackStillReportsTheRefusal(t *testing.T) {
	truth := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}).Build()
	pc := &planCounter{plan: memoryRaisePlusAdvice()}
	c := interceptor.NewClient(truth, interceptor.Funcs{
		List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			// The preflight and the first gate pass (which stops at Validate) are done once the
			// planner has answered; the next RemediationAction list is the fallback's gate run.
			if _, ok := list.(*dorguv1.RemediationActionList); ok && pc.calls > 0 {
				return errors.New("the API server is unavailable")
			}
			return w.List(ctx, list, opts...)
		},
	})
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(pc))
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalCrashLoopBackOff
	diag.SuggestedAction = rulesInvestigate

	result, err := p.Propose(context.Background(), diag, newTestIncident(defaultNamespace, "crash", "my-app", "CrashLoopBackOff"))
	require.NoError(t, err)

	assert.False(t, result.Proposed)
	assert.Equal(t, gate.StageValidate, result.Stage, "the ActionClass refusal is the result, so it is recorded")
	assert.Equal(t, gate.GuardrailActionClass, result.Reason)
}
