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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// C6b (review F-8): after a person's ticketed rollback completes, the planner must not
// offer the rolled-back value again, on either path, while a different value is still
// allowed. The world below is the memhog scenario after the fact: Dorgu raised memory
// from 64Mi to 128Mi, a person rolled it back, and the workload holds 64Mi again.

const (
	c6bOriginal = "ra-original"
	c6bRollback = "ra-rollback"
	c6bRolled   = "128Mi"
	c6bBefore   = "64Mi"
	c6bField    = "spec.resources.limits.memory"
)

func c6bPatch(memory string) *apiextensionsv1.JSON {
	return mustJSON(`{"spec":{"resources":{"limits":{"memory":"` + memory + `"}}}}`)
}

func c6bPersonaRef() dorguv1.PersonaReference {
	return dorguv1.PersonaReference{Kind: kindApplicationPersona, Name: groundedPersona, Namespace: defaultNamespace}
}

// c6bOriginalAction is the action that raised memory and Completed.
func c6bOriginalAction() *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: c6bOriginal, Namespace: defaultNamespace, UID: "ra-uid"},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef:  c6bPersonaRef(),
			IncidentRef: dorguv1.IncidentReference{Name: "im-earlier", Namespace: defaultNamespace},
			Provenance: &dorguv1.RemediationProvenance{
				Proposal: "p", Source: dorguv1.ProposalSourceDorguPlanner,
				Proposer: dorguv1.ProposerIdentity{Username: "system:serviceaccount:dorgu-system:dorgu-operator"},
			},
		},
		Status: dorguv1.RemediationActionStatus{
			Phase: "Completed",
			Gate:  &dorguv1.GateStatus{Phase: "ApprovalRecheck", Result: "Pass", PrePatchState: c6bPatch(c6bBefore)},
			ApplyTicket: &dorguv1.ApplyTicket{
				WorkloadUID: "uid-1", Container: groundedPersona, Patch: c6bPatch(c6bRolled),
			},
		},
	}
}

// c6bRollbackAction is a person's rollback of it, Completed.
func c6bRollbackAction(mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name: c6bRollback, Namespace: defaultNamespace,
			Annotations: map[string]string{dorguv1.AnnotationRollbackOf: c6bOriginal},
		},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef:  c6bPersonaRef(),
			IncidentRef: dorguv1.IncidentReference{Name: "im-earlier", Namespace: defaultNamespace},
			Provenance: &dorguv1.RemediationProvenance{
				Proposal: "p-rb", Source: dorguv1.ProposalSourceRollback, RequestedBy: "carol@example.com",
				Proposer: dorguv1.ProposerIdentity{Username: "system:serviceaccount:dorgu-system:dorgu-operator"},
			},
		},
		Status: dorguv1.RemediationActionStatus{
			Phase: "Completed",
			Gate:  &dorguv1.GateStatus{Phase: "ApprovalRecheck", Result: "Pass", PrePatchState: c6bPatch(c6bRolled)},
			ApplyTicket: &dorguv1.ApplyTicket{
				WorkloadUID: "uid-1", Container: groundedPersona, Patch: c6bPatch(c6bBefore),
			},
		},
	}
	for _, m := range mutate {
		m(ra)
	}
	return ra
}

// c6bWorld is the memhog world after the rollback: persona and Deployment at 64Mi.
func c6bWorld(t *testing.T, actions ...client.Object) (*Proposer, *dorguv1.IncidentMemory, client.Client) {
	t.Helper()
	return c6bWorldAt(t, c6bBefore, actions...)
}

// c6bWorldAt is the same world with the persona and the Deployment holding memory.
func c6bWorldAt(t *testing.T, memory string, actions ...client.Object) (*Proposer, *dorguv1.IncidentMemory, client.Client) {
	t.Helper()
	persona := personaWithLimits(groundedPersona, "200m", memory)
	deploy := liveDeployment(groundedPersona,
		map[corev1.ResourceName]string{corev1.ResourceMemory: memory, corev1.ResourceCPU: "200m"},
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi", corev1.ResourceCPU: "50m"})
	incident := newTestIncident(defaultNamespace, "oom", groundedPersona, "OOMKilled")

	objs := append([]client.Object{persona, deploy}, actions...)
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).WithObjects(objs...).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}).Build()
	return NewProposer(c, gate.New(c, testLogger(), gate.WithReader(c)), testLogger()), incident, c
}

func c6bRolledBackWorld(t *testing.T) (*Proposer, *dorguv1.IncidentMemory, client.Client) {
	return c6bWorld(t, c6bOriginalAction(), c6bRollbackAction())
}

