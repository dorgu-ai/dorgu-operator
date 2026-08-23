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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// ownedBy stamps the ownership evidence a given system leaves on a Deployment.
func ownedBy(deploy *appsv1.Deployment, owner string) *appsv1.Deployment {
	out := deploy.DeepCopy()
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	switch owner {
	case dorguv1.ManagedByHelm:
		out.Annotations["meta.helm.sh/release-name"] = "frontend"
		out.Annotations["meta.helm.sh/release-namespace"] = defaultNamespace
	case dorguv1.ManagedByArgoCD:
		out.Labels["argocd.argoproj.io/instance"] = "frontend"
	case dorguv1.ManagedByFlux:
		out.Labels["kustomize.toolkit.fluxcd.io/name"] = "apps"
	case dorguv1.ManagedByKustomize:
		out.Labels["app.kubernetes.io/managed-by"] = "kustomize"
	}
	return out
}

// memoryFixPlan is a plan whose advisory step patches the Deployment directly,
// which is the shape that broke `helm upgrade` in the clean room.
func memoryFixPlan() *planner.RemediationPlan {
	return &planner.RemediationPlan{
		RootCause:  "the container is OOMKilled at its 32Mi memory limit",
		Confidence: 0.9,
		Steps: []planner.PlannedStep{
			{
				Order:       1,
				Type:        "persona-update",
				Description: "Record the higher memory limit in the persona",
				Rationale:   "keep desired state in sync",
				Risk:        "low",
				Patch:       json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"64Mi"}}}}`),
			},
			{
				Order:       2,
				Type:        "workload-apply",
				Description: "Raise the memory limit on the Deployment to 64Mi",
				Rationale:   "the pod needs headroom",
				Risk:        "medium",
				Command:     "kubectl patch deployment my-app -n default --type=json -p []",
			},
			{
				Order:       3,
				Type:        "manual",
				Description: "Watch the rollout",
				Rationale:   "confirm the pod stays up",
				Risk:        "low",
				Command:     "kubectl rollout status deployment/my-app -n default",
			},
		},
	}
}

// proposeAgainst runs the AI path against a live Deployment with the given
// owner and returns the persisted action.
func proposeAgainst(t *testing.T, owner string) *dorguv1.RemediationAction {
	t.Helper()

	persona := personaWithLimits("my-app", "", "32Mi")
	deploy := ownedBy(liveDeployment("my-app",
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"}, nil), owner)
	incident := newTestIncident(defaultNamespace, "oom-"+owner, "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	p := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger(),
		WithPlanner(&stubPlanner{plan: memoryFixPlan()}))

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)
	return result.Action
}

// TestProposer_NoWorkloadWriteCommandForOwnedWorkload is F-02: a plan for an
// owned Deployment must never hand the reader a command that patches it. That
// command is what takes field ownership and makes the next `helm upgrade` fail
// outright rather than merely revert the fix.
func TestProposer_NoWorkloadWriteCommandForOwnedWorkload(t *testing.T) {
	for _, owner := range []string{
		dorguv1.ManagedByHelm,
		dorguv1.ManagedByArgoCD,
		dorguv1.ManagedByFlux,
		dorguv1.ManagedByKustomize,
	} {
		t.Run(owner, func(t *testing.T) {
			action := proposeAgainst(t, owner)
			require.Equal(t, owner, action.Spec.WorkloadRef.ManagedBy)

			for _, step := range action.Spec.Steps {
				assert.NotContains(t, step.Command, "kubectl patch",
					"step %d still hands over a workload patch", step.Order)
				assert.False(t, writesWorkload(step.Command),
					"step %d command writes to an owned workload: %q", step.Order, step.Command)
			}

			// Read-only commands stay: they are useful and harmless.
			assert.Equal(t, "kubectl rollout status deployment/my-app -n default",
				action.Spec.Steps[2].Command)
		})
	}
}

// TestProposer_UnmanagedKeepsTheDirectCommand is the other half of the
// contract: where nothing reconciles the workload, the direct command is the
// right instruction and must survive.
func TestProposer_UnmanagedKeepsTheDirectCommand(t *testing.T) {
	action := proposeAgainst(t, dorguv1.ManagedByUnmanaged)
	require.Equal(t, dorguv1.ManagedByUnmanaged, action.Spec.WorkloadRef.ManagedBy)
	assert.Equal(t, "kubectl patch deployment my-app -n default --type=json -p []",
		action.Spec.Steps[1].Command)
}

// TestProposer_PlanTextDiffersByOwner checks the moat: the same incident
// produces a different, owner-correct instruction for each source of truth.
func TestProposer_PlanTextDiffersByOwner(t *testing.T) {
	tests := []struct {
		owner       string
		wantInStep  []string
		wantInWhy   string
		wantMissing string
	}{
		{
			owner:       dorguv1.ManagedByHelm,
			wantInStep:  []string{"resources.limits.memory: 64Mi", `Helm release "frontend"`, "helm upgrade"},
			wantInWhy:   "field-manager conflict",
			wantMissing: "kubectl patch",
		},
		{
			owner:       dorguv1.ManagedByArgoCD,
			wantInStep:  []string{"resources.limits.memory: 64Mi", "Git manifests", `ArgoCD application "frontend"`, "sync"},
			wantInWhy:   "reverted on the next sync",
			wantMissing: "helm upgrade",
		},
		{
			owner:       dorguv1.ManagedByFlux,
			wantInStep:  []string{"resources.limits.memory: 64Mi", "Git source", `Flux Kustomization "apps"`, "reconcile"},
			wantInWhy:   "reverted on the next reconciliation",
			wantMissing: "helm upgrade",
		},
		{
			owner:       dorguv1.ManagedByKustomize,
			wantInStep:  []string{"resources.limits.memory: 64Mi", "kustomize overlay", "re-apply"},
			wantInWhy:   "overwritten the next time the overlay is applied",
			wantMissing: "helm upgrade",
		},
	}

	seen := make(map[string]bool)
	for _, tc := range tests {
		t.Run(tc.owner, func(t *testing.T) {
			action := proposeAgainst(t, tc.owner)
			step := action.Spec.Steps[1]

			for _, want := range tc.wantInStep {
				assert.Contains(t, step.Description, want)
			}
			assert.Contains(t, step.Rationale, tc.wantInWhy)
			assert.NotContains(t, step.Description, tc.wantMissing)
			assert.Contains(t, action.Spec.PlanSummary, tc.wantInWhy,
				"the plan summary must name the owner constraint too")

			assert.False(t, seen[step.Description], "each owner gets its own instruction")
			seen[step.Description] = true
		})
	}
}

// TestProposer_OwnershipDoesNotTouchPersonaSteps is the distinction that is
// easiest to get wrong: the operator patching the ApplicationPersona is always
// safe, so ownership must not demote it. Only the CLI patching the Deployment
// is gated.
func TestProposer_OwnershipDoesNotTouchPersonaSteps(t *testing.T) {
	action := proposeAgainst(t, dorguv1.ManagedByHelm)

	personaStep := action.Spec.Steps[0]
	require.Equal(t, dorguv1.StepTypePersonaUpdate, personaStep.Type)
	assert.True(t, personaStep.AutoExecutable,
		"a Helm-owned workload does not stop the operator writing its own persona")
	require.NotNil(t, personaStep.Patch)
	assert.Contains(t, string(personaStep.Patch.Raw), "64Mi")
	assert.Equal(t, dorguv1.ActionTypePersonaUpdate, action.Spec.Action.Type)
}

// TestProposer_AIPath_CapIsRelativeToLiveWorkload is F-04 on the AI path: a
// plan sized against a stale persona is measured against the running container,
// so a 6x jump is demoted to advisory instead of being applied as "within 2x".
func TestProposer_AIPath_CapIsRelativeToLiveWorkload(t *testing.T) {
	persona := personaWithLimits("my-app", "", "96Mi")
	deploy := liveDeployment("my-app",
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"}, nil)
	incident := newTestIncident(defaultNamespace, "oom-cap", "my-app", "OOMKilled")

	plan := &planner.RemediationPlan{
		RootCause:  "OOMKilled",
		Confidence: 0.9,
		Steps: []planner.PlannedStep{{
			Order:       1,
			Type:        "persona-update",
			Description: "Raise the memory limit to 192Mi",
			Rationale:   "twice the persona's 96Mi",
			Risk:        "medium",
			Patch:       json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"192Mi"}}}}`),
		}},
	}

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	p := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger(), WithPlanner(&stubPlanner{plan: plan}))
	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	step := result.Action.Spec.Steps[0]
	assert.False(t, step.AutoExecutable,
		"192Mi is 6x the live 32Mi, so the blast-radius guardrail must catch it")
	assert.Contains(t, step.Rationale, "blast-radius")
	assert.Equal(t, dorguv1.ActionTypeNotification, result.Action.Spec.Action.Type,
		"nothing auto-applicable survives, so the legacy action is advisory")
}

