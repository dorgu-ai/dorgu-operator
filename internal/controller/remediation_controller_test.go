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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// mockCollector implements detection.SignalCollector for testing.
type remediationMockCollector struct {
	signals []detection.Signal
	err     error
}

func (m *remediationMockCollector) Name() string { return "mock" }
func (m *remediationMockCollector) Collect(_ context.Context) ([]detection.Signal, error) {
	return m.signals, m.err
}

var _ = Describe("RemediationController", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	var (
		testLogger = zap.New(zap.UseDevMode(true))
	)

	createTestPersona := func(name, namespace string) *dorguv1.ApplicationPersona {
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: dorguv1.ApplicationPersonaSpec{
				Name: name,
				Type: "api",
				Tier: "standard",
				Resources: &dorguv1.ResourceConstraints{
					Limits: &dorguv1.ResourceValues{
						Memory: "256Mi",
						CPU:    "250m",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, persona)).To(Succeed())
		return persona
	}

	createTestIncident := func(name, namespace, personaName, signal string) *dorguv1.IncidentMemory {
		incident := &dorguv1.IncidentMemory{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels: map[string]string{
					LabelPersonaKind:      "ApplicationPersona",
					LabelPersonaName:      personaName,
					LabelPersonaNamespace: namespace,
					LabelCategory:         "resource",
					LabelSeverity:         "critical",
					LabelSignal:           signal,
					LabelPhase:            PhaseDetected,
				},
			},
			Spec: dorguv1.IncidentMemorySpec{
				PersonaRef: dorguv1.PersonaReference{
					Kind:      "ApplicationPersona",
					Name:      personaName,
					Namespace: namespace,
				},
				Category: "resource",
				Severity: "critical",
				Detection: dorguv1.DetectionInfo{
					Signal:    signal,
					Source:    "pod-failure-detector",
					FirstSeen: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
					LastSeen:  metav1.NewTime(time.Now()),
					AffectedResources: []dorguv1.ResourceReference{
						{Kind: "Pod", Name: "test-pod", Namespace: namespace},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, incident)).To(Succeed())

		// Set status.
		incident.Status = dorguv1.IncidentMemoryStatus{
			Phase:           PhaseDetected,
			OccurrenceCount: 1,
		}
		Expect(k8sClient.Status().Update(ctx, incident)).To(Succeed())
		return incident
	}

	createTestAction := func(name, namespace, personaName, incidentName string) *dorguv1.RemediationAction {
		action := &dorguv1.RemediationAction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels: map[string]string{
					"dorgu.io/persona-kind":      "ApplicationPersona",
					"dorgu.io/persona-name":      personaName,
					"dorgu.io/persona-namespace": namespace,
				},
			},
			Spec: dorguv1.RemediationActionSpec{
				IncidentRef: dorguv1.IncidentReference{
					Name:      incidentName,
					Namespace: namespace,
				},
				PersonaRef: dorguv1.PersonaReference{
					Kind:      "ApplicationPersona",
					Name:      personaName,
					Namespace: namespace,
				},
				TrustLevel:  2,
				Confidence:  "0.85",
				Explanation: "Increase memory limit from 256Mi to 512Mi",
				Action: dorguv1.RemediationActionDetail{
					Type:          "persona-update",
					Patch:         &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
					PrePatchState: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)},
				},
				Approval: &dorguv1.ApprovalSpec{Required: true},
				Rollback: &dorguv1.RemediationRollbackSpec{
					Enabled:          true,
					HealthCheckAfter: &metav1.Duration{Duration: 1 * time.Second}, // Short for tests.
					MaxRetries:       1,
				},
			},
		}
		Expect(k8sClient.Create(ctx, action)).To(Succeed())
		return action
	}

	newController := func(signals []detection.Signal) *RemediationController {
		collector := &remediationMockCollector{signals: signals}
		engine := detection.NewEngine(testLogger, collector)
		return &RemediationController{
			Client:   k8sClient,
			Executor: remediation.NewExecutor(k8sClient, testLogger),
			Verifier: remediation.NewVerifier(engine, k8sClient, testLogger),
			Rollback: remediation.NewRollback(k8sClient, testLogger),
			Logger:   testLogger.WithName("remediation-controller-test"),
		}
	}

	Context("Pending phase", func() {
		It("should take no action on Pending RemediationAction", func() {
			persona := createTestPersona("rc-pending-persona", "default")
			incident := createTestIncident("rc-pending-incident", "default", persona.Name, "OOMKilled")
			action := createTestAction("rc-pending-action", "default", persona.Name, incident.Name)

			// Set status to Pending.
			action.Status = dorguv1.RemediationActionStatus{Phase: RemediationPhasePending}
			Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())

			controller := newController(nil)
			result, err := controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Persona should be unchanged.
			var updated dorguv1.ApplicationPersona
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(persona), &updated)).To(Succeed())
			Expect(updated.Spec.Resources.Limits.Memory).To(Equal("256Mi"))
		})
	})

	Context("Approved → Applying → Verifying → Completed lifecycle", func() {
		It("should complete the full lifecycle when verification is healthy", func() {
			persona := createTestPersona("rc-complete-persona", "default")
			incident := createTestIncident("rc-complete-incident", "default", persona.Name, "OOMKilled")
			action := createTestAction("rc-complete-action", "default", persona.Name, incident.Name)

			// Set status to Approved.
			action.Status = dorguv1.RemediationActionStatus{Phase: RemediationPhaseApproved}
			Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())

			// No signals → verification will be Healthy.
			controller := newController(nil)

			// Step 1: Approved → Applying.
			result, err := controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Verify persona was patched.
			var updatedPersona dorguv1.ApplicationPersona
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(persona), &updatedPersona)).To(Succeed())
			Expect(updatedPersona.Spec.Resources.Limits.Memory).To(Equal("512Mi"))

			// Verify action is in Applying phase.
			var updatedAction dorguv1.RemediationAction
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &updatedAction)).To(Succeed())
			Expect(updatedAction.Status.Phase).To(Equal(RemediationPhaseApplying))
			Expect(updatedAction.Status.AppliedAt).NotTo(BeNil())

			// Step 2: Wait, then Applying → Verifying.
			time.Sleep(2 * time.Second)
			result, err = controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeTrue())

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &updatedAction)).To(Succeed())
			Expect(updatedAction.Status.Phase).To(Equal(RemediationPhaseVerifying))

			// Step 3: Verifying → Completed.
			result, err = controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &updatedAction)).To(Succeed())
			Expect(updatedAction.Status.Phase).To(Equal(RemediationPhaseCompleted))
			Expect(updatedAction.Status.VerificationResult).To(Equal("Healthy"))

			// Verify incident has resolution.
			var updatedIncident dorguv1.IncidentMemory
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(incident), &updatedIncident)).To(Succeed())
			Expect(updatedIncident.Spec.Resolution).NotTo(BeNil())
			Expect(updatedIncident.Spec.Resolution.Outcome).To(Equal("resolved"))
			Expect(updatedIncident.Spec.Resolution.RemediationRef).NotTo(BeNil())
			Expect(updatedIncident.Spec.Resolution.RemediationRef.Name).To(Equal(action.Name))
		})
	})

	Context("Approved → Applying → Verifying → RolledBack lifecycle", func() {
		It("should rollback when verification shows degraded health", func() {
			persona := createTestPersona("rc-rollback-persona", "default")
			incident := createTestIncident("rc-rollback-incident", "default", persona.Name, "OOMKilled")
			action := createTestAction("rc-rollback-action", "default", persona.Name, incident.Name)

			// Set status to Approved.
			action.Status = dorguv1.RemediationActionStatus{Phase: RemediationPhaseApproved}
			Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())

			// Signal still present → Degraded.
			signals := []detection.Signal{
				{
					Type:     detection.SignalOOMKilled,
					Severity: detection.SeverityCritical,
					Category: detection.CategoryResource,
					Source:   "pod-failure-detector",
					PersonaRef: &dorguv1.PersonaReference{
						Kind:      "ApplicationPersona",
						Name:      persona.Name,
						Namespace: "default",
					},
					DetectedAt: time.Now(),
				},
			}
			controller := newController(signals)

			// Step 1: Approved → Applying.
			_, err := controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify persona was patched to 512Mi.
			var updatedPersona dorguv1.ApplicationPersona
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(persona), &updatedPersona)).To(Succeed())
			Expect(updatedPersona.Spec.Resources.Limits.Memory).To(Equal("512Mi"))

			// Step 2: Wait, Applying → Verifying.
			time.Sleep(2 * time.Second)
			var updatedAction dorguv1.RemediationAction
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &updatedAction)).To(Succeed())

			_, err = controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &updatedAction)).To(Succeed())
			Expect(updatedAction.Status.Phase).To(Equal(RemediationPhaseVerifying))

			// Step 3: Verifying → RolledBack.
			_, err = controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &updatedAction)).To(Succeed())
			Expect(updatedAction.Status.Phase).To(Equal(RemediationPhaseRolledBack))
			Expect(updatedAction.Status.VerificationResult).To(Equal("Degraded"))

			// Verify persona was rolled back to 256Mi.
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(persona), &updatedPersona)).To(Succeed())
			Expect(updatedPersona.Spec.Resources.Limits.Memory).To(Equal("256Mi"))

			// Verify incident has rollback resolution.
			var updatedIncident dorguv1.IncidentMemory
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(incident), &updatedIncident)).To(Succeed())
			Expect(updatedIncident.Spec.Resolution).NotTo(BeNil())
			Expect(updatedIncident.Spec.Resolution.Outcome).To(Equal("rollback"))
		})
	})

	Context("Terminal states", func() {
		It("should be no-op for Completed state", func() {
			persona := createTestPersona("rc-terminal-persona", "default")
			incident := createTestIncident("rc-terminal-incident", "default", persona.Name, "OOMKilled")
			action := createTestAction("rc-terminal-action", "default", persona.Name, incident.Name)

			action.Status = dorguv1.RemediationActionStatus{Phase: RemediationPhaseCompleted}
			Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())

			controller := newController(nil)
			result, err := controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("should be no-op for Failed state", func() {
			persona := createTestPersona("rc-failed-persona", "default")
			incident := createTestIncident("rc-failed-incident", "default", persona.Name, "OOMKilled")
			action := createTestAction("rc-failed-action", "default", persona.Name, incident.Name)

			action.Status = dorguv1.RemediationActionStatus{Phase: RemediationPhaseFailed}
			Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())

			controller := newController(nil)
			result, err := controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("should be no-op for RolledBack state", func() {
			persona := createTestPersona("rc-rb-persona", "default")
			incident := createTestIncident("rc-rb-incident", "default", persona.Name, "OOMKilled")
			action := createTestAction("rc-rb-action", "default", persona.Name, incident.Name)

			action.Status = dorguv1.RemediationActionStatus{Phase: RemediationPhaseRolledBack}
			Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())

			controller := newController(nil)
			result, err := controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})

	Context("IncidentMemory resolution", func() {
		It("should update IncidentMemory on successful completion", func() {
			persona := createTestPersona("rc-im-persona", "default")
			incident := createTestIncident("rc-im-incident", "default", persona.Name, "OOMKilled")
			action := createTestAction("rc-im-action", "default", persona.Name, incident.Name)

			action.Status = dorguv1.RemediationActionStatus{Phase: RemediationPhaseApproved}
			Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())

			controller := newController(nil)

			// Run through full lifecycle.
			_, err := controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})
			Expect(err).NotTo(HaveOccurred())

			time.Sleep(2 * time.Second)

			_, err = controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = controller.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(action),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify resolution info.
			Eventually(func(g Gomega) {
				var updatedIncident dorguv1.IncidentMemory
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(incident), &updatedIncident)).To(Succeed())
				g.Expect(updatedIncident.Spec.Resolution).NotTo(BeNil())
				g.Expect(updatedIncident.Spec.Resolution.Outcome).To(Equal("resolved"))
				g.Expect(updatedIncident.Spec.Resolution.Action).To(ContainSubstring("memory"))
				g.Expect(updatedIncident.Spec.Resolution.RemediationRef).NotTo(BeNil())
				g.Expect(updatedIncident.Spec.Resolution.AppliedAt).NotTo(BeNil())
				g.Expect(updatedIncident.Spec.Resolution.Duration).NotTo(BeNil())
			}, timeout, interval).Should(Succeed())
		})
	})
})
