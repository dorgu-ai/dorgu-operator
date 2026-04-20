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

package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func discoveryTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = dorguv1.AddToScheme(s)
	return s
}

func newDiscoveryReconciler(cl *fake.ClientBuilder, excludeNS ...string) *AppDiscoveryReconciler {
	exclude := make(map[string]bool, len(excludeNS))
	for _, ns := range excludeNS {
		exclude[ns] = true
	}
	return &AppDiscoveryReconciler{
		Client:            cl.Build(),
		Scheme:            runtime.NewScheme(),
		Logger:            logr.Discard(),
		ExcludeNamespaces: exclude,
	}
}

func makeDeployment(namespace, name string, cpu, memory string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "myimage:latest",
						},
					},
				},
			},
		},
	}
	if cpu != "" || memory != "" {
		reqs := corev1.ResourceList{}
		lims := corev1.ResourceList{}
		if cpu != "" {
			reqs[corev1.ResourceCPU] = resource.MustParse(cpu)
			lims[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if memory != "" {
			reqs[corev1.ResourceMemory] = resource.MustParse(memory)
			lims[corev1.ResourceMemory] = resource.MustParse(memory)
		}
		d.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: reqs,
			Limits:   lims,
		}
	}
	return d
}

func makeStatefulSet(namespace, name string, cpu, memory string) *appsv1.StatefulSet {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "myimage:latest",
						},
					},
				},
			},
		},
	}
	if cpu != "" || memory != "" {
		reqs := corev1.ResourceList{}
		lims := corev1.ResourceList{}
		if cpu != "" {
			reqs[corev1.ResourceCPU] = resource.MustParse(cpu)
			lims[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if memory != "" {
			reqs[corev1.ResourceMemory] = resource.MustParse(memory)
			lims[corev1.ResourceMemory] = resource.MustParse(memory)
		}
		sts.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: reqs,
			Limits:   lims,
		}
	}
	return sts
}

