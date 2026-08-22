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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// newWorkloadScheme is newTestScheme plus apps/v1, so a fake client can serve
// the live Deployment the proposer now grounds itself in.
func newWorkloadScheme() *runtime.Scheme {
	s := newTestScheme()
	_ = appsv1.AddToScheme(s)
	return s
}

// groundedPersona is the persona name every grounding test resolves through.
const groundedPersona = "my-app"

// liveDeployment builds a Deployment carrying the groundedPersona label, whose
// single container has exactly the resource keys given. An absent key means the
// workload does not set it, which is the distinction F-05 turns on.
func liveDeployment(name string, limits, requests map[corev1.ResourceName]string) *appsv1.Deployment {
	toList := func(in map[corev1.ResourceName]string) corev1.ResourceList {
		if len(in) == 0 {
			return nil
		}
		out := corev1.ResourceList{}
		for k, v := range in {
			out[k] = resource.MustParse(v)
		}
		return out
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: defaultNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": groundedPersona},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "ghcr.io/stefanprodan/podinfo:6.14.1",
						Resources: corev1.ResourceRequirements{
							Limits:   toList(limits),
							Requests: toList(requests),
						},
					}},
				},
			},
		},
	}
}

// personaWithLimits builds a persona whose recorded limits may diverge from the
// live workload, which is the whole point of these tests.
func personaWithLimits(name, cpu, memory string) *dorguv1.ApplicationPersona {
	p := newTestPersona(defaultNamespace, name)
	p.Spec.Resources.Limits = &dorguv1.ResourceValues{CPU: cpu, Memory: memory}
	return p
}

func patchLimit(t *testing.T, raw []byte, key string) string {
	t.Helper()
	var patch map[string]any
	require.NoError(t, json.Unmarshal(raw, &patch))
	spec, ok := patch["spec"].(map[string]any)
	if !ok {
		return ""
	}
	resources, ok := spec["resources"].(map[string]any)
	if !ok {
		return ""
	}
	limits, ok := resources["limits"].(map[string]any)
	if !ok {
		return ""
	}
	val, _ := limits[key].(string)
	return val
}

// TestProposer_GroundsMemoryInLiveWorkload reproduces F-03 and F-04 together:
// the persona recorded 96Mi, the live Deployment runs 32Mi, and the old
// proposer computed 2x off the persona (192Mi, a 6x jump on the real workload)
// while narrating "96Mi" as the current limit.
func TestProposer_GroundsMemoryInLiveWorkload(t *testing.T) {
	scheme := newWorkloadScheme()
	persona := personaWithLimits("my-app", "", "96Mi")
	deploy := liveDeployment("my-app",
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"},
		map[corev1.ResourceName]string{corev1.ResourceMemory: "16Mi", corev1.ResourceCPU: "25m"})
	incident := newTestIncident(defaultNamespace, "oom-drift", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	proposer := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger())
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)

	result, err := proposer.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	// 2x of the LIVE 32Mi, not of the stale 96Mi.
	assert.Equal(t, "64Mi", patchLimit(t, result.Action.Spec.Action.Patch.Raw, "memory"))

	// Every stated number comes from the live workload.
	assert.Contains(t, result.Action.Spec.Explanation, "32Mi")
	assert.NotContains(t, result.Action.Spec.Explanation, "96Mi",
		"the persona value must never be quoted as the current limit")
}

// TestProposer_WorkloadRefRecordsLiveState pins the CRD contract the CLI reads.
func TestProposer_WorkloadRefRecordsLiveState(t *testing.T) {
	scheme := newWorkloadScheme()
	persona := personaWithLimits("my-app", "", "96Mi")
	// The Deployment is named differently from the persona, as it is in most
	// brownfield clusters. The label rung resolves it.
	deploy := liveDeployment("my-app-podinfo",
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"},
		map[corev1.ResourceName]string{corev1.ResourceMemory: "16Mi", corev1.ResourceCPU: "25m"})
	incident := newTestIncident(defaultNamespace, "oom-ref", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	proposer := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger())
	result, err := proposer.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	ref := result.Action.Spec.WorkloadRef
	require.NotNil(t, ref, "every proposal must record the live workload it concerns")
	assert.Equal(t, "Deployment", ref.Kind)
	assert.Equal(t, "my-app-podinfo", ref.Name, "the workload name, not the persona name")
	assert.Equal(t, defaultNamespace, ref.Namespace)
	assert.Equal(t, "app", ref.Container)
	assert.Equal(t, dorguv1.ManagedByUnmanaged, ref.ManagedBy)
	assert.Equal(t, "ghcr.io/stefanprodan/podinfo:6.14.1", ref.ObservedImage)
	require.NotNil(t, ref.ObservedResources)
	require.NotNil(t, ref.ObservedResources.Limits)
	assert.Equal(t, "32Mi", ref.ObservedResources.Limits.Memory)
	assert.Empty(t, ref.ObservedResources.Limits.CPU, "the workload has no CPU limit")
	require.NotNil(t, ref.ObservedResources.Requests)
	assert.Equal(t, "25m", ref.ObservedResources.Requests.CPU)
}

// TestProposer_NeverIntroducesAbsentResourceKey reproduces F-05: the workload
// has no CPU limit, the persona invented one, and a CPU-saturation proposal
// used to patch a key the workload does not have.
func TestProposer_NeverIntroducesAbsentResourceKey(t *testing.T) {
	scheme := newWorkloadScheme()
	persona := personaWithLimits("my-app", "50m", "32Mi")
	deploy := liveDeployment("my-app",
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"},
		map[corev1.ResourceName]string{corev1.ResourceCPU: "25m"})
	incident := newTestIncident(defaultNamespace, "cpu-absent", "my-app", "CPUSaturationHigh")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	proposer := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger())
	diag := newCPUDiagnosis(defaultNamespace, "my-app", detection.SeverityWarning)

	result, err := proposer.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	assert.False(t, result.Proposed,
		"raising a CPU limit the workload does not have would introduce throttling")
	assert.Contains(t, result.SkipReason, "does not set")
}

// TestProposer_FallsBackToPersonaWhenWorkloadUnreadable keeps the operator
// useful on a cluster where the Deployment cannot be resolved, while refusing
// to present the persona's numbers as live fact.
func TestProposer_FallsBackToPersonaWhenWorkloadUnreadable(t *testing.T) {
	scheme := newWorkloadScheme()
	persona := personaWithLimits("solo-app", "", "256Mi")
	incident := newTestIncident(defaultNamespace, "oom-noworkload", "solo-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	proposer := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger())
	result, err := proposer.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "solo-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	assert.Equal(t, "512Mi", patchLimit(t, result.Action.Spec.Action.Patch.Raw, "memory"))
	require.NotNil(t, result.Action.Spec.WorkloadRef)
	assert.Equal(t, dorguv1.ManagedByUnknown, result.Action.Spec.WorkloadRef.ManagedBy,
		"an unresolvable workload is owned until proven otherwise")
	assert.True(t, strings.Contains(result.Action.Spec.Explanation, "persona"),
		"the explanation must say the number came from the persona, not the running pod")
}
