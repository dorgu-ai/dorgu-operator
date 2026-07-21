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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// This exercises WS8 F1: the manager must register the spec.nodeName Pod field
// index (detection.RegisterPodNodeNameIndex) so the resource-saturation checker
// can list pods-by-node. Against real envtest, an unregistered index makes the
// cache reject the field selector with
// "Index with name field:spec.nodeName does not exist".
var _ = Describe("Pod spec.nodeName field index (WS8 F1)", func() {
	It("lists pods scoped to a node through the registered index", func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 scheme.Scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())

		// Register exactly what main.go registers at startup.
		Expect(detection.RegisterPodNodeNameIndex(context.Background(), mgr.GetFieldIndexer())).To(Succeed())

		mgrCtx, mgrCancel := context.WithCancel(context.Background())
		DeferCleanup(mgrCancel)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		const ns = "field-index-ns"
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			}))
		})

		// Create pods pre-scheduled onto a node: spec.nodeName is immutable after
		// creation (envtest has no scheduler), so it must be set up front.
		makePod := func(name, node string) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: corev1.PodSpec{
					NodeName:   node,
					Containers: []corev1.Container{{Name: "c", Image: "nginx:latest"}},
				},
			}
			Expect(k8sClient.Create(context.Background(), pod)).To(Succeed())
		}

		makePod("pod-on-a", "node-a")
		makePod("pod-on-b", "node-b")

		// The cached manager client must resolve the field selector (no
		// "index does not exist" error) and return only node-a's pod.
		var onA corev1.PodList
		Eventually(func(g Gomega) {
			g.Expect(mgr.GetClient().List(mgrCtx, &onA,
				client.InNamespace(ns),
				client.MatchingFields{detection.PodNodeNameIndex: "node-a"},
			)).To(Succeed())
			g.Expect(onA.Items).To(HaveLen(1))
			g.Expect(onA.Items[0].Name).To(Equal("pod-on-a"))
		}).Should(Succeed())
	})
})