func TestAppDiscovery_CreatesPersonaForNewDeployment(t *testing.T) {
	scheme := discoveryTestScheme(t)
	deploy := makeDeployment("default", "my-app", "100m", "128Mi")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy)
	r := &AppDiscoveryReconciler{
		Client:            cl.Build(),
		Scheme:            scheme,
		Logger:            logr.Discard(),
		ExcludeNamespaces: map[string]bool{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "my-app"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	persona := &dorguv1.ApplicationPersona{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-app"}, persona); err != nil {
		t.Fatalf("expected ApplicationPersona to be created: %v", err)
	}

	if persona.Spec.Managed == nil || *persona.Spec.Managed {
		t.Error("expected spec.managed=false for auto-discovered persona")
	}
	if persona.Labels[LabelSource] != LabelSourceAutoDiscovered {
		t.Errorf("expected label %s=%s, got %s", LabelSource, LabelSourceAutoDiscovered, persona.Labels[LabelSource])
	}
	if persona.Labels[LabelWorkloadKind] != "Deployment" {
		t.Errorf("expected label %s=Deployment, got %s", LabelWorkloadKind, persona.Labels[LabelWorkloadKind])
	}
	if persona.Spec.Resources == nil {
		t.Fatal("expected Spec.Resources to be set")
	}
	if persona.Spec.Resources.Requests == nil || persona.Spec.Resources.Requests.CPU != "100m" {
		t.Errorf("expected CPU request 100m, got %v", persona.Spec.Resources.Requests)
	}
}

func TestAppDiscovery_CreatesPersonaForStatefulSet(t *testing.T) {
	scheme := discoveryTestScheme(t)
	sts := makeStatefulSet("default", "my-db", "200m", "256Mi")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts)
	r := &AppDiscoveryReconciler{
		Client:            cl.Build(),
		Scheme:            scheme,
		Logger:            logr.Discard(),
		ExcludeNamespaces: map[string]bool{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "my-db"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	persona := &dorguv1.ApplicationPersona{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-db"}, persona); err != nil {
		t.Fatalf("expected ApplicationPersona to be created: %v", err)
	}

	if persona.Spec.Managed == nil || *persona.Spec.Managed {
		t.Error("expected spec.managed=false for auto-discovered persona")
	}
	if persona.Labels[LabelWorkloadKind] != "StatefulSet" {
		t.Errorf("expected label %s=StatefulSet, got %s", LabelWorkloadKind, persona.Labels[LabelWorkloadKind])
	}
}

func TestAppDiscovery_SkipsExcludedNamespace(t *testing.T) {
	scheme := discoveryTestScheme(t)
	deploy := makeDeployment("kube-system", "coredns", "100m", "128Mi")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy)
	r := &AppDiscoveryReconciler{
		Client:            cl.Build(),
		Scheme:            scheme,
		Logger:            logr.Discard(),
		ExcludeNamespaces: map[string]bool{"kube-system": true},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "kube-system", Name: "coredns"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	list := &dorguv1.ApplicationPersonaList{}
	_ = r.List(context.Background(), list)
	if len(list.Items) != 0 {
		t.Errorf("expected no ApplicationPersonas created for excluded namespace, got %d", len(list.Items))
	}
}

func TestAppDiscovery_SkipsExistingUserDefinedPersona(t *testing.T) {
	scheme := discoveryTestScheme(t)
	deploy := makeDeployment("default", "my-app", "100m", "128Mi")

	existingPersona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "default",
			Labels: map[string]string{
				LabelSource: LabelSourceUserDefined,
			},
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "my-app",
			Type: "api",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, existingPersona)
	r := &AppDiscoveryReconciler{
		Client:            cl.Build(),
		Scheme:            scheme,
		Logger:            logr.Discard(),
		ExcludeNamespaces: map[string]bool{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "my-app"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	persona := &dorguv1.ApplicationPersona{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-app"}, persona); err != nil {
		t.Fatalf("persona should still exist: %v", err)
	}
	// User-defined persona should be unchanged — still user-defined.
	if persona.Labels[LabelSource] != LabelSourceUserDefined {
		t.Errorf("user-defined persona label should not be changed, got %s", persona.Labels[LabelSource])
	}
	if persona.Spec.Type != "api" {
		t.Errorf("user-defined persona spec should not be changed, got type=%s", persona.Spec.Type)
	}
}

func TestAppDiscovery_SyncsResourcesForAutoDiscoveredPersona(t *testing.T) {
	scheme := discoveryTestScheme(t)

	managed := false
	existingPersona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "default",
			Labels: map[string]string{
				LabelSource:       LabelSourceAutoDiscovered,
				LabelWorkloadKind: "Deployment",
			},
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name:    "my-app",
			Type:    "worker",
			Managed: &managed,
			Resources: &dorguv1.ResourceConstraints{
				Requests: &dorguv1.ResourceValues{CPU: "100m", Memory: "128Mi"},
				Limits:   &dorguv1.ResourceValues{CPU: "100m", Memory: "128Mi"},
			},
		},
	}

	// Updated deployment with 200m CPU.
	deploy := makeDeployment("default", "my-app", "200m", "128Mi")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPersona, deploy)
	r := &AppDiscoveryReconciler{
		Client:            cl.Build(),
		Scheme:            scheme,
		Logger:            logr.Discard(),
		ExcludeNamespaces: map[string]bool{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "my-app"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	persona := &dorguv1.ApplicationPersona{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-app"}, persona); err != nil {
		t.Fatalf("persona should still exist: %v", err)
	}
	if persona.Spec.Resources == nil || persona.Spec.Resources.Requests == nil {
		t.Fatal("expected resources to be set after sync")
	}
	if persona.Spec.Resources.Requests.CPU != "200m" {
		t.Errorf("expected CPU to be synced to 200m, got %s", persona.Spec.Resources.Requests.CPU)
	}
}

func TestAppDiscovery_AnnotatesPersonaOnWorkloadDeletion(t *testing.T) {
	scheme := discoveryTestScheme(t)

	managed := false
	existingPersona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "default",
			Labels: map[string]string{
				LabelSource:       LabelSourceAutoDiscovered,
				LabelWorkloadKind: "Deployment",
			},
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name:    "my-app",
			Type:    "worker",
			Managed: &managed,
		},
	}

	// No Deployment object — simulates deletion.
	// WithStatusSubresource ensures the fake client honours the status subresource split,
	// matching real API server behaviour for resources with +kubebuilder:subresource:status.
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPersona).
		WithStatusSubresource(&dorguv1.ApplicationPersona{})
	r := &AppDiscoveryReconciler{
		Client:            cl.Build(),
		Scheme:            scheme,
		Logger:            logr.Discard(),
		ExcludeNamespaces: map[string]bool{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "my-app"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	persona := &dorguv1.ApplicationPersona{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-app"}, persona); err != nil {
		t.Fatalf("persona should NOT be deleted: %v", err)
	}
	if persona.Annotations[LabelWorkloadDeleted] != "true" {
		t.Errorf("expected annotation %s=true, got %q", LabelWorkloadDeleted, persona.Annotations[LabelWorkloadDeleted])
	}
	if persona.Status.Phase != dorguv1.PhaseUnmanaged {
		t.Errorf("expected status.phase=%s, got %q", dorguv1.PhaseUnmanaged, persona.Status.Phase)
	}
}
