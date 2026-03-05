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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

var _ = Describe("ClusterPersona Controller", func() {
	Context("When reconciling a ClusterPersona resource", func() {
		const resourceName = "test-cluster"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "dorgu-system",
		}

		BeforeEach(func() {
			By("creating the dorgu-system namespace")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "dorgu-system",
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "dorgu-system"}, ns)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			}

			By("creating the custom resource for the Kind ClusterPersona")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			if err != nil && errors.IsNotFound(err) {
				resource := &dorguv1.ClusterPersona{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "dorgu-system",
					},
					Spec: dorguv1.ClusterPersonaSpec{
						Name:        resourceName,
						Environment: "development",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &dorguv1.ClusterPersona{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				By("Cleanup the specific resource instance ClusterPersona")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the status was updated")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusterpersona.Status.Phase).To(Or(Equal("Discovering"), Equal("Ready"), Equal("Degraded")))
		})

		It("should discover nodes in the cluster", func() {
			By("Creating a test node")
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node-1",
					Labels: map[string]string{
						"kubernetes.io/os":   "linux",
						"kubernetes.io/arch": "amd64",
					},
				},
				Status: corev1.NodeStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("8Gi"),
					},
					Allocatable: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("3800m"),
						corev1.ResourceMemory: resource.MustParse("7Gi"),
					},
					NodeInfo: corev1.NodeSystemInfo{
						KubeletVersion: "v1.28.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			By("Reconciling the ClusterPersona")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking nodes were discovered")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusterpersona.Status.Nodes).To(HaveLen(1))
			Expect(clusterpersona.Status.Nodes[0].Name).To(Equal("test-node-1"))

			By("Cleanup test node")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		})

		It("should count namespaces", func() {
			By("Creating test namespaces")
			ns1 := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ns-1",
				},
			}
			ns2 := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-ns-2",
				},
			}
			Expect(k8sClient.Create(ctx, ns1)).To(Succeed())
			Expect(k8sClient.Create(ctx, ns2)).To(Succeed())

			By("Reconciling the ClusterPersona")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking namespace summary")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusterpersona.Status.Namespaces).NotTo(BeNil())
			Expect(clusterpersona.Status.Namespaces.Total).To(BeNumerically(">=", 4))

			By("Cleanup test namespaces")
			Expect(k8sClient.Delete(ctx, ns1)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ns2)).To(Succeed())
		})
	})

	Context("When reconciling with multiple nodes", func() {
		const resourceName = "multi-node-cluster"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "dorgu-system",
		}

		var nodes []*corev1.Node

		BeforeEach(func() {
			By("creating the dorgu-system namespace")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "dorgu-system",
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "dorgu-system"}, ns)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			}

			By("creating multiple test nodes")
			nodes = []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "multi-node-cp-1",
						Labels: map[string]string{
							"node-role.kubernetes.io/control-plane": "",
							"kubernetes.io/os":                      "linux",
							"kubernetes.io/arch":                    "amd64",
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
						Capacity: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
						},
						Allocatable: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("3800m"),
							corev1.ResourceMemory: resource.MustParse("7Gi"),
						},
						NodeInfo: corev1.NodeSystemInfo{
							KubeletVersion: "v1.28.0",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "multi-node-worker-1",
						Labels: map[string]string{
							"kubernetes.io/os":   "linux",
							"kubernetes.io/arch": "amd64",
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
						Capacity: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
						Allocatable: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("7800m"),
							corev1.ResourceMemory: resource.MustParse("15Gi"),
						},
						NodeInfo: corev1.NodeSystemInfo{
							KubeletVersion: "v1.28.0",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "multi-node-worker-2",
						Labels: map[string]string{
							"kubernetes.io/os":   "linux",
							"kubernetes.io/arch": "amd64",
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
						Capacity: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("8"),
							corev1.ResourceMemory: resource.MustParse("16Gi"),
						},
						Allocatable: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("7800m"),
							corev1.ResourceMemory: resource.MustParse("15Gi"),
						},
						NodeInfo: corev1.NodeSystemInfo{
							KubeletVersion: "v1.28.0",
						},
					},
				},
			}

			for _, node := range nodes {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &corev1.Node{})
				if errors.IsNotFound(err) {
					Expect(k8sClient.Create(ctx, node)).To(Succeed())
				}
			}

			By("creating the ClusterPersona")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			if err != nil && errors.IsNotFound(err) {
				resource := &dorguv1.ClusterPersona{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "dorgu-system",
					},
					Spec: dorguv1.ClusterPersonaSpec{
						Name:        resourceName,
						Environment: "production",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup test nodes")
			for _, node := range nodes {
				n := &corev1.Node{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, n)
				if err == nil {
					Expect(k8sClient.Delete(ctx, n)).To(Succeed())
				}
			}

			By("Cleanup ClusterPersona")
			resource := &dorguv1.ClusterPersona{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should discover all nodes and calculate resource summary", func() {
			By("Reconciling the ClusterPersona")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking all nodes were discovered")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(clusterpersona.Status.Nodes)).To(BeNumerically(">=", 3))

			By("Checking resource summary was calculated")
			Expect(clusterpersona.Status.ResourceSummary).NotTo(BeNil())
			Expect(clusterpersona.Status.ResourceSummary.TotalCPU).NotTo(BeEmpty())
			Expect(clusterpersona.Status.ResourceSummary.TotalMemory).NotTo(BeEmpty())
		})

		It("should set Ready phase when all nodes are ready", func() {
			By("Reconciling the ClusterPersona")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the phase is Ready")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusterpersona.Status.Phase).To(Equal("Ready"))
		})

		It("should identify node roles correctly", func() {
			By("Reconciling the ClusterPersona")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking node roles")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			Expect(err).NotTo(HaveOccurred())

			controlPlaneCount := 0
			workerCount := 0
			for _, node := range clusterpersona.Status.Nodes {
				if node.Role == "control-plane" {
					controlPlaneCount++
				} else if node.Role == "worker" {
					workerCount++
				}
			}
			Expect(controlPlaneCount).To(BeNumerically(">=", 1))
			Expect(workerCount).To(BeNumerically(">=", 2))
		})
	})

	Context("When reconciling with degraded nodes", func() {
		const resourceName = "degraded-cluster"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "dorgu-system",
		}

		var nodes []*corev1.Node

		BeforeEach(func() {
			By("creating the dorgu-system namespace")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "dorgu-system",
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "dorgu-system"}, ns)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			}

			By("creating nodes with mixed ready states")
			nodes = []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "degraded-node-ready",
						Labels: map[string]string{
							"kubernetes.io/os": "linux",
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionTrue,
							},
						},
						Capacity: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
						},
						Allocatable: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("3800m"),
							corev1.ResourceMemory: resource.MustParse("7Gi"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "degraded-node-notready",
						Labels: map[string]string{
							"kubernetes.io/os": "linux",
						},
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionFalse,
								Reason: "KubeletNotReady",
							},
						},
						Capacity: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
						},
						Allocatable: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("3800m"),
							corev1.ResourceMemory: resource.MustParse("7Gi"),
						},
					},
				},
			}

			for _, node := range nodes {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &corev1.Node{})
				if errors.IsNotFound(err) {
					Expect(k8sClient.Create(ctx, node)).To(Succeed())
				}
			}

			By("creating the ClusterPersona")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			if err != nil && errors.IsNotFound(err) {
				resource := &dorguv1.ClusterPersona{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "dorgu-system",
					},
					Spec: dorguv1.ClusterPersonaSpec{
						Name:        resourceName,
						Environment: "staging",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup test nodes")
			for _, node := range nodes {
				n := &corev1.Node{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, n)
				if err == nil {
					Expect(k8sClient.Delete(ctx, n)).To(Succeed())
				}
			}

			By("Cleanup ClusterPersona")
			resource := &dorguv1.ClusterPersona{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set Degraded phase when some nodes are not ready", func() {
			By("Reconciling the ClusterPersona")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the phase is Degraded")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusterpersona.Status.Phase).To(Equal("Degraded"))
		})

		It("should correctly report node ready status", func() {
			By("Reconciling the ClusterPersona")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking node ready status")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			Expect(err).NotTo(HaveOccurred())

			readyCount := 0
			notReadyCount := 0
			for _, node := range clusterpersona.Status.Nodes {
				if node.Name == "degraded-node-ready" || node.Name == "degraded-node-notready" {
					if node.Ready {
						readyCount++
					} else {
						notReadyCount++
					}
				}
			}
			Expect(readyCount).To(Equal(1))
			Expect(notReadyCount).To(Equal(1))
		})
	})

	Context("When reconciling a deleted resource", func() {
		const resourceName = "deleted-cluster"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "dorgu-system",
		}

		It("should handle not found gracefully", func() {
			By("Reconciling a non-existent resource")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})
	})

	Context("When counting ApplicationPersonas", func() {
		const resourceName = "cluster-with-apps"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "dorgu-system",
		}

		var personas []*dorguv1.ApplicationPersona

		BeforeEach(func() {
			By("creating the dorgu-system namespace")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "dorgu-system",
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "dorgu-system"}, ns)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			}

			By("creating test ApplicationPersonas")
			personas = []*dorguv1.ApplicationPersona{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-app-1",
						Namespace: "default",
					},
					Spec: dorguv1.ApplicationPersonaSpec{
						Name: "test-app-1",
						Type: "api",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-app-2",
						Namespace: "default",
					},
					Spec: dorguv1.ApplicationPersonaSpec{
						Name: "test-app-2",
						Type: "web",
					},
				},
			}

			for _, persona := range personas {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: persona.Name, Namespace: persona.Namespace}, &dorguv1.ApplicationPersona{})
				if errors.IsNotFound(err) {
					Expect(k8sClient.Create(ctx, persona)).To(Succeed())
				}
			}

			By("creating the ClusterPersona")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			if err != nil && errors.IsNotFound(err) {
				resource := &dorguv1.ClusterPersona{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "dorgu-system",
					},
					Spec: dorguv1.ClusterPersonaSpec{
						Name:        resourceName,
						Environment: "development",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup test ApplicationPersonas")
			for _, persona := range personas {
				p := &dorguv1.ApplicationPersona{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: persona.Name, Namespace: persona.Namespace}, p)
				if err == nil {
					Expect(k8sClient.Delete(ctx, p)).To(Succeed())
				}
			}

			By("Cleanup ClusterPersona")
			resource := &dorguv1.ClusterPersona{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should count ApplicationPersonas in the cluster", func() {
			By("Reconciling the ClusterPersona")
			controllerReconciler := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the application count")
			clusterpersona := &dorguv1.ClusterPersona{}
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpersona)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusterpersona.Status.ApplicationCount).To(BeNumerically(">=", 2))
		})
	})
})
