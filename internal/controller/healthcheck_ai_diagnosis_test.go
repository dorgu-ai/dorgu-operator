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
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
)

// stubLLMClient returns a fixed enhancement and counts its calls. It mirrors what
// the real response parsers produce: an enhanced summary, a recommended action,
// and no confidence adjustment.
type stubLLMClient struct {
	summary string
	action  string
	calls   int
}

func (s *stubLLMClient) EnhanceDiagnosis(_ context.Context, _ llm.DiagnosisRequest) (*llm.DiagnosisResponse, error) {
	s.calls++
	return &llm.DiagnosisResponse{
		EnhancedSummary:   s.summary,
		RecommendedAction: s.action,
	}, nil
}

func (s *stubLLMClient) Provider() string { return "stub" }

// F-05: the AI diagnosis has to reach the persisted record, not just the log.
// The operator used to log {"provider": "ai-enhanced", "count": 2} while every
// IncidentMemory in the cluster read Provider: rule-based, so users paid for
// calls they could never see.
var _ = Describe("HealthCheckReconciler AI diagnosis persistence", func() {
	var (
		testLogger = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))
		testCtx    = context.Background()
	)

	// aiReconciler wires detection, a rule-based provider and a real AIProvider
	// over the stub LLM exactly as cmd/main.go does.
	newAIReconciler := func(personaName string, stub *stubLLMClient) *HealthCheckReconciler {
		collector := &stubCollector{
			name: "test-collector",
			signals: []detection.Signal{
				{
					Type:     detection.SignalOOMKilled,
					Severity: detection.SeverityCritical,
					Category: detection.CategoryResource,
					Source:   "test-collector",
					Message:  "Container killed due to OOM",
					Resource: dorguv1.ResourceReference{
						Kind:      "Pod",
						Name:      personaName + "-abc123",
						Namespace: "default",
					},
					PersonaRef: &dorguv1.PersonaReference{
						Kind:      "ApplicationPersona",
						Name:      personaName,
						Namespace: "default",
					},
					DetectedAt: time.Now(),
				},
			},
		}

		return &HealthCheckReconciler{
			Client:    k8sClient,
			Detection: detection.NewEngine(testLogger, collector),
			Diagnosis: diagnosis.NewEngine(testLogger,
				diagnosis.NewRuleBasedProvider(testLogger),
				diagnosis.NewAIProvider(stub, testLogger),
			),
			EventStore:        &noopEventStore{},
			EventEmitter:      &noopEmitter{},
			Logger:            testLogger,
			ReconcileInterval: time.Second,
		}
	}

	createPersona := func(name string) *dorguv1.ApplicationPersona {
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       dorguv1.ApplicationPersonaSpec{Name: name, Type: "worker"},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
		return persona
	}

	It("persists the AI-enhanced diagnosis on a newly created IncidentMemory", func() {
		persona := createPersona("hc-ai-new")
		defer func() { _ = k8sClient.Delete(testCtx, persona) }()

		stub := &stubLLMClient{
			summary: "hc-ai-new needs about 120M but its limit is 48Mi, so the kernel kills it on startup.",
			action:  "resource-adjustment",
		}

		newAIReconciler("hc-ai-new", stub).reconcile(testCtx)

		Expect(stub.calls).To(BeNumerically(">", 0), "the AI path never ran")

		var im dorguv1.IncidentMemory
		name := generateIncidentName("default", "hc-ai-new", reasonOOMKilled)
		Eventually(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &im)
		}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		defer func() { _ = k8sClient.Delete(testCtx, &im) }()

		Expect(im.Spec.RootCause).NotTo(BeNil())
		Expect(im.Spec.RootCause.Provider).To(Equal("ai-enhanced"))
		Expect(im.Spec.RootCause.Summary).To(Equal(stub.summary))
		Expect(im.Spec.Detection.Source).To(Equal("ai-enhanced"))
	})

	It("upgrades an existing rule-based record once the AI diagnosis arrives", func() {
		persona := createPersona("hc-ai-existing")
		defer func() { _ = k8sClient.Delete(testCtx, persona) }()

		// First cycle: rule-based only, as an operator installed without a key.
		ruleOnly := newAIReconciler("hc-ai-existing", &stubLLMClient{})
		ruleOnly.Diagnosis = diagnosis.NewEngine(testLogger, diagnosis.NewRuleBasedProvider(testLogger))
		ruleOnly.reconcile(testCtx)

		var im dorguv1.IncidentMemory
		name := generateIncidentName("default", "hc-ai-existing", reasonOOMKilled)
		Eventually(func() error {
			return k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &im)
		}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
		defer func() { _ = k8sClient.Delete(testCtx, &im) }()
		Expect(im.Spec.RootCause).NotTo(BeNil())
		Expect(im.Spec.RootCause.Provider).To(Equal("rule-based"))

		// Second cycle: the same incident, now with AI configured.
		stub := &stubLLMClient{
			summary: "Repeated OOM kills on hc-ai-existing point at a limit set below the real working set.",
			action:  "resource-adjustment",
		}
		newAIReconciler("hc-ai-existing", stub).reconcile(testCtx)

		Eventually(func() string {
			var updated dorguv1.IncidentMemory
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &updated); err != nil {
				return ""
			}
			if updated.Spec.RootCause == nil {
				return ""
			}
			return updated.Spec.RootCause.Provider
		}, 5*time.Second, 100*time.Millisecond).Should(Equal("ai-enhanced"))
	})
})
