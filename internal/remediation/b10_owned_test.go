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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B10 (b), D1.1 finding 5: a plan for a workload that Helm, Argo CD, Flux or kustomize
// owns carries no auto-executable step at all, the persona-update step included, and its
// explanation no longer says "1 applied on approval".

var deployers = []string{
	dorguv1.ManagedByHelm, dorguv1.ManagedByArgoCD, dorguv1.ManagedByFlux, dorguv1.ManagedByKustomize,
}

// proposeRuleBased runs the rule engine's path against a live Deployment with the given owner.
func proposeRuleBased(t *testing.T, owner string) *dorguv1.RemediationAction {
	t.Helper()
	persona := personaWithLimits("my-app", "", "32Mi")
	deploy := ownedBy(liveDeployment("my-app",
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"}, nil), owner)
	incident := newTestIncident(defaultNamespace, "oom-"+owner, "my-app", "OOMKilled")
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed, "the proposal exists, as advice: %s", result.SkipReason)
	return result.Action
}

func assertAdviceOnly(t *testing.T, action *dorguv1.RemediationAction) {
	t.Helper()
	require.NotEmpty(t, action.Spec.Steps, "the owner still gets the advice")
	for _, step := range action.Spec.Steps {
		assert.False(t, step.AutoExecutable, "step %d (%s) must not be auto-executable on an owned workload", step.Order, step.Type)
		assert.Nil(t, step.Patch, "step %d carries a patch", step.Order)
		assert.Nil(t, step.PrePatchState, "step %d carries a pre-patch state", step.Order)
		assert.NotEqual(t, dorguv1.StepTypePersonaUpdate, step.Type, "step %d is still a persona-update", step.Order)
	}
	assert.Equal(t, dorguv1.ActionTypeNotification, action.Spec.Action.Type)
	assert.Nil(t, action.Spec.Action.Patch)
	assert.False(t, action.HasAutoApplicableChange())
	assert.NotContains(t, action.Spec.Explanation, "applied on approval", "the explanation must not promise an apply")
}

func TestB10Owned_AIPlanHasNoAutoExecutableStep(t *testing.T) {
	for _, owner := range deployers {
		t.Run(owner, func(t *testing.T) {
			action := proposeAgainst(t, owner)
			require.Equal(t, owner, action.Spec.WorkloadRef.ManagedBy)
			assertAdviceOnly(t, action)
			assert.Contains(t, action.Spec.Explanation, "advisory")
			// The value the persona step carried is not lost: the reader is told what to set, and where.
			var described strings.Builder
			for _, step := range action.Spec.Steps {
				described.WriteString(step.Description + "\n")
			}
			assert.Contains(t, described.String(), "resources.limits.memory: 64Mi")
		})
	}
}

func TestB10Owned_RuleBasedPlanHasNoAutoExecutableStep(t *testing.T) {
	for _, owner := range deployers {
		t.Run(owner, func(t *testing.T) {
			action := proposeRuleBased(t, owner)
			require.Equal(t, owner, action.Spec.WorkloadRef.ManagedBy)
			assertAdviceOnly(t, action)
			assert.Contains(t, action.Spec.Explanation, "will not change")
			assert.Contains(t, action.Spec.Steps[0].Description, "resources.limits.memory")
		})
	}
}

func TestB10Owned_UnmanagedStillGetsTheAppliedStep(t *testing.T) {
	for name, action := range map[string]*dorguv1.RemediationAction{
		"ai":   proposeAgainst(t, dorguv1.ManagedByUnmanaged),
		"rule": proposeRuleBased(t, dorguv1.ManagedByUnmanaged),
	} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, action.HasAutoApplicableChange(), "an unmanaged workload's persona step is still applied on approval")
			assert.Equal(t, dorguv1.StepTypePersonaUpdate, action.Spec.Steps[0].Type)
		})
	}
}

// The gate's own net: a Final proposal that still carries an auto-executable step on an
// owned workload is refused, so a producer that forgets to shape the plan cannot ship it.
func TestB10Owned_TheGateRefusesAnAutoStepOnAnOwnedWorkloadAtProposal(t *testing.T) {
	persona := personaWithLimits("my-app", "", "32Mi")
	deploy := ownedBy(liveDeployment("my-app", map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"}, nil), dorguv1.ManagedByHelm)
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).WithRuntimeObjects(persona, deploy).Build()
	action := proposeAgainstUnshaped(t)

	decision, err := gate.New(c, testLogger()).Run(context.Background(),
		candidateFor(action, newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical),
			newTestIncident(defaultNamespace, "oom", "my-app", "OOMKilled"), true), gate.PhaseProposal)

	require.NoError(t, err)
	stage, guardrail := decision.RefusedBy()
	assert.Equal(t, gate.StageOwnership, stage)
	assert.Equal(t, gate.GuardrailOwnerAutoStep, guardrail)
}

// proposeAgainstUnshaped is a plan exactly as the rule engine builds it, auto step included.
func proposeAgainstUnshaped(t *testing.T) *dorguv1.RemediationAction {
	t.Helper()
	return proposeRuleBased(t, dorguv1.ManagedByUnmanaged)
}

func TestB10Owned_TheAdviceNoteNamesTheOwner(t *testing.T) {
	for _, tc := range []struct {
		ref  dorguv1.WorkloadRef
		want string
	}{
		{dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByHelm}, "A Helm release owns"},
		{dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByArgoCD}, "An ArgoCD application owns"},
		{dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByFlux}, "A Flux controller owns"},
		{dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByKustomize}, "A kustomize overlay owns"},
		{dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByHelm, ManagedByDetail: `Helm release "web"`}, `Helm release "web" owns`},
	} {
		note := ownerAdviceNote(&tc.ref)
		assert.Contains(t, note, tc.want)
		assert.Contains(t, note, "will not change its persona or the Deployment")
	}
}

func TestB10Owned_ChangesOfAPatchIsEmptyForNoPatch(t *testing.T) {
	assert.Nil(t, changesOfPatch(nil))
	assert.Nil(t, changesOfPatch(&apiextensionsv1.JSON{}))
}

func TestB10Owned_UnknownOrUnresolvedOwnershipIsNotShaped(t *testing.T) {
	for _, ref := range []*dorguv1.WorkloadRef{nil, {ManagedBy: dorguv1.ManagedByUnknown}, {ManagedBy: dorguv1.ManagedByUnmanaged}} {
		action := &dorguv1.RemediationAction{Spec: dorguv1.RemediationActionSpec{Steps: []dorguv1.RemediationStep{
			{Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true, Patch: memPatchJSON("64Mi")},
		}}}
		assert.Zero(t, adviseForOwner(action, ref))
		assert.True(t, action.Spec.Steps[0].AutoExecutable)
	}
	assert.Zero(t, adviseForOwner(nil, &dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByHelm}))
}

func memPatchJSON(v string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"` + v + `"}}}}`)}
}
