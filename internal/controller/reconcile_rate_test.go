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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// reconcileCounts reads controller_runtime_reconcile_total for the named
// controllers from the process-wide registry.
func reconcileCounts(controllers ...string) map[string]float64 {
	families, err := ctrlmetrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	out := make(map[string]float64, len(controllers))
	for _, f := range families {
		if f.GetName() != "controller_runtime_reconcile_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != "controller" {
					continue
				}
				for _, want := range controllers {
					if l.GetValue() == want {
						out[want] += m.GetCounter().GetValue()
					}
				}
			}
		}
	}
	return out
}

// 1.14: is there a hot loop? A controller that writes a fresh timestamp into its
// own status on every pass wakes itself through the watch, forever. This starts
// the three CR-driven controllers against a real API server with an idle
// cluster (a few personas, each with its Deployment, plus the incidents they
// own), lets them settle, then counts reconciles over a window. The per-persona
// requeue is 60s, so a quiet window sees none; a hot loop would see one per
// second or more. Measured on this suite: 12, 2 and 8 reconciles while settling
// (3 per persona, 2 for the ClusterPersona, 2 per incident), then 0 in a 20s
// window, so 1.14 is closed as won't-do and this stays as the guard.
var _ = Describe("Reconcile rate on an idle cluster (task 1.14)", func() {
	const (
		ns       = "idle-rate"
		personas = 4
		settle   = 4 * time.Second
		window   = 10 * time.Second
		// A hot loop is one reconcile per object per second or worse, which is at
		// least personas*window (40) here. Anything past a handful is a loop.
		maxInWindow = 8
	)

	It("does not reconcile in a hot loop", func() {
		bg := context.Background()
		Expect(k8sClient.Create(bg, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(bg, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
		})

		for i := range personas {
			name := fmt.Sprintf("app-%d", i)
			labels := map[string]string{"app": name}
			Expect(k8sClient.Create(bg, &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To[int32](1),
					Selector: &metav1.LabelSelector{MatchLabels: labels},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:latest"}}},
					},
				},
			})).To(Succeed())
			Expect(k8sClient.Create(bg, &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       dorguv1.ApplicationPersonaSpec{Name: name, Type: "api"},
			})).To(Succeed())

			im := &dorguv1.IncidentMemory{
				ObjectMeta: metav1.ObjectMeta{Name: "im-" + name, Namespace: ns},
				Spec: dorguv1.IncidentMemorySpec{
					PersonaRef:  dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: name, Namespace: ns},
					Attribution: AttributionPersona,
					Category:    "resource",
					Severity:    "critical",
					Detection: dorguv1.DetectionInfo{
						Signal: "OOMKilled", Source: "rule-based",
						FirstSeen: metav1.Now(), LastSeen: metav1.Now(),
						AffectedResources: []dorguv1.ResourceReference{{Kind: "Pod", Name: name + "-pod", Namespace: ns}},
					},
				},
			}
			Expect(k8sClient.Create(bg, im)).To(Succeed())
			im.Status.Phase = PhaseDetected
			im.Status.OccurrenceCount = 1
			Expect(k8sClient.Status().Update(bg, im)).To(Succeed())
		}
		Expect(k8sClient.Create(bg, &dorguv1.ClusterPersona{
			ObjectMeta: metav1.ObjectMeta{Name: "idle-cluster"},
			Spec:       dorguv1.ClusterPersonaSpec{Name: "idle-cluster"},
		})).To(Succeed())
		DeferCleanup(func() {
			_ = client.IgnoreNotFound(k8sClient.Delete(bg, &dorguv1.ClusterPersona{ObjectMeta: metav1.ObjectMeta{Name: "idle-cluster"}}))
		})

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 scheme.Scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			// Other specs build managers with the same controllers in this process.
			Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		dc, err := discovery.NewDiscoveryClientForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		Expect((&ApplicationPersonaReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())
		Expect((&ClusterPersonaReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), DiscoveryClient: dc}).SetupWithManager(mgr)).To(Succeed())
		Expect((&IncidentController{Client: mgr.GetClient(), Logger: ctrl.Log.WithName("rate")}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel := context.WithCancel(bg)
		DeferCleanup(mgrCancel)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		names := []string{"applicationpersona", "clusterpersona", "incidentmemory"}
		time.Sleep(settle)
		before := reconcileCounts(names...)
		time.Sleep(window)
		after := reconcileCounts(names...)

		for _, n := range names {
			delta := after[n] - before[n]
			perMinute := delta / window.Minutes()
			line := fmt.Sprintf("RECONCILE-RATE controller=%s settled=%.0f window=%s reconciles=%.0f perMinute=%.1f",
				n, before[n], window, delta, perMinute)
			GinkgoWriter.Println(line)
			fmt.Println(line)
			Expect(delta).To(BeNumerically("<=", maxInWindow),
				"%s reconciled %.0f times in %s on an idle cluster: a hot loop", n, delta, window)
		}
	})
})