// TestProposer_AIPath_DropsPatchKeysTheWorkloadLacks is F-05 on the AI path: the
// model bundles a CPU limit into a memory fix, and the workload has no CPU
// limit, so the CPU leaf is dropped and said out loud.
func TestProposer_AIPath_DropsPatchKeysTheWorkloadLacks(t *testing.T) {
	persona := personaWithLimits("my-app", "50m", "32Mi")
	deploy := liveDeployment("my-app",
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"},
		map[corev1.ResourceName]string{corev1.ResourceCPU: "25m"})
	incident := newTestIncident(defaultNamespace, "oom-f05", "my-app", "OOMKilled")

	plan := &planner.RemediationPlan{
		RootCause:  "OOMKilled",
		Confidence: 0.9,
		Steps: []planner.PlannedStep{{
			Order:       1,
			Type:        "persona-update",
			Description: "Raise memory and set a CPU limit",
			Rationale:   "the persona records a CPU limit of 50m",
			Risk:        "medium",
			Patch:       json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"64Mi","cpu":"50m"}}}}`),
		}},
	}

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	p := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger(), WithPlanner(&stubPlanner{plan: plan}))
	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	step := result.Action.Spec.Steps[0]
	require.NotNil(t, step.Patch)
	assert.Equal(t, "64Mi", patchLimit(t, step.Patch.Raw, "memory"))
	assert.Empty(t, patchLimit(t, step.Patch.Raw, "cpu"),
		"the container has no CPU limit, so the fix must not introduce one")
	assert.Contains(t, step.Rationale, "Left out")
	assert.True(t, step.AutoExecutable, "the memory half of the fix is still applied")
}

// TestProposer_AIPath_EmptiedPatchBecomesAdvisory covers the case where every
// key in a patch targets a field the workload lacks: there is nothing left to
// apply, so the step must not claim it will apply something.
func TestProposer_AIPath_EmptiedPatchBecomesAdvisory(t *testing.T) {
	persona := personaWithLimits("my-app", "50m", "32Mi")
	deploy := liveDeployment("my-app", nil,
		map[corev1.ResourceName]string{corev1.ResourceCPU: "25m"})
	incident := newTestIncident(defaultNamespace, "cpu-f05", "my-app", "CPUSaturationHigh")

	plan := &planner.RemediationPlan{
		RootCause:  "CPU saturation",
		Confidence: 0.8,
		Steps: []planner.PlannedStep{{
			Order:       1,
			Type:        "persona-update",
			Description: "Raise the CPU limit",
			Rationale:   "throttling",
			Risk:        "medium",
			Patch:       json.RawMessage(`{"spec":{"resources":{"limits":{"cpu":"100m"}}}}`),
		}},
	}

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	p := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger(), WithPlanner(&stubPlanner{plan: plan}))
	result, err := p.Propose(context.Background(),
		newCPUDiagnosis(defaultNamespace, "my-app", detection.SeverityWarning), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	step := result.Action.Spec.Steps[0]
	assert.Nil(t, step.Patch)
	assert.False(t, step.AutoExecutable)
	assert.Contains(t, step.Rationale, "Left out")
	assert.True(t, strings.Contains(result.Action.Spec.Explanation, "advisory"))
}

func TestWritesWorkload(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"kubectl patch deployment web -n apps --type=merge -p {}", true},
		{"kubectl set image deployment/web web=nginx:1.27 -n apps", true},
		{"kubectl apply -f deploy.yaml", true},
		{"kubectl scale deployment/web --replicas=3", true},
		{"kubectl delete pod web-abc", true},
		{"kubectl rollout undo deployment/web -n apps", true},
		{"kubectl rollout restart deployment/web -n apps", true},
		{"kubectl -n apps patch deployment web -p {}", true},
		{"kubectl rollout status deployment/web -n apps", false},
		{"kubectl rollout history deployment/web -n apps", false},
		{"kubectl get pods -n apps", false},
		{"kubectl describe deployment web -n apps", false},
		{"kubectl logs deployment/web -n apps", false},
		{"kubectl top pod -n apps", false},
		{"", false},
		{"helm upgrade frontend", false},
	}

	for _, tc := range tests {
		t.Run(tc.command, func(t *testing.T) {
			assert.Equal(t, tc.want, writesWorkload(tc.command))
		})
	}
}

// TestUnknownOwnerNamesTheFieldManager is the reporting half of F-03. Once
// detection refuses on behalf of a specific field manager, the plan has to say
// which one. "Dorgu could not identify what manages this Deployment" is a dead
// end; the manager's name is a lead the reader can follow.
func TestUnknownOwnerNamesTheFieldManager(t *testing.T) {
	ref := &dorguv1.WorkloadRef{
		Kind:            "Deployment",
		Name:            "report-worker",
		Namespace:       "apps",
		ManagedBy:       dorguv1.ManagedByUnknown,
		ManagedByDetail: `field manager "acme-platform-operator" already owns this container's resources`,
	}

	why := whyDorguWillNotPatch(ref)
	assert.Contains(t, why, "acme-platform-operator")
	assert.Contains(t, why, "unknown is treated as owned")

	where, _ := ownerSourceOfTruth(ref)
	assert.Contains(t, where, "report-worker")
	assert.Contains(t, where, "acme-platform-operator")
}

// TestUnknownOwnerWithoutADetailStillExplains keeps the older shape working:
// an unknown owner with nothing to name still gets the full explanation rather
// than a truncated one.
func TestUnknownOwnerWithoutADetailStillExplains(t *testing.T) {
	ref := &dorguv1.WorkloadRef{
		Kind:      "Deployment",
		Name:      "report-worker",
		Namespace: "apps",
		ManagedBy: dorguv1.ManagedByUnknown,
	}

	why := whyDorguWillNotPatch(ref)
	assert.Contains(t, why, "Dorgu could not identify what manages this Deployment")
	assert.Contains(t, why, "Unknown is treated as owned")

	where, _ := ownerSourceOfTruth(ref)
	assert.Contains(t, where, "Dorgu could not identify it")
}
