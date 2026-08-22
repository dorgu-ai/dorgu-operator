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

package workload

import (
	"context"
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
)

func observeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(s))
	return s
}

func containerWith(name, image string, limits, requests corev1.ResourceList) corev1.Container {
	return corev1.Container{
		Name:      name,
		Image:     image,
		Resources: corev1.ResourceRequirements{Limits: limits, Requests: requests},
	}
}

func deployWithContainers(name string, labels map[string]string, containers ...corev1.Container) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps", Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers}},
		},
	}
}

func TestObserve_ResolvesAndRecordsLiveState(t *testing.T) {
	deploy := deployWithContainers("frontend-podinfo",
		map[string]string{LabelAppName: "frontend"},
		containerWith("podinfo", "ghcr.io/stefanprodan/podinfo:6.14.1",
			corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
			corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("16Mi"),
				corev1.ResourceCPU:    resource.MustParse("25m"),
			}))
	deploy.Labels[LabelManagedBy] = "Helm"

	c := fake.NewClientBuilder().WithScheme(observeScheme(t)).WithRuntimeObjects(deploy).Build()

	obs, err := Observe(context.Background(), c, "apps", "frontend")
	require.NoError(t, err)
	require.NotNil(t, obs)
	require.NotNil(t, obs.Container)
	assert.Equal(t, "podinfo", obs.Container.Name,
		"the sole container is the one the persona describes, even under a different name")

	ref := obs.Ref(metav1.Now())
	assert.Equal(t, "Deployment", ref.Kind)
	assert.Equal(t, "frontend-podinfo", ref.Name, "the workload name is not the persona name")
	assert.Equal(t, "apps", ref.Namespace)
	assert.Equal(t, "podinfo", ref.Container)
	assert.Equal(t, dorguv1.ManagedByHelm, ref.ManagedBy)
	assert.Equal(t, "ghcr.io/stefanprodan/podinfo:6.14.1", ref.ObservedImage)

	require.NotNil(t, ref.ObservedResources)
	require.NotNil(t, ref.ObservedResources.Limits)
	assert.Equal(t, "32Mi", ref.ObservedResources.Limits.Memory)
	assert.Empty(t, ref.ObservedResources.Limits.CPU,
		"an absent CPU limit stays absent rather than becoming a zero quantity")
	require.NotNil(t, ref.ObservedResources.Requests)
	assert.Equal(t, "25m", ref.ObservedResources.Requests.CPU)
	assert.Equal(t, "16Mi", ref.ObservedResources.Requests.Memory)
}

func TestObserve_NoMatchIsNotAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(observeScheme(t)).Build()

	obs, err := Observe(context.Background(), c, "apps", "frontend")
	require.NoError(t, err)
	assert.Nil(t, obs)

	ref := UnresolvedRef(metav1.Now())
	assert.Equal(t, dorguv1.ManagedByUnknown, ref.ManagedBy)
	assert.True(t, ref.IsOwned(), "a workload we cannot see is owned until proven otherwise")
}

func TestObserve_AmbiguousMatchIsAnError(t *testing.T) {
	a := deployWithContainers("frontend-a", map[string]string{LabelAppName: "frontend"},
		containerWith("app", "nginx:1.27", nil, nil))
	b := deployWithContainers("frontend-b", map[string]string{LabelAppName: "frontend"},
		containerWith("app", "nginx:1.27", nil, nil))

	c := fake.NewClientBuilder().WithScheme(observeScheme(t)).WithRuntimeObjects(a, b).Build()

	obs, err := Observe(context.Background(), c, "apps", "frontend")
	require.Error(t, err, "grounding in an arbitrarily chosen workload is worse than not grounding")
	assert.Nil(t, obs)
}

func TestPickContainer(t *testing.T) {
	sidecar := containerWith("istio-proxy", "istio:1.21", nil, nil)
	app := containerWith("frontend", "nginx:1.27", nil, nil)
	other := containerWith("worker", "worker:1", nil, nil)

	t.Run("exact name match wins", func(t *testing.T) {
		d := deployWithContainers("d", nil, sidecar, app)
		assert.Equal(t, "frontend", PickContainer(d, "frontend").Name)
	})

	t.Run("the sole container is used when the name differs", func(t *testing.T) {
		d := deployWithContainers("d", nil, sidecar)
		assert.Equal(t, "istio-proxy", PickContainer(d, "frontend").Name)
	})

	t.Run("the first container is the fallback", func(t *testing.T) {
		d := deployWithContainers("d", nil, other, sidecar)
		assert.Equal(t, "worker", PickContainer(d, "frontend").Name)
	})

	t.Run("no containers yields nil", func(t *testing.T) {
		assert.Nil(t, PickContainer(deployWithContainers("d", nil), "frontend"))
	})
}

func TestObservedResources_NoResourceBlockAtAll(t *testing.T) {
	deploy := deployWithContainers("bare", map[string]string{LabelAppName: "bare"},
		containerWith("app", "nginx:1.27", nil, nil))
	c := fake.NewClientBuilder().WithScheme(observeScheme(t)).WithRuntimeObjects(deploy).Build()

	obs, err := Observe(context.Background(), c, "apps", "bare")
	require.NoError(t, err)
	require.NotNil(t, obs)

	ref := obs.Ref(metav1.Now())
	assert.Nil(t, ref.ObservedResources,
		"a container that sets nothing must not be described as setting zeroes")
}
