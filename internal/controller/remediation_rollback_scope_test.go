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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// CR5-02, clean-room run #5. The remediation failed verification, the operator
// rolled the ApplicationPersona back, wrote phase RolledBack, and stopped. The
// live Deployment kept the value the heal had put there. `dorgu remediation
// list --all` showed RolledBack and nothing else; `diff` still printed the old
// change and still said "Rollback: Automatic rollback if health degrades".
// There was no condition, no event and no log line about the half that did not
// happen.
//
// The operator has get/list/watch on Deployments and no write verbs, by design,
// so it cannot finish the rollback. This is the record that it did not.
var _ = Describe("RemediationController rollback scope (CR5-02)", func() {
	const namespace = "default"

	ctx := context.Background()
	testLogger := zap.New(zap.UseDevMode(true))

	createPersona := func(name string) *dorguv1.ApplicationPersona {
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dorguv1.ApplicationPersonaSpec{
				Name: name,
				Type: "api",
				Tier: "standard",
				Resources: &dorguv1.ResourceConstraints{
					Limits: &dorguv1.ResourceValues{Memory: "256Mi", CPU: "250m"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, persona)).To(Succeed())
		return persona
	}

	createIncident := func(name, personaName string) *dorguv1.IncidentMemory {
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
					LabelSignal:           reasonOOMKilled,
					LabelPhase:            PhaseDetected,
				},
			},
			Spec: dorguv1.IncidentMemorySpec{
				PersonaRef: dorguv1.PersonaReference{
					Kind: "ApplicationPersona", Name: personaName, Namespace: namespace,
				},
				Category: "resource",
				Severity: "critical",
				Detection: dorguv1.DetectionInfo{
					Signal:    reasonOOMKilled,
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
		incident.Status = dorguv1.IncidentMemoryStatus{Phase: PhaseDetected, OccurrenceCount: 1}
		Expect(k8sClient.Status().Update(ctx, incident)).To(Succeed())
		return incident
	}

	// healedDeployment is the workload as it stands after the CLI applied the
	// remediation with the user's own credentials: 512Mi live, where Dorgu had
	// recorded 256Mi before the remediation ran.
	healedDeployment := func(name, liveMemory string) *appsv1.Deployment {
		replicas := int32(1)
		labels := map[string]string{"app.kubernetes.io/name": name}
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  name,
							Image: "ghcr.io/stefanprodan/podinfo:6.14.1",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse(liveMemory),
								},
							},
						}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deploy)).To(Succeed())
		return deploy
	}

	groundedAction := func(name, personaName, incidentName, deployName string) *dorguv1.RemediationAction {
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
				IncidentRef: dorguv1.IncidentReference{Name: incidentName, Namespace: namespace},
				PersonaRef: dorguv1.PersonaReference{
					Kind: "ApplicationPersona", Name: personaName, Namespace: namespace,
				},
				TrustLevel:  2,
				Confidence:  "0.85",
				Explanation: "Increase memory limit from 256Mi to 512Mi",
				WorkloadRef: &dorguv1.WorkloadRef{
					Kind:      "Deployment",
					Name:      deployName,
					Namespace: namespace,
					Container: deployName,
					ManagedBy: dorguv1.ManagedByUnmanaged,
					ObservedResources: &dorguv1.ObservedResources{
						Limits: &dorguv1.ResourceValues{Memory: "256Mi"},
					},
				},
				Action: dorguv1.RemediationActionDetail{
					Type:          "persona-update",
					Patch:         &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
					PrePatchState: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)},
				},
				Approval: &dorguv1.ApprovalSpec{Required: true},
				Rollback: &dorguv1.RemediationRollbackSpec{
					Enabled:          true,
					HealthCheckAfter: &metav1.Duration{Duration: 1 * time.Second},
					MaxRetries:       1,
				},
			},
		}
		Expect(k8sClient.Create(ctx, action)).To(Succeed())
		return action
	}

	// degradedController always reports the incident's signal as still present,
	// which is what drives verification to Degraded and the rollback.
	degradedController := func(personaName string) *RemediationController {
		signals := []detection.Signal{{
			Type:     detection.SignalOOMKilled,
			Severity: detection.SeverityCritical,
			Category: detection.CategoryResource,
			Source:   "pod-failure-detector",
			PersonaRef: &dorguv1.PersonaReference{
				Kind: "ApplicationPersona", Name: personaName, Namespace: namespace,
			},
			DetectedAt: time.Now(),
		}}
		collector := &remediationMockCollector{signals: signals}
		return &RemediationController{
			Client:   k8sClient,
			Executor: remediation.NewExecutor(k8sClient, testLogger),
			Verifier: remediation.NewVerifier(detection.NewEngine(testLogger, collector), k8sClient, testLogger),
			Rollback: remediation.NewRollback(k8sClient, testLogger),
			Logger:   testLogger.WithName("rollback-scope-test"),
		}
	}

	driveToRolledBack := func(controller *RemediationController, action *dorguv1.RemediationAction) *dorguv1.RemediationAction {
		action.Status = dorguv1.RemediationActionStatus{Phase: RemediationPhaseApproved}
		Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())

		key := client.ObjectKeyFromObject(action)
		for range 3 {
			_, err := controller.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			time.Sleep(1100 * time.Millisecond)
		}

		var out dorguv1.RemediationAction
		Expect(k8sClient.Get(ctx, key, &out)).To(Succeed())
		Expect(out.Status.Phase).To(Equal(RemediationPhaseRolledBack))
		return &out
	}

	conditionOf := func(action *dorguv1.RemediationAction, condType string) *metav1.Condition {
		for i := range action.Status.Conditions {
			if action.Status.Conditions[i].Type == condType {
				return &action.Status.Conditions[i]
			}
		}
		return nil
	}

	It("records the Deployment the rollback could not reach", func() {
		persona := createPersona("rs-diverged-persona")
		incident := createIncident("rs-diverged-incident", persona.Name)
		healedDeployment("rs-diverged-persona", "512Mi")
		action := groundedAction("rs-diverged-action", persona.Name, incident.Name, "rs-diverged-persona")

		out := driveToRolledBack(degradedController(persona.Name), action)

		// The half that always worked.
		var rolledBackPersona dorguv1.ApplicationPersona
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(persona), &rolledBackPersona)).To(Succeed())
		Expect(rolledBackPersona.Spec.Resources.Limits.Memory).To(Equal("256Mi"))

		// The half that was silent.
		cond := conditionOf(out, ConditionWorkloadDiverged)
		Expect(cond).NotTo(BeNil(), "a rollback that did not reach the workload must say so")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(remediation.ReasonWorkloadDiverged))

		Expect(cond.Message).To(ContainSubstring("default/rs-diverged-persona"))
		Expect(cond.Message).To(ContainSubstring("resources.limits.memory"))
		Expect(cond.Message).To(ContainSubstring("512Mi"))
		Expect(cond.Message).To(ContainSubstring("256Mi"))
		Expect(cond.Message).To(ContainSubstring("kubectl set resources deployment/rs-diverged-persona"))
	})

	It("stays quiet when the workload never took the change", func() {
		persona := createPersona("rs-clean-persona")
		incident := createIncident("rs-clean-incident", persona.Name)
		healedDeployment("rs-clean-persona", "256Mi")
		action := groundedAction("rs-clean-action", persona.Name, incident.Name, "rs-clean-persona")

		out := driveToRolledBack(degradedController(persona.Name), action)

		cond := conditionOf(out, ConditionWorkloadDiverged)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(remediation.ReasonWorkloadRestored))
	})

	It("will not claim a clean rollback it could not verify", func() {
		persona := createPersona("rs-unread-persona")
		incident := createIncident("rs-unread-incident", persona.Name)
		// No Deployment is created: the workload cannot be read.
		action := groundedAction("rs-unread-action", persona.Name, incident.Name, "rs-unread-missing")

		out := driveToRolledBack(degradedController(persona.Name), action)

		cond := conditionOf(out, ConditionWorkloadDiverged)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
		Expect(cond.Reason).To(Equal(remediation.ReasonWorkloadUnreadable))
	})
})
