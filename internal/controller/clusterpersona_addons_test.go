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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("OpenObserve Addon Discovery", func() {
	ctx := context.Background()

	Context("checkAddon for openobserve", func() {
		It("returns Installed=false when namespace does not exist", func() {
			By("ensuring the openobserve namespace does not exist")
			ns := &corev1.Namespace{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "openobserve"}, ns)
			if err == nil {
				Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
			}

			By("calling checkAddon for openobserve")
			r := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			addon := r.checkAddon(ctx, "openobserve", "openobserve", "monitoring")

			By("verifying the addon is not installed")
			Expect(addon.Installed).To(BeFalse())
		})

		It("returns Installed=true when matching pod exists in namespace", func() {
			By("creating the openobserve namespace")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "openobserve",
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "openobserve"}, ns)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			}

			By("creating a running openobserve pod")
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "openobserve-0",
					Namespace: "openobserve",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "openobserve",
							Image: "openobserve/openobserve:0.10.2",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			By("patching pod status to Running")
			pod.Status.Phase = corev1.PodRunning
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			By("calling checkAddon for openobserve")
			r := &ClusterPersonaReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			addon := r.checkAddon(ctx, "openobserve", "openobserve", "monitoring")

			By("verifying the addon is installed with correct version and health")
			Expect(addon.Installed).To(BeTrue())
			Expect(addon.Version).To(Equal("0.10.2"))
			Expect(addon.Healthy).NotTo(BeNil())
			Expect(*addon.Healthy).To(BeTrue())

			By("cleaning up")
			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})
	})
})
