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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// F-1: an incident in a protected namespace is still detected and recorded, but
// its context is never sent to the model, so it is recorded rule-based.
var _ = Describe("HealthCheckReconciler deny-list and the diagnosis LLM call", func() {
	var (
		testLogger = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))
		testCtx    = context.Background()
	)

	It("records a kube-system incident rule-based and never calls the model", func() {
		const namespace = "kube-system"
		const name = "hc-deny-memhog"

		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       dorguv1.ApplicationPersonaSpec{Name: name, Type: "worker"},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
		defer func() { _ = k8sClient.Delete(testCtx, persona) }()

		collector := &stubCollector{
			name: "test-collector",
			signals: []detection.Signal{{
				Type:     detection.SignalOOMKilled,
				Severity: detection.SeverityCritical,
				Category: detection.CategoryResource,
				Source:   "test-collector",
				Message:  "Container killed due to OOM",
				Resource: dorguv1.ResourceReference{Kind: "Pod", Name: name + "-abc123", Namespace: namespace},
				PersonaRef: &dorguv1.PersonaReference{
					Kind: "ApplicationPersona", Name: name, Namespace: namespace,
				},
				DetectedAt: time.Now(),
			}},
		}
		stub := &stubLLMClient{summary: "model narrative", action: "resource-adjustment"}
		checker := remediation.NewSafetyChecker(k8sClient, testLogger)

		r := &HealthCheckReconciler{
			Client:    k8sClient,
			Detection: detection.NewEngine(testLogger, collector),
			Diagnosis: diagnosis.NewEngine(testLogger,
				diagnosis.NewRuleBasedProvider(testLogger),
				diagnosis.NewAIProvider(stub, checker, testLogger),
			),
			EventStore:        &noopEventStore{},
			EventEmitter:      &noopEmitter{},
			Logger:            testLogger,
			ReconcileInterval: time.Second,
		}
		r.reconcile(testCtx)

		var im dorguv1.IncidentMemory
		incident := generateIncidentName(namespace, name, reasonOOMKilled)
		Eventually(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: incident, Namespace: namespace}, &im)
		}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		defer func() { _ = k8sClient.Delete(testCtx, &im) }()

		Expect(im.Spec.RootCause).NotTo(BeNil())
		Expect(im.Spec.RootCause.Provider).To(Equal("rule-based"))
		Expect(stub.calls).To(BeZero(), "a protected namespace must not reach the model")
	})
})
