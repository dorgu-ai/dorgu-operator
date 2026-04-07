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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// BUG-12-2: ApplicationPersona must match deployments using both `app.kubernetes.io/name`
// and the common `app` label as a fallback.
var _ = Describe("ApplicationPersona label matching fallback (BUG-12-2)", func() {
	var (
		testCtx = context.Background()
	)

	createNamespace := func(name string) {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}
		Expect(k8sClient.Create(testCtx, ns)).To(Succeed())
	}

	deleteNamespace := func(name string) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		_ = client.IgnoreNotFound(k8sClient.Delete(testCtx, ns))
	}

	createPersona := func(ns, appName string) {
		minR := int32(1)
		maxR := int32(1)
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{
				Name:      appName,
				Namespace: ns,
			},
			Spec: dorguv1.ApplicationPersonaSpec{
				Name:    appName,
				Type:    "api",
				Scaling: &dorguv1.ScalingSpec{MinReplicas: &minR, MaxReplicas: &maxR},
			},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
	}

	createDeployment := func(ns, name string, labels map[string]string) {
		replicas := int32(1)
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Labels:    labels,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": name},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": name},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "app", Image: "nginx:latest"},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, deployment)).To(Succeed())

		// Set deployment status so it's considered available
		var dep appsv1.Deployment
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			Replicas:      1,
			ReadyReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		}
		Expect(k8sClient.Status().Update(testCtx, &dep)).To(Succeed())
	}

	It("TestApplicationPersona_MatchesAppLabel", func() {
		const ns = "bug-12-2-app-label"
		createNamespace(ns)
		defer deleteNamespace(ns)

		const appName = "app-label-only"
		createPersona(ns, appName)

		// Deployment with ONLY the "app" label — no app.kubernetes.io/name
		createDeployment(ns, appName, map[string]string{
			"app": appName,
		})

		rec := &ApplicationPersonaReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: appName, Namespace: ns}}
		_, err := rec.Reconcile(testCtx, req)
		Expect(err).NotTo(HaveOccurred())

		// Persona should NOT be in Pending phase with "No Deployment" message
		var persona dorguv1.ApplicationPersona
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: appName, Namespace: ns}, &persona)).To(Succeed())
		Expect(persona.Status.Phase).NotTo(Equal(phasePending), "persona should find deployment via 'app' label fallback")
		Expect(persona.Status.Deployments).NotTo(BeNil(), "deployment tracking should be populated")
	})

	It("TestApplicationPersona_PrefersRecommendedLabel", func() {
		const ns = "bug-12-2-recommended"
		createNamespace(ns)
		defer deleteNamespace(ns)

		const appName = "recommended-label"
		createPersona(ns, appName)

		// Deployment with both labels — recommended should be found on first pass
		createDeployment(ns, appName, map[string]string{
			"app.kubernetes.io/name": appName,
			"app":                    appName,
		})

		rec := &ApplicationPersonaReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: appName, Namespace: ns}}
		_, err := rec.Reconcile(testCtx, req)
		Expect(err).NotTo(HaveOccurred())

		var persona dorguv1.ApplicationPersona
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: appName, Namespace: ns}, &persona)).To(Succeed())
		Expect(persona.Status.Phase).NotTo(Equal(phasePending))
		Expect(persona.Status.Deployments).NotTo(BeNil())
	})

	It("TestApplicationPersona_NoLabelNoMatch", func() {
		const ns = "bug-12-2-no-label"
		createNamespace(ns)
		defer deleteNamespace(ns)

		const appName = "no-label-match"
		createPersona(ns, appName)

		// Deployment with neither label
		createDeployment(ns, appName+"-different", map[string]string{
			"team": "backend",
		})

		rec := &ApplicationPersonaReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: appName, Namespace: ns}}
		_, err := rec.Reconcile(testCtx, req)
		Expect(err).NotTo(HaveOccurred())

		var persona dorguv1.ApplicationPersona
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: appName, Namespace: ns}, &persona)).To(Succeed())
		Expect(persona.Status.Phase).To(Equal(phasePending), "persona with no matching deployment should be Pending")
	})

	It("TestDeploymentToPersona_FallbackLabel", func() {
		const ns = "bug-12-2-mapper-fallback"
		createNamespace(ns)
		defer deleteNamespace(ns)

		const appName = "mapper-fallback"
		createPersona(ns, appName)

		rec := &ApplicationPersonaReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		// Deployment with only "app" label
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      appName,
				Namespace: ns,
				Labels: map[string]string{
					"app": appName,
				},
			},
		}

		requests := rec.deploymentToPersona(testCtx, deploy)
		Expect(requests).NotTo(BeEmpty(), "deploymentToPersona should match via 'app' label fallback")
		Expect(requests[0].NamespacedName.Name).To(Equal(appName))
	})

	It("TestDeploymentToPersona_RecommendedLabel", func() {
		const ns = "bug-12-2-mapper-recommended"
		createNamespace(ns)
		defer deleteNamespace(ns)

		const appName = "mapper-recommended"
		createPersona(ns, appName)

		rec := &ApplicationPersonaReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		// Deployment with both labels — should use app.kubernetes.io/name
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      appName,
				Namespace: ns,
				Labels: map[string]string{
					"app.kubernetes.io/name": appName,
					"app":                    appName,
				},
			},
		}

		requests := rec.deploymentToPersona(testCtx, deploy)
		Expect(requests).NotTo(BeEmpty())
		Expect(requests[0].NamespacedName.Name).To(Equal(appName))
	})

	It("TestDeploymentToPersona_NoLabel", func() {
		const ns = "bug-12-2-mapper-none"
		createNamespace(ns)
		defer deleteNamespace(ns)

		rec := &ApplicationPersonaReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		// Deployment with neither label
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "no-label-deploy",
				Namespace: ns,
				Labels: map[string]string{
					"team": "backend",
				},
			},
		}

		requests := rec.deploymentToPersona(testCtx, deploy)
		Expect(requests).To(BeEmpty(), "deployment with no app labels should not trigger persona reconcile")
	})
})
