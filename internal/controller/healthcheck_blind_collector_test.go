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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

// B2b: a collector that cannot look is not a collector that saw nothing. The
// health cycle used to treat the two the same, so a cycle in which detection
// was blind resolved every open incident whose workload could not be shown to
// be running.
var _ = Describe("HealthCheckReconciler with a blind collector", func() {
	var (
		testLogger = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))
		testCtx    = context.Background()
	)

	// openStaleIncident records an incident for a persona with no workload and
	// backdates it past the grace period, so only the cycle's own evidence can
	// keep it open.
	openStaleIncident := func(name string) (*HealthCheckReconciler, *dorguv1.IncidentMemory) {
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       dorguv1.ApplicationPersonaSpec{Name: name, Type: "api"},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, persona) })

		seeing := &stubCollector{
			name: "pods",
			signals: []detection.Signal{{
				Type:     detection.SignalImagePullBackOff,
				Severity: detection.SeverityWarning,
				Category: detection.CategoryDeployment,
				Source:   "pods",
				Message:  "Image pull back off",
				Resource: dorguv1.ResourceReference{Kind: "Pod", Name: name + "-pod", Namespace: "default"},
				PersonaRef: &dorguv1.PersonaReference{
					Kind: "ApplicationPersona", Name: name, Namespace: "default",
				},
				DetectedAt: time.Now(),
			}},
		}
		r := &HealthCheckReconciler{
			Client:            k8sClient,
			Detection:         detection.NewEngine(testLogger, seeing),
			Diagnosis:         diagnosis.NewEngine(testLogger, diagnosis.NewRuleBasedProvider(testLogger)),
			EventStore:        &noopEventStore{},
			EventEmitter:      &noopEmitter{},
			Logger:            testLogger,
			ReconcileInterval: time.Second,
		}
		r.reconcile(testCtx)

		var list dorguv1.IncidentMemoryList
		Expect(k8sClient.List(testCtx, &list,
			client.InNamespace("default"),
			client.MatchingLabels{LabelPersonaName: name},
		)).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		im := &list.Items[0]
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, im) })

		im.Spec.Detection.LastSeen = metav1.Time{Time: time.Now().Add(-ResolutionGracePeriod - time.Minute)}
		Expect(k8sClient.Update(testCtx, im)).To(Succeed())
		return r, im
	}

	phaseOf := func(im *dorguv1.IncidentMemory) string {
		var got dorguv1.IncidentMemory
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: im.Name, Namespace: im.Namespace}, &got)).To(Succeed())
		return got.Status.Phase
	}

	It("keeps incidents open when every collector fails", func() {
		r, im := openStaleIncident("hc-blind-all")

		r.Detection = detection.NewEngine(testLogger,
			&stubCollector{name: "pods", err: errors.New("forbidden")},
			&stubCollector{name: "nodes", err: errors.New("connection refused")},
		)
		r.reconcile(testCtx)

		Expect(phaseOf(im)).To(Equal("Detected"),
			"a cycle that could not look at the cluster must not resolve anything")
	})

	It("keeps incidents open when one collector fails and the others see nothing", func() {
		r, im := openStaleIncident("hc-blind-partial")

		r.Detection = detection.NewEngine(testLogger,
			&stubCollector{name: "nodes"},
			&stubCollector{name: "pods", err: errors.New("timeout")},
		)
		r.reconcile(testCtx)

		Expect(phaseOf(im)).To(Equal("Detected"))
	})

	It("still resolves once every collector can look again", func() {
		r, im := openStaleIncident("hc-blind-recover")

		r.Detection = detection.NewEngine(testLogger, &stubCollector{name: "pods"})
		r.reconcile(testCtx)

		Expect(phaseOf(im)).To(Equal("Resolved"),
			"the guard must only hold while collection is incomplete")
	})

	It("still records what the working collectors saw during a partial failure", func() {
		r, _ := openStaleIncident("hc-blind-record")
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: "hc-blind-new", Namespace: "default"},
			Spec:       dorguv1.ApplicationPersonaSpec{Name: "hc-blind-new", Type: "api"},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, persona) })

		r.Detection = detection.NewEngine(testLogger,
			&stubCollector{name: "nodes", err: errors.New("timeout")},
			&stubCollector{name: "pods", signals: []detection.Signal{{
				Type:     detection.SignalImagePullBackOff,
				Severity: detection.SeverityWarning,
				Category: detection.CategoryDeployment,
				Source:   "pods",
				Message:  "Image pull back off",
				Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "hc-blind-new-pod", Namespace: "default"},
				PersonaRef: &dorguv1.PersonaReference{
					Kind: "ApplicationPersona", Name: "hc-blind-new", Namespace: "default",
				},
				DetectedAt: time.Now(),
			}}},
		)
		r.reconcile(testCtx)

		var list dorguv1.IncidentMemoryList
		Expect(k8sClient.List(testCtx, &list,
			client.InNamespace("default"),
			client.MatchingLabels{LabelPersonaName: "hc-blind-new"},
		)).To(Succeed())
		Expect(list.Items).To(HaveLen(1), "partial results are still evidence of a problem")
		DeferCleanup(func() {
			for i := range list.Items {
				_ = k8sClient.Delete(testCtx, &list.Items[i])
			}
		})
	})
})