// c6bPatches is every persona patch on an action, as text, for "is the value anywhere".
func c6bPatches(a *dorguv1.RemediationAction) string {
	var out []byte
	for i := range a.Spec.Steps {
		if p := a.Spec.Steps[i].Patch; p != nil {
			out = append(out, p.Raw...)
		}
	}
	if p := a.Spec.Action.Patch; p != nil {
		out = append(out, p.Raw...)
	}
	return string(out)
}

func c6bActionCount(t *testing.T, c client.Client) int {
	t.Helper()
	var list dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &list))
	return len(list.Items)
}

func TestC6b_RuleBasedPath_DoesNotOfferTheRolledBackValueAgain(t *testing.T) {
	p, incident, c := c6bRolledBackWorld(t)
	before := c6bActionCount(t, c)

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.False(t, result.Proposed, "the same raise must not be proposed again")
	assert.Equal(t, gate.StageGuardrails, result.Stage)
	assert.Equal(t, gate.GuardrailRolledBackValue, result.Reason)
	assert.Contains(t, result.SkipReason, "rolled back by a person", "the incident says why, in Dorgu's own words")
	assert.Contains(t, result.SkipReason, c6bRolled)
	assert.Contains(t, result.SkipReason, c6bRollback)
	assert.Equal(t, before, c6bActionCount(t, c), "no action is created for a plan that applies nothing")
}

func TestC6b_RuleBasedPath_ADifferentRaiseIsStillProposed(t *testing.T) {
	p, incident, _ := c6bRolledBackWorld(t)

	// Warning severity sizes a smaller raise: 64Mi x 1.5 = 96Mi, not the 128Mi a person undid.
	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityWarning), incident)

	require.NoError(t, err)
	require.True(t, result.Proposed, "refused: %s", result.SkipReason)
	assert.True(t, result.Action.HasAutoApplicableChange())
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"96Mi"}}}}`, c6bPatches(result.Action))
}

func TestC6b_WithoutARollbackTheSameRaiseIsProposed(t *testing.T) {
	// Control for the tests above: the original action alone, however it ended, withholds nothing.
	p, incident, _ := c6bWorld(t, c6bOriginalAction())

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.True(t, result.Proposed, "refused: %s", result.SkipReason)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`, c6bPatches(result.Action))
}

func TestC6b_AForgedRecordDoesNotWithholdTheValue(t *testing.T) {
	forgeries := map[string]func(*dorguv1.RemediationAction){
		"the annotation on an action that is not a rollback": func(ra *dorguv1.RemediationAction) { ra.Spec.Provenance = nil },
		"a rollback that never completed":                    func(ra *dorguv1.RemediationAction) { ra.Status.Phase = "Pending" },
		"a rollback that failed":                             func(ra *dorguv1.RemediationAction) { ra.Status.Phase = "Failed" },
		"a rollback with no ticket":                          func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket = nil },
	}
	for name, forge := range forgeries {
		t.Run(name, func(t *testing.T) {
			p, incident, _ := c6bWorld(t, c6bOriginalAction(), c6bRollbackAction(forge))

			result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

			require.NoError(t, err)
			// A Pending or Failed sibling can hold the persona for its own reasons; what
			// matters is that the rolled-back guardrail did not rule.
			assert.NotEqual(t, gate.GuardrailRolledBackValue, result.Reason, result.SkipReason)
		})
	}
}

func c6bAIPlan(steps ...planner.PlannedStep) *planner.RemediationPlan {
	return &planner.RemediationPlan{RootCause: "memory limit too low", Confidence: 0.86, Steps: steps}
}

