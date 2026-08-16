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

package webhook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, dorguv1.AddToScheme(scheme))
	return scheme
}

// personaWithMemoryLimit builds a persona capping memory at the given quantity.
func personaWithMemoryLimit(name, namespace, limit string) *dorguv1.ApplicationPersona {
	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: name,
			Type: "web",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{Memory: limit},
			},
		},
	}
}

// brownfieldDeployment mirrors the clean-room manifest: labels on the pod
// template and selector only, nothing on the Deployment object.
func brownfieldDeployment(memoryLimit string) *appsv1.Deployment {
	const (
		name      = "web"
		namespace = "apps"
	)
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "nginx",
						Image: "nginx:1.27-alpine",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse(memoryLimit),
							},
						},
					}},
				},
			},
		},
	}
}

func admissionRequestFor(t *testing.T, deploy *appsv1.Deployment) admission.Request {
	t.Helper()
	raw, err := json.Marshal(deploy)
	require.NoError(t, err)
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

// F-01: the validator used to bail out on "no app.kubernetes.io/name label",
// which silently exempted every Helm/kustomize workload from persona checks.
func TestHandle_ValidatesPodTemplateLabelledDeployment(t *testing.T) {
	scheme := testScheme(t)
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(personaWithMemoryLimit("web", "apps", "96Mi")).
		Build()

	v := &DeploymentValidator{Client: client, Mode: ModeAdvisory}
	require.NoError(t, v.InjectDecoder(admission.NewDecoder(scheme)))

	resp := v.Handle(context.Background(), admissionRequestFor(t, brownfieldDeployment("512Mi")))

	assert.True(t, resp.Allowed, "advisory mode always allows")
	require.NotEmpty(t, resp.Warnings, "a Deployment over its persona limit must be warned about")
	assert.Contains(t, resp.Warnings[0], "exceeds persona limit")
}

func TestHandle_EnforcingRejectsPodTemplateLabelledDeployment(t *testing.T) {
	scheme := testScheme(t)
	minReplicas := int32(3)
	persona := personaWithMemoryLimit("web", "apps", "96Mi")
	persona.Spec.Scaling = &dorguv1.ScalingSpec{MinReplicas: &minReplicas}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(persona).Build()

	v := &DeploymentValidator{Client: client, Mode: ModeEnforcing}
	require.NoError(t, v.InjectDecoder(admission.NewDecoder(scheme)))

	deploy := brownfieldDeployment("64Mi")
	replicas := int32(1)
	deploy.Spec.Replicas = &replicas

	resp := v.Handle(context.Background(), admissionRequestFor(t, deploy))

	assert.False(t, resp.Allowed)
	assert.Contains(t, resp.Result.Message, "below persona minimum")
}

func TestHandle_AllowsWhenNoPersonaMatches(t *testing.T) {
	scheme := testScheme(t)
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(personaWithMemoryLimit("billing", "apps", "96Mi")).
		Build()

	v := &DeploymentValidator{Client: client, Mode: ModeEnforcing}
	require.NoError(t, v.InjectDecoder(admission.NewDecoder(scheme)))

	resp := v.Handle(context.Background(), admissionRequestFor(t, brownfieldDeployment("512Mi")))

	assert.True(t, resp.Allowed)
	assert.Empty(t, resp.Warnings)
}

func TestHandle_IgnoresDeleteAndConnect(t *testing.T) {
	scheme := testScheme(t)
	v := &DeploymentValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		Mode:   ModeEnforcing,
	}
	require.NoError(t, v.InjectDecoder(admission.NewDecoder(scheme)))

	req := admissionRequestFor(t, brownfieldDeployment("512Mi"))
	req.Operation = admissionv1.Delete

	resp := v.Handle(context.Background(), req)

	assert.True(t, resp.Allowed)
}
