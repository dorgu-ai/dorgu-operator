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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

var _ = Describe("IncidentController", func() {
	var (
		testLogger = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))
		testCtx    = context.Background()
	)

	Context("Label management", func() {
		It("should ensure all required labels are set", func() {
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ic-label-app",
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: "ic-label-app",
					Type: "api",
				},
			}
			Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, persona)
			}()

			// Create IncidentMemory without labels.
			now := metav1.Now()
			im := &dorguv1.IncidentMemory{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ic-test-labels",
					Namespace: "default",
				},
				Spec: dorguv1.IncidentMemorySpec{
					PersonaRef: dorguv1.PersonaReference{
						Kind:      "ApplicationPersona",
						Name:      "ic-label-app",
						Namespace: "default",
					},
					Category: "resource",
					Severity: "critical",
					Detection: dorguv1.DetectionInfo{
						Signal:    "OOMKilled",
						Source:    "test",
						FirstSeen: now,
						LastSeen:  now,
						AffectedResources: []dorguv1.ResourceReference{
							{Kind: "Pod", Name: "test-pod", Namespace: "default"},
						},
					},
				},
			}
			Expect(k8sClient.Create(testCtx, im)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, im)
			}()

			// Set initial status so the controller has something to work with.
			im.Status = dorguv1.IncidentMemoryStatus{
				Phase:           "Detected",
				OccurrenceCount: 1,
				LastOccurrence:  &now,
			}
			Expect(k8sClient.Status().Update(testCtx, im)).To(Succeed())

			// Reconcile.
			controller := &IncidentController{
				Client: k8sClient,
				Logger: testLogger,
			}
			_, err := controller.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "ic-test-labels",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify labels.
			var updated dorguv1.IncidentMemory
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name:      "ic-test-labels",
				Namespace: "default",
			}, &updated)).To(Succeed())

			Expect(updated.Labels[LabelPersonaKind]).To(Equal("ApplicationPersona"))
			Expect(updated.Labels[LabelPersonaName]).To(Equal("ic-label-app"))
			Expect(updated.Labels[LabelPersonaNamespace]).To(Equal("default"))
			Expect(updated.Labels[LabelCategory]).To(Equal("resource"))
			Expect(updated.Labels[LabelSeverity]).To(Equal("critical"))
			Expect(updated.Labels[LabelSignal]).To(Equal("OOMKilled"))
			Expect(updated.Labels[LabelPhase]).To(Equal("Detected"))
		})
	})

	Context("Condition updates", func() {
		It("should set Detected condition for active incidents", func() {
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ic-cond-app",
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: "ic-cond-app",
					Type: "api",
				},
			}
			Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, persona)
			}()

			now := metav1.Now()
			im := &dorguv1.IncidentMemory{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ic-test-conditions",
					Namespace: "default",
				},
				Spec: dorguv1.IncidentMemorySpec{
					PersonaRef: dorguv1.PersonaReference{
						Kind:      "ApplicationPersona",
						Name:      "ic-cond-app",
						Namespace: "default",
					},
					Category: "health",
					Severity: "warning",
					Detection: dorguv1.DetectionInfo{
						Signal:    "CrashLoopBackOff",
						Source:    "test",
						FirstSeen: now,
						LastSeen:  now,
						AffectedResources: []dorguv1.ResourceReference{
							{Kind: "Pod", Name: "test-pod", Namespace: "default"},
						},
					},
				},
			}
			Expect(k8sClient.Create(testCtx, im)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, im)
			}()

			// Set status.
			im.Status = dorguv1.IncidentMemoryStatus{
				Phase:           "Detected",
				OccurrenceCount: 1,
				LastOccurrence:  &now,
			}
			Expect(k8sClient.Status().Update(testCtx, im)).To(Succeed())

			controller := &IncidentController{
				Client: k8sClient,
				Logger: testLogger,
			}
			_, err := controller.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "ic-test-conditions",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			var updated dorguv1.IncidentMemory
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name:      "ic-test-conditions",
				Namespace: "default",
			}, &updated)).To(Succeed())

			// Detected should be True.
			var detectedCond *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == ConditionDetected {
					detectedCond = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(detectedCond).NotTo(BeNil())
			Expect(detectedCond.Status).To(Equal(metav1.ConditionTrue))

			// Resolved should be False.
			var resolvedCond *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == ConditionResolved {
					resolvedCond = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(resolvedCond).NotTo(BeNil())
			Expect(resolvedCond.Status).To(Equal(metav1.ConditionFalse))

			// Recurring should be False (occurrence count = 1).
			var recurringCond *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == ConditionRecurring {
					recurringCond = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(recurringCond).NotTo(BeNil())
			Expect(recurringCond.Status).To(Equal(metav1.ConditionFalse))
		})

		It("should set Recurring condition when occurrence count > 1", func() {
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ic-recur-app",
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: "ic-recur-app",
					Type: "api",
				},
			}
			Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, persona)
			}()

			now := metav1.Now()
			im := &dorguv1.IncidentMemory{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ic-test-recurring",
					Namespace: "default",
				},
				Spec: dorguv1.IncidentMemorySpec{
					PersonaRef: dorguv1.PersonaReference{
						Kind:      "ApplicationPersona",
						Name:      "ic-recur-app",
						Namespace: "default",
					},
					Category: "resource",
					Severity: "critical",
					Detection: dorguv1.DetectionInfo{
						Signal:    "OOMKilled",
						Source:    "test",
						FirstSeen: now,
						LastSeen:  now,
						AffectedResources: []dorguv1.ResourceReference{
							{Kind: "Pod", Name: "test-pod", Namespace: "default"},
						},
					},
				},
			}
			Expect(k8sClient.Create(testCtx, im)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, im)
			}()

			// Set status with occurrence > 1.
			im.Status = dorguv1.IncidentMemoryStatus{
				Phase:           "Detected",
				OccurrenceCount: 3,
				LastOccurrence:  &now,
			}
			Expect(k8sClient.Status().Update(testCtx, im)).To(Succeed())

			controller := &IncidentController{
				Client: k8sClient,
				Logger: testLogger,
			}
			_, err := controller.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "ic-test-recurring",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			var updated dorguv1.IncidentMemory
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name:      "ic-test-recurring",
				Namespace: "default",
			}, &updated)).To(Succeed())

			var recurringCond *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == ConditionRecurring {
					recurringCond = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(recurringCond).NotTo(BeNil())
			Expect(recurringCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(recurringCond.Reason).To(Equal("MultipleOccurrences"))
		})
	})

	Context("Persona sync", func() {
		It("should update persona active incident count", func() {
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ic-sync-app",
					Namespace: "default",
				},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name: "ic-sync-app",
					Type: "api",
				},
			}
			Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, persona)
			}()

			now := metav1.Now()
			im := &dorguv1.IncidentMemory{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ic-test-sync",
					Namespace: "default",
					Labels: map[string]string{
						LabelPersonaKind: "ApplicationPersona",
						LabelPersonaName: "ic-sync-app",
					},
				},
				Spec: dorguv1.IncidentMemorySpec{
					PersonaRef: dorguv1.PersonaReference{
						Kind:      "ApplicationPersona",
						Name:      "ic-sync-app",
						Namespace: "default",
					},
					Category: "resource",
					Severity: "critical",
					Detection: dorguv1.DetectionInfo{
						Signal:    "OOMKilled",
						Source:    "test",
						FirstSeen: now,
						LastSeen:  now,
						AffectedResources: []dorguv1.ResourceReference{
							{Kind: "Pod", Name: "test-pod", Namespace: "default"},
						},
					},
				},
			}
			Expect(k8sClient.Create(testCtx, im)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(testCtx, im)
			}()

			// Set active status.
			im.Status = dorguv1.IncidentMemoryStatus{
				Phase:           "Detected",
				OccurrenceCount: 1,
				LastOccurrence:  &now,
			}
			Expect(k8sClient.Status().Update(testCtx, im)).To(Succeed())

			controller := &IncidentController{
				Client: k8sClient,
				Logger: testLogger,
			}
			_, err := controller.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "ic-test-sync",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify persona status.
			var updatedPersona dorguv1.ApplicationPersona
			Eventually(func() int32 {
				_ = k8sClient.Get(testCtx, types.NamespacedName{
					Name: "ic-sync-app", Namespace: "default",
				}, &updatedPersona)
				return updatedPersona.Status.ActiveIncidents
			}, 5*time.Second, 100*time.Millisecond).Should(Equal(int32(1)))

			Expect(updatedPersona.Status.LastIncidentTime).NotTo(BeNil())
		})
	})

	Context("ensureLabels", func() {
		It("should initialize labels when nil", func() {
			controller := &IncidentController{Logger: testLogger}
			im := &dorguv1.IncidentMemory{
				Spec: dorguv1.IncidentMemorySpec{
					PersonaRef: dorguv1.PersonaReference{
						Kind:      "ApplicationPersona",
						Name:      "test",
						Namespace: "default",
					},
					Category: "resource",
					Severity: "critical",
					Detection: dorguv1.DetectionInfo{
						Signal: "OOMKilled",
					},
				},
				Status: dorguv1.IncidentMemoryStatus{
					Phase: "Detected",
				},
			}

			changed := controller.ensureLabels(im)
			Expect(changed).To(BeTrue())
			Expect(im.Labels).To(HaveLen(7))
			Expect(im.Labels[LabelPersonaKind]).To(Equal("ApplicationPersona"))
		})

		It("should return false when labels already correct", func() {
			controller := &IncidentController{Logger: testLogger}
			im := &dorguv1.IncidentMemory{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelPersonaKind:      "ApplicationPersona",
						LabelPersonaName:      "test",
						LabelPersonaNamespace: "default",
						LabelCategory:         "resource",
						LabelSeverity:         "critical",
						LabelSignal:           "OOMKilled",
						LabelPhase:            "Detected",
					},
				},
				Spec: dorguv1.IncidentMemorySpec{
					PersonaRef: dorguv1.PersonaReference{
						Kind:      "ApplicationPersona",
						Name:      "test",
						Namespace: "default",
					},
					Category: "resource",
					Severity: "critical",
					Detection: dorguv1.DetectionInfo{
						Signal: "OOMKilled",
					},
				},
				Status: dorguv1.IncidentMemoryStatus{
					Phase: "Detected",
				},
			}

			changed := controller.ensureLabels(im)
			Expect(changed).To(BeFalse())
		})
	})

	Context("updateConditions", func() {
		It("should set resolved condition when phase is Resolved", func() {
			controller := &IncidentController{Logger: testLogger}
			im := &dorguv1.IncidentMemory{
				Status: dorguv1.IncidentMemoryStatus{
					Phase:           "Resolved",
					OccurrenceCount: 1,
				},
			}

			controller.updateConditions(im)

			var resolvedCond *metav1.Condition
			for i := range im.Status.Conditions {
				if im.Status.Conditions[i].Type == ConditionResolved {
					resolvedCond = &im.Status.Conditions[i]
					break
				}
			}
			Expect(resolvedCond).NotTo(BeNil())
			Expect(resolvedCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(resolvedCond.Reason).To(Equal("SignalCleared"))
		})
	})

	Context("Not found handling", func() {
		It("should handle deleted IncidentMemory gracefully", func() {
			controller := &IncidentController{
				Client: k8sClient,
				Logger: testLogger,
			}
			result, err := controller.Reconcile(testCtx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "non-existent-incident",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})
})
