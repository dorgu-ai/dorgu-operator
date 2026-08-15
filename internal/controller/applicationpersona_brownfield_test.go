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

// F-01: on a cluster that already has apps, Deployments carry their labels on
// the pod template only. Matching on Deployment-object labels found nothing, so
// every persona sat Pending with "No Deployment with label
// app.kubernetes.io/name=<app>" while the Deployment sat right there in the
// namespace. These specs use the clean-room brownfield manifest shape verbatim.
var _ = Describe("ApplicationPersona brownfield Deployment discovery (F-01)", func() {
	var testCtx = context.Background()

	createNamespace := func(name string) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		Expect(k8sClient.Create(testCtx, ns)).To(Succeed())
	}

	deleteNamespace := func(name string) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		_ = client.IgnoreNotFound(k8sClient.Delete(testCtx, ns))
	}

	createPersona := func(ns, appName string) {
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: ns},
			Spec: dorguv1.ApplicationPersonaSpec{
				Name: appName,
				Type: "web",
				Resources: &dorguv1.ResourceConstraints{
					Requests: &dorguv1.ResourceValues{CPU: "25m", Memory: "32Mi"},
					Limits:   &dorguv1.ResourceValues{CPU: "200m", Memory: "96Mi"},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
	}

	// createBrownfieldDeployment mirrors ~/dorgu-cleanroom/2026-08-09/brownfield.yaml:
	// no labels on the Deployment object, app=<name> on the selector and the pod
	// template. This is what Helm, kustomize and hand-written YAML produce.
	createBrownfieldDeployment := func(ns, name, appLabel string) {
		replicas := int32(1)
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appLabel}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "nginx", Image: "nginx:1.27-alpine"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, deployment)).To(Succeed())

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

	reconcilePersona := func(ns, name string) dorguv1.ApplicationPersona {
		rec := &ApplicationPersonaReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
		_, err := rec.Reconcile(testCtx, req)
		Expect(err).NotTo(HaveOccurred())

		var persona dorguv1.ApplicationPersona
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &persona)).To(Succeed())
		return persona
	}

	readyCondition := func(persona dorguv1.ApplicationPersona) *metav1.Condition {
		for i := range persona.Status.Conditions {
			if persona.Status.Conditions[i].Type == conditionTypeReady {
				return &persona.Status.Conditions[i]
			}
		}
		return nil
	}

	It("resolves a Deployment labelled only on the pod template", func() {
		const ns = "f01-pod-template-labels"
		createNamespace(ns)
		defer deleteNamespace(ns)

		createPersona(ns, "web")
		createBrownfieldDeployment(ns, "web", "web")

		persona := reconcilePersona(ns, "web")

		Expect(persona.Status.Phase).NotTo(Equal(phasePending),
			"a Deployment named web with app=web pod labels must be found")
		Expect(persona.Status.Deployments).NotTo(BeNil())
		Expect(persona.Status.Deployments.Current).To(Equal("nginx:1.27-alpine"))

		cond := readyCondition(persona)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).NotTo(Equal("NoDeployment"))
	})

	It("resolves by selector when the Deployment name differs from the persona", func() {
		const ns = "f01-selector-match"
		createNamespace(ns)
		defer deleteNamespace(ns)

		createPersona(ns, "checkout-api")
		createBrownfieldDeployment(ns, "checkout-api-v2", "checkout-api")

		persona := reconcilePersona(ns, "checkout-api")

		Expect(persona.Status.Phase).NotTo(Equal(phasePending))
		Expect(persona.Status.Deployments).NotTo(BeNil())
	})

	It("ignores unrelated Deployments sharing the namespace", func() {
		const ns = "f01-mixed-namespace"
		createNamespace(ns)
		defer deleteNamespace(ns)

		createPersona(ns, "report-worker")
		createBrownfieldDeployment(ns, "web", "web")
		createBrownfieldDeployment(ns, "checkout-api", "checkout-api")
		createBrownfieldDeployment(ns, "report-worker", "report-worker")

		persona := reconcilePersona(ns, "report-worker")

		Expect(persona.Status.Phase).NotTo(Equal(phasePending))
		Expect(persona.Status.Deployments).NotTo(BeNil())
	})

	It("names every rung it tried when nothing matches", func() {
		const ns = "f01-nothing-matches"
		createNamespace(ns)
		defer deleteNamespace(ns)

		createPersona(ns, "billing")
		createBrownfieldDeployment(ns, "web", "web")

		persona := reconcilePersona(ns, "billing")

		Expect(persona.Status.Phase).To(Equal(phasePending))
		cond := readyCondition(persona)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("NoDeployment"))
		Expect(cond.Message).To(ContainSubstring("app.kubernetes.io/name"))
		Expect(cond.Message).To(ContainSubstring("metadata.name"))
		Expect(cond.Message).To(ContainSubstring("spec.selector.matchLabels"))
	})

	It("refuses to guess when two Deployments match the same rung", func() {
		const ns = "f01-ambiguous"
		createNamespace(ns)
		defer deleteNamespace(ns)

		createPersona(ns, "web")
		createBrownfieldDeployment(ns, "web-blue", "web")
		createBrownfieldDeployment(ns, "web-green", "web")

		persona := reconcilePersona(ns, "web")

		Expect(persona.Status.Phase).To(Equal(phasePending))
		cond := readyCondition(persona)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("AmbiguousDeployment"))
		Expect(cond.Message).To(ContainSubstring("web-blue"))
		Expect(cond.Message).To(ContainSubstring("web-green"))
	})

	It("enqueues the persona when a pod-template-labelled Deployment changes", func() {
		const ns = "f01-mapper"
		createNamespace(ns)
		defer deleteNamespace(ns)

		createPersona(ns, "report-worker")
		rec := &ApplicationPersonaReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "report-worker", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "report-worker"}},
			},
		}

		requests := rec.deploymentToPersona(testCtx, deploy)

		Expect(requests).To(HaveLen(1))
		Expect(requests[0].NamespacedName.Name).To(Equal("report-worker"))
	})
})