func c6bPersonaStep(memory string) planner.PlannedStep {
	return planner.PlannedStep{
		Order: 1, Type: "persona-update", Risk: "low",
		Description: "Raise the memory limit to " + memory, Rationale: "the container needs " + memory,
		Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"` + memory + `"}}}}`),
	}
}

func c6bAdviceStep(order int32, text string) planner.PlannedStep {
	return planner.PlannedStep{Order: order, Type: "manual", Description: text, Rationale: "to confirm the cause", Risk: "low"}
}

func TestC6b_AIPath_AModelPlanThatOnlyRepeatsItIsRefused(t *testing.T) {
	p, incident, c := c6bRolledBackWorld(t)
	p.planner = &stubPlanner{plan: c6bAIPlan(c6bPersonaStep(c6bRolled))}
	before := c6bActionCount(t, c)

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.False(t, result.Proposed)
	assert.Equal(t, gate.GuardrailRolledBackValue, result.Reason)
	assert.Equal(t, before, c6bActionCount(t, c))
}

func TestC6b_AIPath_TheValueIsWithdrawnAndTheAdviceStaysWithAnExplanation(t *testing.T) {
	p, incident, _ := c6bRolledBackWorld(t)
	p.planner = &stubPlanner{plan: c6bAIPlan(
		c6bPersonaStep(c6bRolled),
		c6bAdviceStep(2, "Check the logs, then raise memory to "+c6bRolled+" if the heap is still growing."),
		c6bAdviceStep(3, "Watch the container for ten minutes."),
	)}

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.True(t, result.Proposed, "refused: %s", result.SkipReason)
	a := result.Action

	assert.NotContains(t, c6bPatches(a), c6bRolled, "no patch offers the value")
	assert.False(t, a.HasAutoApplicableChange(), "nothing here is for the operator to apply")
	assert.Contains(t, a.Spec.Explanation, "rolled back by a person", "the proposal says why, in Dorgu's words")
	assert.Contains(t, a.Spec.Explanation, c6bRollback)

	step := a.Spec.Steps[0]
	require.Len(t, step.Safety, 1)
	assert.Equal(t, dorguv1.SafetyVerdictRejected, step.Safety[0].Verdict)
	assert.Equal(t, c6bRolled, step.Safety[0].Requested)
	assert.Contains(t, step.Description, "rolled back by a person")
	assert.False(t, step.AutoExecutable)

	// The model's advice that names the value is redacted by the same scrub every
	// refused value gets; the sentence of advice that does not name it stays.
	assert.NotContains(t, a.Spec.Steps[1].Description, c6bRolled)
	assert.Contains(t, a.Spec.Steps[2].Description, "Watch the container")
}

func TestC6b_AIPath_APlanWithNoPatchIsNotGivenTheRuleEnginesRolledBackValue(t *testing.T) {
	// The clean-room shape: advice and a patchless persona-update. Dorgu used to size
	// the patch itself (CR-01), and the number it would size is the one a person undid.
	p, incident, _ := c6bRolledBackWorld(t)
	p.planner = &stubPlanner{plan: cleanRoomOOMPlan()}

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.True(t, result.Proposed, "refused: %s", result.SkipReason)
	a := result.Action
	assert.NotContains(t, c6bPatches(a), c6bRolled)
	assert.False(t, a.HasAutoApplicableChange())
	assert.Contains(t, a.Spec.Explanation, "rolled back by a person")
	assert.Contains(t, a.Spec.Explanation, c6bRolled, "Dorgu names the value in its own sentence")
}

func TestC6b_AIPath_ADifferentValueIsStillAllowed(t *testing.T) {
	p, incident, _ := c6bRolledBackWorld(t)
	p.planner = &stubPlanner{plan: c6bAIPlan(c6bPersonaStep("96Mi"), c6bAdviceStep(2, "Watch the container."))}

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.True(t, result.Proposed, "refused: %s", result.SkipReason)
	require.True(t, result.Action.HasAutoApplicableChange())
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"96Mi"}}}}`, c6bPatches(result.Action))
	assert.NotContains(t, result.Action.Spec.Explanation, "rolled back by a person")
}

func TestC6b_AIPath_TheSameBytesInAnotherNotationAreTheSameValue(t *testing.T) {
	p, incident, _ := c6bRolledBackWorld(t)
	p.planner = &stubPlanner{plan: c6bAIPlan(c6bPersonaStep("134217728"), c6bAdviceStep(2, "Watch the container."))}

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.True(t, result.Proposed, "refused: %s", result.SkipReason)
	assert.False(t, result.Action.HasAutoApplicableChange(), "134217728 is 128Mi")
}

func c6bCPUStep() planner.PlannedStep {
	return planner.PlannedStep{
		Order: 2, Type: "persona-update", Risk: "low", Description: "Raise the CPU limit", Rationale: "more headroom",
		Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"cpu":"300m"}}}}`),
	}
}

func TestC6b_AIPath_AWithheldMemoryStepDoesNotLeaveAnAutoExecutableCPUOnlyProposal(t *testing.T) {
	// Review F-1: an OOM incident whose memory step is withheld must not become a CPU-only
	// proposal a person is asked to approve as "the fix".
	p, incident, c := c6bRolledBackWorld(t)
	p.planner = &stubPlanner{plan: c6bAIPlan(c6bPersonaStep(c6bRolled), c6bCPUStep())}
	before := c6bActionCount(t, c)

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.False(t, result.Proposed, "a CPU change was offered for an out-of-memory incident: %s", c6bPatchesOf(result))
	assert.Equal(t, gate.StageGuardrails, result.Stage)
	assert.Equal(t, gate.GuardrailRolledBackValue, result.Reason)
	assert.Contains(t, result.SkipReason, "rolled back by a person")
	assert.Contains(t, result.SkipReason, c6bRolled)
	assert.Equal(t, before, c6bActionCount(t, c), "nothing is recorded for a plan that is refused")
}

func c6bPatchesOf(r *ProposalResult) string {
	if r == nil || r.Action == nil {
		return ""
	}
	return c6bPatches(r.Action)
}

func TestC6b_AIPath_TheSameValueFromAnotherStartingPointIsStillProposed(t *testing.T) {
	// Review F-2: a person rolled 64Mi to 128Mi back. At 100Mi the same number is a raise
	// of 1.28x, another decision, and it is held to the normal gate only.
	p, incident, _ := c6bWorldAt(t, "100Mi", c6bOriginalAction(), c6bRollbackAction())
	p.planner = &stubPlanner{plan: c6bAIPlan(c6bPersonaStep(c6bRolled), c6bAdviceStep(2, "Watch the container."))}

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.True(t, result.Proposed, "refused: %s", result.SkipReason)
	require.True(t, result.Action.HasAutoApplicableChange())
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`, c6bPatches(result.Action))
	assert.NotContains(t, result.Action.Spec.Explanation, "rolled back by a person")
}

