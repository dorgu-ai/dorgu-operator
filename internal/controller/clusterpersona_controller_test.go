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
			// Phase will be "Unknown" when no nodes are present, or "Ready"/"Degraded" with nodes
			Expect(clusterpersona.Status.Phase).To(Or(Equal("Unknown"), Equal("Ready"), Equal("Degraded")))
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
			// Should have namespace summary populated
			Expect(clusterpersona.Status.Namespaces).NotTo(BeNil())
			// Should have at least the test namespaces + default + dorgu-system
			Expect(clusterpersona.Status.Namespaces.Total).To(BeNumerically(">=", 4))

			By("Cleanup test namespaces")
			Expect(k8sClient.Delete(ctx, ns1)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ns2)).To(Succeed())
		})
	})
})
