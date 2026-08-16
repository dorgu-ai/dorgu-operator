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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
)

// stubCollector is a SignalCollector that returns predetermined signals.
type stubCollector struct {
	name    string
	signals []detection.Signal
	err     error
}

func (s *stubCollector) Name() string { return s.name }
func (s *stubCollector) Collect(_ context.Context) ([]detection.Signal, error) {
	return s.signals, s.err
}

var _ = Describe("HealthCheckReconciler", func() {
	var (
		testLogger = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))
		testCtx    = context.Background()
	)

	Context("generateIncidentName", func() {
		It("should create deterministic names", func() {
			name1 := generateIncidentName("default", "api-server", reasonOOMKilled)
			name2 := generateIncidentName("default", "api-server", reasonOOMKilled)
			Expect(name1).To(Equal(name2))
		})

		It("should create different names for different signals", func() {
			name1 := generateIncidentName("default", "api-server", reasonOOMKilled)
			name2 := generateIncidentName("default", "api-server", "CrashLoopBackOff")
			Expect(name1).NotTo(Equal(name2))
		})

		It("should start with im- prefix", func() {
			name := generateIncidentName("default", "api-server", reasonOOMKilled)
			Expect(name).To(HavePrefix("im-"))
		})

		It("should truncate long names to 253 chars", func() {
			var longName strings.Builder
			for range 300 {
				longName.WriteString("a")
			}
			name := generateIncidentName("default", longName.String(), reasonOOMKilled)
			Expect(len(name)).To(BeNumerically("<=", MaxIncidentNameLength))
		})
	})

	Context("sanitizeName", func() {
		It("should lowercase uppercase characters", func() {
			Expect(sanitizeName(reasonOOMKilled)).To(Equal("oomkilled"))
		})

		It("should replace non-alphanumeric chars with hyphens", func() {
			Expect(sanitizeName("some.name/here")).To(Equal("some-name-here"))
		})

		It("should keep valid characters unchanged", func() {
			Expect(sanitizeName("valid-name-123")).To(Equal("valid-name-123"))
		})
	})

	Context("primarySignalType", func() {
		It("should return the first contributing signal type", func() {
			diag := &diagnosis.Diagnosis{
				Contributing: []diagnosis.ContributingSignal{
					{Signal: detection.Signal{Type: detection.SignalOOMKilled}},
					{Signal: detection.Signal{Type: detection.SignalCrashLoopBackOff}},
				},
			}
			Expect(primarySignalType(diag)).To(Equal(detection.SignalOOMKilled))
		})

		It("should return Unknown when no contributing signals", func() {
			diag := &diagnosis.Diagnosis{}
			Expect(primarySignalType(diag)).To(Equal(detection.SignalType("Unknown")))
		})
	})

	Context("buildRootCause", func() {
		It("should build root cause from diagnosis", func() {
			diag := &diagnosis.Diagnosis{
				Summary:    "Container OOM killed due to memory limit",
				Confidence: 0.85,
				Provider:   "rule-based",
				Contributing: []diagnosis.ContributingSignal{
					{
						Signal: detection.Signal{Type: detection.SignalOOMKilled},
						Detail: "Pod exceeded memory limit",
					},
				},
			}
			rc := buildRootCause(diag)
			Expect(rc).NotTo(BeNil())
			Expect(rc.Summary).To(Equal("Container OOM killed due to memory limit"))
			Expect(rc.Confidence).To(Equal("0.85"))
			Expect(rc.Provider).To(Equal("rule-based"))
			Expect(rc.Contributing).To(HaveLen(1))
			Expect(rc.Contributing[0].Signal).To(Equal(reasonOOMKilled))
		})

		It("should return nil for empty diagnosis", func() {
			diag := &diagnosis.Diagnosis{}
			Expect(buildRootCause(diag)).To(BeNil())
		})
	})

	Context("toResourceRefs", func() {
		It("should set affected role on all refs", func() {
			refs := toResourceRefs([]dorguv1.ResourceReference{
				{Kind: "Pod", Name: "my-pod", Namespace: "default"},
				{Kind: "Node", Name: "node-1"},
			})
			Expect(refs).To(HaveLen(2))
			Expect(refs[0].Role).To(Equal("affected"))
			Expect(refs[1].Role).To(Equal("affected"))
		})
	})

	Context("timeEqual", func() {
		It("should return true for both nil", func() {
			Expect(timeEqual(nil, nil)).To(BeTrue())
		})

		It("should return false when one is nil", func() {
			now := metav1.Now()
			Expect(timeEqual(&now, nil)).To(BeFalse())
			Expect(timeEqual(nil, &now)).To(BeFalse())
		})

		It("should return true for equal times", func() {
			now := metav1.Now()
			Expect(timeEqual(&now, &now)).To(BeTrue())
		})
	})

	Context("signalKey", func() {
		It("should create consistent keys", func() {
			r := &HealthCheckReconciler{Logger: testLogger}
			diag := &diagnosis.Diagnosis{
				PersonaRef: &dorguv1.PersonaReference{
					Kind:      "ApplicationPersona",
					Name:      "api-server",
					Namespace: "default",
				},
				Category: "resource",
				Contributing: []diagnosis.ContributingSignal{
					{Signal: detection.Signal{Type: detection.SignalOOMKilled}},
				},
			}
			key := r.signalKey(diag)
			Expect(key).To(Equal("ApplicationPersona/default/api-server/resource/OOMKilled"))
		})
	})

	Context("incidentSignalKey", func() {
		It("should reconstruct signal key from incident", func() {
			im := &dorguv1.IncidentMemory{
				Spec: dorguv1.IncidentMemorySpec{
					PersonaRef: dorguv1.PersonaReference{
						Kind:      "ApplicationPersona",
						Name:      "api-server",
						Namespace: "default",
					},
					Category: "resource",
					Detection: dorguv1.DetectionInfo{
						Signal: reasonOOMKilled,
					},
				},
			}
			key := incidentSignalKey(im)
			Expect(key).To(Equal("ApplicationPersona/default/api-server/resource/OOMKilled"))
		})
	})

	Context("Integration with envtest", func() {
		It("should create IncidentMemory for OOMKilled signal", func() {
			// Create an ApplicationPersona.
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-test-app",
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: "hc-test-app",
					Type: "api",
				},
			}
			Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, persona)
			}()

			// Create a collector that returns an OOMKilled signal.
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
							Name:      "hc-test-app-abc123",
							Namespace: "default",
						},
						PersonaRef: &dorguv1.PersonaReference{
							Kind:      "ApplicationPersona",
							Name:      "hc-test-app",
							Namespace: "default",
						},
						DetectedAt: time.Now(),
					},
				},
			}

			detectionEngine := detection.NewEngine(testLogger, collector)
			ruleProvider := diagnosis.NewRuleBasedProvider(testLogger)
			diagnosisEngine := diagnosis.NewEngine(testLogger, ruleProvider)

			// Use a no-op event store and emitter.
			store := &noopEventStore{}
			emitter := &noopEmitter{}

			reconciler := &HealthCheckReconciler{
				Client:            k8sClient,
				Detection:         detectionEngine,
				Diagnosis:         diagnosisEngine,
				EventStore:        store,
				EventEmitter:      emitter,
				Logger:            testLogger,
				ReconcileInterval: time.Second,
			}

			// Run one reconciliation cycle.
			reconciler.reconcile(testCtx)

			// Verify IncidentMemory was created.
			expectedName := generateIncidentName("default", "hc-test-app", reasonOOMKilled)
			var im dorguv1.IncidentMemory
			Eventually(func() error {
				return k8sClient.Get(testCtx, types.NamespacedName{
					Name:      expectedName,
					Namespace: "default",
				}, &im)
			}, 5*time.Second, 100*time.Millisecond).Should(Succeed())

			Expect(im.Spec.Category).To(Equal("resource"))
			Expect(im.Spec.Severity).To(Equal("critical"))
			Expect(im.Spec.Detection.Signal).To(Equal(reasonOOMKilled))
			Expect(im.Spec.PersonaRef.Name).To(Equal("hc-test-app"))
			Expect(im.Labels[LabelSignal]).To(Equal(reasonOOMKilled))
			Expect(im.Labels[LabelCategory]).To(Equal("resource"))

			// Clean up.
			_ = k8sClient.Delete(testCtx, &im)
		})

		It("should deduplicate incidents on subsequent reconciliations", func() {
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-dedup-app",
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: "hc-dedup-app",
					Type: "api",
				},
			}
			Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, persona)
			}()

			collector := &stubCollector{
				name: "test-collector",
				signals: []detection.Signal{
					{
						Type:     detection.SignalCrashLoopBackOff,
						Severity: detection.SeverityCritical,
						Category: detection.CategoryHealth,
						Source:   "test-collector",
						Message:  "Container crash looping",
						Resource: dorguv1.ResourceReference{
							Kind:      "Pod",
							Name:      "hc-dedup-app-xyz789",
							Namespace: "default",
						},
						PersonaRef: &dorguv1.PersonaReference{
							Kind:      "ApplicationPersona",
							Name:      "hc-dedup-app",
							Namespace: "default",
						},
						DetectedAt: time.Now(),
					},
				},
			}

			detectionEngine := detection.NewEngine(testLogger, collector)
			diagnosisEngine := diagnosis.NewEngine(testLogger, diagnosis.NewRuleBasedProvider(testLogger))

			reconciler := &HealthCheckReconciler{
				Client:            k8sClient,
				Detection:         detectionEngine,
				Diagnosis:         diagnosisEngine,
				EventStore:        &noopEventStore{},
				EventEmitter:      &noopEmitter{},
				Logger:            testLogger,
				ReconcileInterval: time.Second,
			}

			// Run reconcile twice.
			reconciler.reconcile(testCtx)
			reconciler.reconcile(testCtx)

			// Verify only 1 IncidentMemory exists.
			var list dorguv1.IncidentMemoryList
			Expect(k8sClient.List(testCtx, &list,
				client.InNamespace("default"),
				client.MatchingLabels{
					LabelPersonaName: "hc-dedup-app",
				},
			)).To(Succeed())

			Expect(list.Items).To(HaveLen(1))
			// Occurrence count should have been incremented.
			Expect(list.Items[0].Status.OccurrenceCount).To(BeNumerically(">=", 2))

			// Clean up.
			for i := range list.Items {
				_ = k8sClient.Delete(testCtx, &list.Items[i])
			}
		})

		It("should auto-resolve incidents when signal clears", func() {
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-resolve-app",
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: "hc-resolve-app",
					Type: "api",
				},
			}
			Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, persona)
			}()

			// First: create an incident with a signal.
			collector := &stubCollector{
				name: "test-collector",
				signals: []detection.Signal{
					{
						Type:     detection.SignalImagePullBackOff,
						Severity: detection.SeverityWarning,
						Category: detection.CategoryDeployment,
						Source:   "test-collector",
						Message:  "Image pull back off",
						Resource: dorguv1.ResourceReference{
							Kind:      "Pod",
							Name:      "hc-resolve-app-pod",
							Namespace: "default",
						},
						PersonaRef: &dorguv1.PersonaReference{
							Kind:      "ApplicationPersona",
							Name:      "hc-resolve-app",
							Namespace: "default",
						},
						DetectedAt: time.Now(),
					},
				},
			}

			detectionEngine := detection.NewEngine(testLogger, collector)
			diagnosisEngine := diagnosis.NewEngine(testLogger, diagnosis.NewRuleBasedProvider(testLogger))

			reconciler := &HealthCheckReconciler{
				Client:            k8sClient,
				Detection:         detectionEngine,
				Diagnosis:         diagnosisEngine,
				EventStore:        &noopEventStore{},
				EventEmitter:      &noopEmitter{},
				Logger:            testLogger,
				ReconcileInterval: time.Second,
			}

			// Create the incident.
			reconciler.reconcile(testCtx)

			// Verify incident created.
			var list dorguv1.IncidentMemoryList
			Expect(k8sClient.List(testCtx, &list,
				client.InNamespace("default"),
				client.MatchingLabels{LabelPersonaName: "hc-resolve-app"},
			)).To(Succeed())
			Expect(list.Items).To(HaveLen(1))
			Expect(list.Items[0].Status.Phase).To(Equal("Detected"))

			// Now: remove the signal and backdate lastSeen to exceed grace period.
			im := &list.Items[0]
			im.Spec.Detection.LastSeen = metav1.Time{Time: time.Now().Add(-ResolutionGracePeriod - time.Minute)}
			Expect(k8sClient.Update(testCtx, im)).To(Succeed())

			// Switch to empty collector (no signals).
			emptyCollector := &stubCollector{name: "test-collector", signals: nil}
			reconciler.Detection = detection.NewEngine(testLogger, emptyCollector)

			// Run reconcile again — should resolve.
			reconciler.reconcile(testCtx)

			// Verify incident resolved.
			var resolved dorguv1.IncidentMemory
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name:      im.Name,
				Namespace: im.Namespace,
			}, &resolved)).To(Succeed())
			Expect(resolved.Status.Phase).To(Equal("Resolved"))
			Expect(resolved.Spec.Resolution).NotTo(BeNil())
			Expect(resolved.Spec.Resolution.Action).To(Equal("auto-resolved"))

			// Clean up.
			_ = k8sClient.Delete(testCtx, &resolved)
		})

		It("should create separate incidents for different issues", func() {
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-multi-app",
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: "hc-multi-app",
					Type: "api",
				},
			}
			Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, persona)
			}()

			collector := &stubCollector{
				name: "test-collector",
				signals: []detection.Signal{
					{
						Type:     detection.SignalOOMKilled,
						Severity: detection.SeverityCritical,
						Category: detection.CategoryResource,
						Source:   "test-collector",
						Message:  "OOM killed",
						Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "hc-multi-app-pod1", Namespace: "default"},
						PersonaRef: &dorguv1.PersonaReference{
							Kind: "ApplicationPersona", Name: "hc-multi-app", Namespace: "default",
						},
						DetectedAt: time.Now(),
					},
					{
						Type:     detection.SignalCrashLoopBackOff,
						Severity: detection.SeverityCritical,
						Category: detection.CategoryHealth,
						Source:   "test-collector",
						Message:  "Crash loop",
						Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "hc-multi-app-pod2", Namespace: "default"},
						PersonaRef: &dorguv1.PersonaReference{
							Kind: "ApplicationPersona", Name: "hc-multi-app", Namespace: "default",
						},
						DetectedAt: time.Now(),
					},
				},
			}

			detectionEngine := detection.NewEngine(testLogger, collector)
			diagnosisEngine := diagnosis.NewEngine(testLogger, diagnosis.NewRuleBasedProvider(testLogger))

			reconciler := &HealthCheckReconciler{
				Client:            k8sClient,
				Detection:         detectionEngine,
				Diagnosis:         diagnosisEngine,
				EventStore:        &noopEventStore{},
				EventEmitter:      &noopEmitter{},
				Logger:            testLogger,
				ReconcileInterval: time.Second,
			}

			reconciler.reconcile(testCtx)

			// Verify 2 separate IncidentMemory CRDs.
			var list dorguv1.IncidentMemoryList
			Expect(k8sClient.List(testCtx, &list,
				client.InNamespace("default"),
				client.MatchingLabels{LabelPersonaName: "hc-multi-app"},
			)).To(Succeed())
			Expect(list.Items).To(HaveLen(2))

			// Verify different signals.
			signals := map[string]bool{}
			for _, im := range list.Items {
				signals[im.Spec.Detection.Signal] = true
			}
			Expect(signals).To(HaveKey(reasonOOMKilled))
			Expect(signals).To(HaveKey("CrashLoopBackOff"))

			// Clean up.
			for i := range list.Items {
				_ = k8sClient.Delete(testCtx, &list.Items[i])
			}
		})

		It("should update persona status with active incident count", func() {
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hc-status-app",
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: "hc-status-app",
					Type: "api",
				},
			}
			Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, persona)
			}()

			collector := &stubCollector{
				name: "test-collector",
				signals: []detection.Signal{
					{
						Type:     detection.SignalOOMKilled,
						Severity: detection.SeverityCritical,
						Category: detection.CategoryResource,
						Source:   "test-collector",
						Message:  "OOM",
						Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "hc-status-app-pod", Namespace: "default"},
						PersonaRef: &dorguv1.PersonaReference{
							Kind: "ApplicationPersona", Name: "hc-status-app", Namespace: "default",
						},
						DetectedAt: time.Now(),
					},
				},
			}

			reconciler := &HealthCheckReconciler{
				Client:            k8sClient,
				Detection:         detection.NewEngine(testLogger, collector),
				Diagnosis:         diagnosis.NewEngine(testLogger, diagnosis.NewRuleBasedProvider(testLogger)),
				EventStore:        &noopEventStore{},
				EventEmitter:      &noopEmitter{},
				Logger:            testLogger,
				ReconcileInterval: time.Second,
			}

			reconciler.reconcile(testCtx)

			// The IncidentController is responsible for syncing persona status.
			// Find the created incident and run the incident controller reconcile.
			var list dorguv1.IncidentMemoryList
			Expect(k8sClient.List(testCtx, &list, client.InNamespace("default"),
				client.MatchingLabels{LabelPersonaName: "hc-status-app"})).To(Succeed())
			Expect(list.Items).NotTo(BeEmpty())

			incidentCtrl := &IncidentController{
				Client: k8sClient,
				Logger: testLogger,
			}
			for _, im := range list.Items {
				_, err := incidentCtrl.Reconcile(testCtx, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: im.Name, Namespace: im.Namespace},
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify persona status updated.
			var updated dorguv1.ApplicationPersona
			Eventually(func() int32 {
				_ = k8sClient.Get(testCtx, types.NamespacedName{Name: "hc-status-app", Namespace: "default"}, &updated)
				return updated.Status.ActiveIncidents
			}, 5*time.Second, 100*time.Millisecond).Should(BeNumerically(">=", 1))

			Expect(updated.Status.LastIncidentTime).NotTo(BeNil())

			// Clean up incidents.
			for i := range list.Items {
				_ = k8sClient.Delete(testCtx, &list.Items[i])
			}
		})

		It("should not create incidents when cluster is healthy", func() {
			// Empty collector — no signals.
			collector := &stubCollector{name: "test-collector", signals: nil}

			reconciler := &HealthCheckReconciler{
				Client:            k8sClient,
				Detection:         detection.NewEngine(testLogger, collector),
				Diagnosis:         diagnosis.NewEngine(testLogger, diagnosis.NewRuleBasedProvider(testLogger)),
				EventStore:        &noopEventStore{},
				EventEmitter:      &noopEmitter{},
				Logger:            testLogger,
				ReconcileInterval: time.Second,
			}

			reconciler.reconcile(testCtx)

			// No new incidents should be created.
			var list dorguv1.IncidentMemoryList
			Expect(k8sClient.List(testCtx, &list,
				client.InNamespace("default"),
				client.MatchingLabels{LabelSignal: "NoSignal"},
			)).To(Succeed())
			Expect(list.Items).To(BeEmpty())
		})
	})
})

// noopEventStore is a no-op EventStore implementation for testing.
type noopEventStore struct{}

func (s *noopEventStore) Store(_ context.Context, _ *events.InternalEvent) error { return nil }
func (s *noopEventStore) Query(_ context.Context, _ events.EventFilter) ([]events.InternalEvent, error) {
	return nil, nil
}
func (s *noopEventStore) Count(_ context.Context, _ events.EventFilter) (int, error) { return 0, nil }

// noopEmitter is a no-op Emitter implementation for testing.
type noopEmitter struct{}

func (e *noopEmitter) Emit(_ context.Context, _ *events.InternalEvent) error { return nil }
