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

// F-03: Dorgu proposed a notification-type remediation, printed its own approve
// command, and approving it marked the action Failed and cooled the app down for
// 30 minutes. Approving a plan with nothing to apply must be an acknowledgement.
var _ = Describe("RemediationController advisory plans", func() {
	var testLogger = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))

	newAdvisoryController := func() *RemediationController {
		engine := detection.NewEngine(testLogger, &remediationMockCollector{})
		return &RemediationController{
			Client:   k8sClient,
			Executor: remediation.NewExecutor(k8sClient, testLogger),
			Verifier: remediation.NewVerifier(engine, k8sClient, testLogger),
			Rollback: remediation.NewRollback(k8sClient, testLogger),
			Logger:   testLogger.WithName("advisory-controller-test"),
		}
	}

	newPersona := func(name string) *dorguv1.ApplicationPersona {
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: dorguv1.ApplicationPersonaSpec{
				Name: name,
				Type: "api",
				Resources: &dorguv1.ResourceConstraints{
					Limits:   &dorguv1.ResourceValues{CPU: "500m", Memory: "256Mi"},
					Requests: &dorguv1.ResourceValues{CPU: "100m", Memory: "128Mi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, persona)).To(Succeed())
		return persona
	}

	newIncident := func(name, personaName string) *dorguv1.IncidentMemory {
		incident := &dorguv1.IncidentMemory{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: dorguv1.IncidentMemorySpec{
				PersonaRef: dorguv1.PersonaReference{
					Kind:      "ApplicationPersona",
					Name:      personaName,
					Namespace: "default",
				},
				Category: "health",
				Severity: "critical",
				Detection: dorguv1.DetectionInfo{
					Signal:    "ImagePullBackOff",
					Source:    "pod-collector",
					FirstSeen: metav1.Now(),
					LastSeen:  metav1.Now(),
					AffectedResources: []dorguv1.ResourceReference{
						{Kind: "Pod", Name: personaName + "-abc123", Namespace: "default"},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, incident)).To(Succeed())
		return incident
	}

	newApprovedAction := func(name, personaName, incidentName string, detail dorguv1.RemediationActionDetail) *dorguv1.RemediationAction {
		action := &dorguv1.RemediationAction{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: dorguv1.RemediationActionSpec{
				IncidentRef: dorguv1.IncidentReference{Name: incidentName, Namespace: "default"},
				PersonaRef: dorguv1.PersonaReference{
					Kind:      "ApplicationPersona",
					Name:      personaName,
					Namespace: "default",
				},
				TrustLevel:  2,
				Confidence:  "0.91",
				Explanation: "The image tag nginx:1.27-alpineX does not exist; nginx:1.27-alpine does.",
				Action:      detail,
				Approval:    &dorguv1.ApprovalSpec{Required: true},
			},
		}
		Expect(k8sClient.Create(ctx, action)).To(Succeed())
		action.Status = dorguv1.RemediationActionStatus{Phase: RemediationPhaseApproved}
		Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())
		return action
	}

	It("acknowledges an approved notification plan instead of failing it", func() {
		persona := newPersona("rc-adv-persona")
		incident := newIncident("rc-adv-incident", persona.Name)
		action := newApprovedAction("rc-adv-action", persona.Name, incident.Name,
			dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification})
		defer func() {
			_ = k8sClient.Delete(ctx, action)
			_ = k8sClient.Delete(ctx, incident)
			_ = k8sClient.Delete(ctx, persona)
		}()

		result, err := newAdvisoryController().Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(action),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		var updated dorguv1.RemediationAction
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(RemediationPhaseAcknowledged))
		Expect(updated.Status.AppliedAt).To(BeNil())

		applied := findCondition(updated.Status.Conditions, ConditionApplied)
		Expect(applied).NotTo(BeNil())
		Expect(applied.Reason).To(Equal(dorguv1.ReasonAdvisoryOnly))

		// The incident records the acknowledgement, not a resolution.
		var updatedIncident dorguv1.IncidentMemory
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(incident), &updatedIncident)).To(Succeed())
		Expect(updatedIncident.Spec.Resolution).NotTo(BeNil())
		Expect(updatedIncident.Spec.Resolution.Outcome).To(Equal("acknowledged"))
		Expect(updatedIncident.Status.Phase).NotTo(Equal(PhaseResolved))
	})

	It("acknowledges an approved plan whose only steps are advisory", func() {
		persona := newPersona("rc-adv-steps-persona")
		incident := newIncident("rc-adv-steps-incident", persona.Name)
		action := newApprovedAction("rc-adv-steps-action", persona.Name, incident.Name,
			dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification})
		action.Spec.Steps = []dorguv1.RemediationStep{
			{
				Order:          1,
				ID:             "step-1",
				Type:           dorguv1.StepTypeManual,
				Description:    "Correct the image tag to nginx:1.27-alpine.",
				Risk:           "low",
				AutoExecutable: false,
			},
		}
		Expect(k8sClient.Update(ctx, action)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, action)
			_ = k8sClient.Delete(ctx, incident)
			_ = k8sClient.Delete(ctx, persona)
		}()

		_, err := newAdvisoryController().Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(action),
		})
		Expect(err).NotTo(HaveOccurred())

		var updated dorguv1.RemediationAction
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(RemediationPhaseAcknowledged))
	})

	It("marks a plan the executor refuses as rejected before apply, not as a failure to apply", func() {
		persona := newPersona("rc-precondition-persona")
		incident := newIncident("rc-precondition-incident", persona.Name)
		// A persona-update carrying a patch but no pre-patch snapshot: there is
		// something to apply, so it reaches the executor, which refuses it
		// because it could never be rolled back.
		action := newApprovedAction("rc-precondition-action", persona.Name, incident.Name,
			dorguv1.RemediationActionDetail{
				Type:  dorguv1.ActionTypePersonaUpdate,
				Patch: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
			})
		defer func() {
			_ = k8sClient.Delete(ctx, action)
			_ = k8sClient.Delete(ctx, incident)
			_ = k8sClient.Delete(ctx, persona)
		}()

		_, err := newAdvisoryController().Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(action),
		})
		Expect(err).NotTo(HaveOccurred())

		var updated dorguv1.RemediationAction
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(RemediationPhaseFailed))
		Expect(updated.Status.AppliedAt).To(BeNil())

		applied := findCondition(updated.Status.Conditions, ConditionApplied)
		Expect(applied).NotTo(BeNil())
		Expect(applied.Reason).To(Equal(dorguv1.ReasonPreconditionRejected))
		Expect(applied.Message).To(ContainSubstring("nothing was applied"))

		// The persona was left alone.
		var updatedPersona dorguv1.ApplicationPersona
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(persona), &updatedPersona)).To(Succeed())
		Expect(updatedPersona.Spec.Resources.Limits.Memory).To(Equal("256Mi"))
	})
})

// findCondition returns the named condition, or nil.
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