func TestC6b_MatchRolledBackNeedsTheSameMove(t *testing.T) {
	rolled := []gate.RolledBackValue{{Field: c6bField, From: c6bBefore, Value: c6bRolled, Action: "a", Rollback: "b"}}
	patch := []byte(`{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`)

	assert.Len(t, matchRolledBack(rolled, patch, map[string]string{c6bField: "64Mi"}), 1)
	assert.Empty(t, matchRolledBack(rolled, patch, map[string]string{c6bField: "100Mi"}))
	assert.Empty(t, matchRolledBack(rolled, patch, nil), "no starting point known is not the same move")
}

func TestC6b_PlanningBaselinesAreTheLiveValueElseThePersonas(t *testing.T) {
	persona := personaWithLimits(groundedPersona, "200m", "96Mi")
	patch := []byte(`{"spec":{"resources":{"limits":{"memory":"128Mi"}}}}`)

	// No resolved workload: the persona's value is the baseline.
	assert.Equal(t, map[string]string{c6bField: "96Mi"}, planningBaselines(patch, persona, nil))

	// A resolved workload that sets the limit: the live value wins over the persona's.
	ref := &dorguv1.WorkloadRef{
		Kind: "Deployment", Name: groundedPersona, Namespace: defaultNamespace, Container: groundedPersona,
		ObservedResources: &dorguv1.ObservedResources{Limits: &dorguv1.ResourceValues{Memory: "64Mi"}},
	}
	assert.Equal(t, map[string]string{c6bField: "64Mi"}, planningBaselines(patch, persona, ref))
}

func TestC6b_RefuseRolledBackOnlyFix(t *testing.T) {
	held := []gate.RolledBackValue{{Field: c6bField, From: c6bBefore, Value: c6bRolled, Action: c6bOriginal, Rollback: c6bRollback}}
	step := func(patch string, auto bool) dorguv1.RemediationStep {
		return dorguv1.RemediationStep{
			Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: auto, Patch: mustJSON(patch),
		}
	}
	advice := dorguv1.RemediationStep{Order: 2, ID: "step-2", Type: dorguv1.StepTypeManual, Description: "read the logs"}
	cpu := `{"spec":{"resources":{"limits":{"cpu":"300m"}}}}`
	mem := `{"spec":{"resources":{"limits":{"memory":"96Mi"}}}}`
	action := func(steps ...dorguv1.RemediationStep) *dorguv1.RemediationAction {
		return &dorguv1.RemediationAction{Spec: dorguv1.RemediationActionSpec{Steps: steps}}
	}

	t.Run("an unrelated change that would still apply is refused with the person's sentence", func(t *testing.T) {
		got := refuseRolledBackOnlyFix(action(step(cpu, true)), nil, held, c6bField)
		require.NotNil(t, got)
		assert.Equal(t, gate.GuardrailRolledBackValue, got.Reason)
		assert.Equal(t, gate.StageGuardrails, got.Stage)
		assert.Contains(t, got.SkipReason, gate.RolledBackMessage(held[0]))
		assert.Contains(t, got.SkipReason, "none of it is proposed")
	})
	t.Run("advice alone is the considered outcome", func(t *testing.T) {
		assert.Nil(t, refuseRolledBackOnlyFix(action(advice), nil, held, c6bField))
	})
	t.Run("a different memory value still answers the incident", func(t *testing.T) {
		assert.Nil(t, refuseRolledBackOnlyFix(action(step(mem, true)), nil, held, c6bField))
	})
	t.Run("nothing was withheld", func(t *testing.T) {
		assert.Nil(t, refuseRolledBackOnlyFix(action(step(cpu, true)), nil, nil, c6bField))
	})
	t.Run("no diagnosed field", func(t *testing.T) {
		assert.Nil(t, refuseRolledBackOnlyFix(action(step(cpu, true)), nil, held, ""))
	})
}
