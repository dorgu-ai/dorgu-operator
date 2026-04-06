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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// BUG-12-1: OOM workload should produce IncidentMemory and RemediationAction for the
// ApplicationPersona. These tests encode the expected behavior; they fail on unfixed code
// because ApplicationPersona reconciliation does not create those CRs today.
var _ = Describe("ApplicationPersona OOM incident flow (BUG-12-1)", func() {
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

	// Mirrors QA reproduction: Deployment + ApplicationPersona + Pod status showing OOMKilled
	// (CrashLoopBackOff with last state OOMKilled).
	setupOOMWorkload := func(ns, appName string) {
		minR := int32(1)
		maxR := int32(1)
		healthPort := int32(8080)
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{
				Name:      appName,
				Namespace: ns,
			},
			Spec: dorguv1.ApplicationPersonaSpec{
				Name:    appName,
				Version: "1",
				Type:    "api",
				Tier:    "standard",
				Technical: &dorguv1.TechnicalProfile{
					Language:    "go",
					Description: "Stress pod to trigger OOM",
				},
				Resources: &dorguv1.ResourceConstraints{
					Requests: &dorguv1.ResourceValues{CPU: "100m", Memory: "32Mi"},
					Limits:   &dorguv1.ResourceValues{CPU: "200m", Memory: "64Mi"},
					Profile:  "standard",
				},
				Scaling: &dorguv1.ScalingSpec{MinReplicas: &minR, MaxReplicas: &maxR},
				Health: &dorguv1.HealthSpec{
					LivenessPath:  "/",
					ReadinessPath: "/",
					Port:          &healthPort,
				},
			},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())

		replicas := int32(1)
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      appName,
				Namespace: ns,
				Labels: map[string]string{
					"app":                    appName,
					"app.kubernetes.io/name": appName,
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": appName},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": appName},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:    "stress",
								Image:   "polinux/stress",
								Command: []string{"stress", "--vm", "1", "--vm-bytes", "256M"},
								Resources: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceMemory: resource.MustParse("64Mi"),
									},
									Requests: corev1.ResourceList{
										corev1.ResourceMemory: resource.MustParse("32Mi"),
									},
								},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, deployment)).To(Succeed())

		deployKey := types.NamespacedName{Name: appName, Namespace: ns}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(testCtx, deployKey, &dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			Replicas:      1,
			ReadyReplicas: 0,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse},
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue},
			},
		}
		Expect(k8sClient.Status().Update(testCtx, &dep)).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      appName + "-pod-1",
				Namespace: ns,
				Labels:    map[string]string{"app": appName},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:    "stress",
						Image:   "polinux/stress",
						Command: []string{"stress", "--vm", "1", "--vm-bytes", "256M"},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, pod)).To(Succeed())

		pod.Status = corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "stress",
					RestartCount: 5,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							ExitCode:   137,
							FinishedAt: metav1.Now(),
						},
					},
				},
			},
		}
		Expect(k8sClient.Status().Update(testCtx, pod)).To(Succeed())
	}

	It("TestApplicationPersona_OOMCreatesIncidentAndRemediation", func() {
		const ns = "bug-12-1-oom-primary"
		createNamespace(ns)
		defer deleteNamespace(ns)

		const appName = "oom-primary"
		setupOOMWorkload(ns, appName)

		rec := &ApplicationPersonaReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: appName, Namespace: ns}}
		_, err := rec.Reconcile(testCtx, req)
		Expect(err).NotTo(HaveOccurred())

		// Expected: IncidentMemory for OOM in this namespace, linked to the persona.
		var incidents dorguv1.IncidentMemoryList
		Expect(k8sClient.List(testCtx, &incidents, client.InNamespace(ns))).To(Succeed())
		var oomIncident *dorguv1.IncidentMemory
		for i := range incidents.Items {
			im := &incidents.Items[i]
			if im.Spec.Detection.Signal == "OOMKilled" || (im.Labels != nil && im.Labels[LabelSignal] == "OOMKilled") {
				oomIncident = im
				break
			}
		}
		Expect(oomIncident).NotTo(BeNil(), "expected an IncidentMemory with OOMKilled after OOM workload reconcile")
		Expect(oomIncident.Spec.PersonaRef.Name).To(Equal(appName))
		Expect(oomIncident.Namespace).To(Equal(ns))

		// Expected: RemediationAction proposing a memory increase for the persona.
		var actions dorguv1.RemediationActionList
		Expect(k8sClient.List(testCtx, &actions, client.InNamespace(ns))).To(Succeed())
		Expect(actions.Items).NotTo(BeEmpty(), "expected a RemediationAction for OOM memory remediation")

		foundMemoryProposal := false
		for i := range actions.Items {
			ra := &actions.Items[i]
			if ra.Spec.Action.Type != "persona-update" {
				continue
			}
			if ra.Spec.Action.Patch != nil && strings.Contains(string(ra.Spec.Action.Patch.Raw), "memory") {
				foundMemoryProposal = true
				break
			}
		}
		Expect(foundMemoryProposal).To(BeTrue(), "expected persona-update patch increasing memory")
	})

	It("TestApplicationPersona_MultipleOOMEventsSingleIncidentWithCount", func() {
		const ns = "bug-12-1-oom-multi"
		createNamespace(ns)
		defer deleteNamespace(ns)

		const appName = "oom-multi"
		setupOOMWorkload(ns, appName)

		rec := &ApplicationPersonaReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: appName, Namespace: ns}}
		_, err := rec.Reconcile(testCtx, req)
		Expect(err).NotTo(HaveOccurred())
		_, err = rec.Reconcile(testCtx, req)
		Expect(err).NotTo(HaveOccurred())

		var incidents dorguv1.IncidentMemoryList
		Expect(k8sClient.List(testCtx, &incidents, client.InNamespace(ns))).To(Succeed())
		Expect(incidents.Items).To(HaveLen(1), "multiple OOM observations should update one incident, not duplicate")

		var oom *dorguv1.IncidentMemory
		for i := range incidents.Items {
			if incidents.Items[i].Spec.Detection.Signal == "OOMKilled" {
				oom = &incidents.Items[i]
				break
			}
		}
		Expect(oom).NotTo(BeNil())
		Expect(oom.Status.OccurrenceCount).To(BeNumerically(">=", 2))
	})

	It("TestApplicationPersona_RestartWithoutOOMNoOOMIncident", func() {
		const ns = "bug-12-1-no-oom"
		createNamespace(ns)
		defer deleteNamespace(ns)

		const appName = "restart-clean"
		minR := int32(1)
		maxR := int32(1)
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: ns},
			Spec: dorguv1.ApplicationPersonaSpec{
				Name: appName,
				Type: "api",
				Resources: &dorguv1.ResourceConstraints{
					Limits: &dorguv1.ResourceValues{Memory: "64Mi", CPU: "200m"},
				},
				Scaling: &dorguv1.ScalingSpec{MinReplicas: &minR, MaxReplicas: &maxR},
			},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())

		replicas := int32(1)
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      appName,
				Namespace: ns,
				Labels: map[string]string{
					"app":                    appName,
					"app.kubernetes.io/name": appName,
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appName}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appName}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, deployment)).To(Succeed())

		var dep appsv1.Deployment
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: appName, Namespace: ns}, &dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			Replicas:      1,
			ReadyReplicas: 0,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse},
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue},
			},
		}
		Expect(k8sClient.Status().Update(testCtx, &dep)).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      appName + "-pod-err",
				Namespace: ns,
				Labels:    map[string]string{"app": appName},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
			},
		}
		Expect(k8sClient.Create(testCtx, pod)).To(Succeed())
		// Restarts from a non-OOM error (e.g. app exit) — must not be classified as OOM.
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 3,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "Error",
							ExitCode:   1,
							FinishedAt: metav1.Now(),
						},
					},
				},
			},
		}
		Expect(k8sClient.Status().Update(testCtx, pod)).To(Succeed())

		rec := &ApplicationPersonaReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := rec.Reconcile(testCtx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: appName, Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		var incidents dorguv1.IncidentMemoryList
		Expect(k8sClient.List(testCtx, &incidents,
			client.InNamespace(ns),
			client.MatchingLabels{LabelSignal: "OOMKilled"},
		)).To(Succeed())
		Expect(incidents.Items).To(BeEmpty(), "non-OOM restarts must not produce an OOMKilled incident")
	})
})
