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

package events

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(dorguv1.AddToScheme(s))
	return s
}

func newInternalEvent(kind, name, namespace string) *InternalEvent {
	return &InternalEvent{
		ID:       "test-id",
		Severity: SeverityCritical,
		Category: CategoryHealth,
		Source:   "test",
		Message:  "test event",
		InvolvedObject: dorguv1.ResourceReference{
			Kind:      kind,
			Name:      name,
			Namespace: namespace,
		},
		EventTime: time.Now(),
	}
}

func TestCorrelator_NodeToClusterPersona(t *testing.T) {
	scheme := testScheme()
	cp := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster"},
		Spec:       dorguv1.ClusterPersonaSpec{Name: "my-cluster"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("Node", "worker-1", "")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	require.NotNil(t, event.PersonaRef)
	assert.Equal(t, "ClusterPersona", event.PersonaRef.Kind)
	assert.Equal(t, "my-cluster", event.PersonaRef.Name)
}

func TestCorrelator_NodeWithNoClusterPersona(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("Node", "worker-1", "")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	assert.Nil(t, event.PersonaRef)
}

func TestCorrelator_PodToApplicationPersona(t *testing.T) {
	scheme := testScheme()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server-7d9f8b6c4-x2k9l",
			Namespace: "production",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "api-server-7d9f8b6c4"},
			},
			Labels: map[string]string{
				"app.kubernetes.io/name": "api-server",
			},
		},
	}
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server",
			Namespace: "production",
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "api-server",
			Type: "api",
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod, persona).
		Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("Pod", "api-server-7d9f8b6c4-x2k9l", "production")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	require.NotNil(t, event.PersonaRef)
	assert.Equal(t, "ApplicationPersona", event.PersonaRef.Kind)
	assert.Equal(t, "api-server", event.PersonaRef.Name)
	assert.Equal(t, "production", event.PersonaRef.Namespace)
}

func TestCorrelator_PodWithDeploymentOwner(t *testing.T) {
	scheme := testScheme()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-app-abc123",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: "web-app"},
			},
		},
	}
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-app",
			Namespace: "default",
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "web-app",
			Type: "web",
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod, persona).
		Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("Pod", "web-app-abc123", "default")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	require.NotNil(t, event.PersonaRef)
	assert.Equal(t, "ApplicationPersona", event.PersonaRef.Kind)
	assert.Equal(t, "web-app", event.PersonaRef.Name)
}

func TestCorrelator_ReplicaSetToApplicationPersona(t *testing.T) {
	scheme := testScheme()
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server-7d9f8b6c4",
			Namespace: "production",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: "api-server"},
			},
		},
	}
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server",
			Namespace: "production",
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "api-server",
			Type: "api",
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rs, persona).
		Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("ReplicaSet", "api-server-7d9f8b6c4", "production")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	require.NotNil(t, event.PersonaRef)
	assert.Equal(t, "ApplicationPersona", event.PersonaRef.Kind)
	assert.Equal(t, "api-server", event.PersonaRef.Name)
}

func TestCorrelator_DeploymentToApplicationPersona(t *testing.T) {
	scheme := testScheme()
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "staging",
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "my-app",
			Type: "worker",
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(persona).
		Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("Deployment", "my-app", "staging")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	require.NotNil(t, event.PersonaRef)
	assert.Equal(t, "ApplicationPersona", event.PersonaRef.Kind)
	assert.Equal(t, "my-app", event.PersonaRef.Name)
}

func TestCorrelator_FallbackToNamespace(t *testing.T) {
	scheme := testScheme()
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "only-app",
			Namespace: "isolated",
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "only-app",
			Type: "api",
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(persona).
		Build()
	correlator := NewCorrelator(fakeClient)

	// Service event — not a Pod/Node/RS/Deployment, so falls through to namespace correlation.
	event := newInternalEvent("Service", "some-service", "isolated")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	require.NotNil(t, event.PersonaRef)
	assert.Equal(t, "only-app", event.PersonaRef.Name)
}

func TestCorrelator_NilEvent(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	correlator := NewCorrelator(fakeClient)

	err := correlator.Correlate(context.Background(), nil)
	assert.NoError(t, err)
}

func TestCorrelator_AlreadyCorrelated(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("Pod", "test-pod", "default")
	event.PersonaRef = &dorguv1.PersonaReference{
		Kind: "ApplicationPersona",
		Name: "existing",
	}

	err := correlator.Correlate(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, "existing", event.PersonaRef.Name)
}

func TestCorrelator_NoMatchingPersona(t *testing.T) {
	scheme := testScheme()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orphan-pod",
			Namespace: "default",
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("Pod", "orphan-pod", "default")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	assert.Nil(t, event.PersonaRef, "should be nil when no persona matches")
}

func TestCorrelator_EmptyNamespace(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("Pod", "some-pod", "")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	assert.Nil(t, event.PersonaRef)
}

func TestCorrelator_PodDeletedFallsBackToNamespace(t *testing.T) {
	scheme := testScheme()
	// Pod doesn't exist but there's a persona in the namespace.
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-server",
			Namespace: "production",
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "api-server",
			Type: "api",
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(persona).
		Build()
	correlator := NewCorrelator(fakeClient)

	event := newInternalEvent("Pod", "deleted-pod-xyz", "production")
	err := correlator.Correlate(context.Background(), event)

	require.NoError(t, err)
	require.NotNil(t, event.PersonaRef)
	assert.Equal(t, "api-server", event.PersonaRef.Name)
}

func TestFindOwnerDeployment(t *testing.T) {
	tests := []struct {
		name     string
		refs     []metav1.OwnerReference
		expected string
	}{
		{
			name:     "no owner references",
			refs:     nil,
			expected: "",
		},
		{
			name: "deployment owner",
			refs: []metav1.OwnerReference{
				{Kind: "Deployment", Name: "my-app"},
			},
			expected: "my-app",
		},
		{
			name: "replicaset owner (not deployment)",
			refs: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-app-abc123"},
			},
			expected: "",
		},
		{
			name: "multiple owners, deployment present",
			refs: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-app-abc123"},
				{Kind: "Deployment", Name: "my-app"},
			},
			expected: "my-app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findOwnerDeployment(tt.refs)
			assert.Equal(t, tt.expected, result)
		})
	}
}
