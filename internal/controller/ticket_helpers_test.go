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

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// envtestGate is the one gate the envtest-backed controller specs use.
//
// Its clock runs a little ahead of the real one: the specs create a Deployment and
// approve against it within the same second, which the gate's clock-skew margin
// (rightly) reads as "written around the time of the ticket". A cluster's workload
// is never that fresh; the clock offset stands in for its age.
func envtestGate(log logr.Logger) *gate.Gate {
	return gate.New(k8sClient, log, gate.WithReader(k8sClient),
		gate.WithClock(func() time.Time { return time.Now().Add(2 * gate.ClockSkewMargin) }))
}

// ensurePersonaDeployment makes sure a live Deployment answers to the persona
// (named after it) in the default namespace with a 256Mi memory limit. An approval is bound to a live
// workload, so every spec that drives one to Applying needs it.
func ensurePersonaDeployment(ctx context.Context, personaName string) {
	const (
		namespace = "default"
		memory    = "256Mi"
	)
	var existing appsv1.Deployment
	key := client.ObjectKey{Name: personaName, Namespace: namespace}
	err := k8sClient.Get(ctx, key, &existing)
	if err == nil {
		return
	}
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "reading the persona's Deployment: %v", err)

	replicas := int32(1)
	labels := map[string]string{"app.kubernetes.io/name": personaName}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: personaName, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  personaName,
					Image: "nginx:1.27",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse(memory),
					}},
				}}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, deploy)).To(Succeed())
}

// approveProposed does what the proposer and the CLI do between them: the gate
// rules on the plan at proposal and its decision and the Proposed condition are
// written to status (phase Pending), then the plan is approved. The action's
// persona must already have a live Deployment (ensurePersonaDeployment).
func approveProposed(ctx context.Context, action *dorguv1.RemediationAction) {
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), action)).To(Succeed())

	decision, err := envtestGate(logr.Discard()).Run(ctx, gate.Candidate{
		Target: action.Spec.PersonaRef, Namespace: action.Namespace, Name: action.Name,
		Source: action.Spec.PlanSource, IncidentRef: action.Spec.IncidentRef,
		Steps: action.EffectiveSteps(), Final: true,
		Facts: gate.Facts{SuggestedAction: gate.ClassResourceAdjustment},
	}, gate.PhaseProposal)
	Expect(err).NotTo(HaveOccurred())
	Expect(decision.Allowed()).To(BeTrue(), decision.SkipReason())

	now := metav1.Now()
	action.Status = dorguv1.RemediationActionStatus{
		Phase:      RemediationPhaseApproved,
		ApprovedBy: "test-user",
		ApprovedAt: &now,
		Gate:       decision.Status(),
		Conditions: []metav1.Condition{{
			Type: dorguv1.ConditionProposed, Status: metav1.ConditionTrue, Reason: dorguv1.ReasonGateCleared,
			ObservedGeneration: action.Generation, LastTransitionTime: now,
		}},
	}
	Expect(k8sClient.Status().Update(ctx, action)).To(Succeed())
}

// reqFor is the reconcile request for an object.
func reqFor(obj client.Object) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
}

// simulateCLIPatch does what `dorgu remediation approve` does to the cluster after
// the operator's ticket: the user's credentials change the Deployment's memory
// limit (which moves its generation), and the CLI records WorkloadPatched on the
// action. The persona's Deployment must exist (ensurePersonaDeployment).
func simulateCLIPatch(ctx context.Context, action *dorguv1.RemediationAction, memory string) {
	var deploy appsv1.Deployment
	key := client.ObjectKey{Name: action.Spec.PersonaRef.Name, Namespace: action.Namespace}
	Expect(k8sClient.Get(ctx, key, &deploy)).To(Succeed())
	deploy.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse(memory)
	Expect(k8sClient.Update(ctx, &deploy, client.FieldOwner(gate.DorguFieldManager))).To(Succeed()) // the CLI patches as "dorgu"

	Eventually(func(g Gomega) {
		var fresh dorguv1.RemediationAction
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(action), &fresh)).To(Succeed())
		fresh.Status.Conditions = append(fresh.Status.Conditions, metav1.Condition{
			Type: "WorkloadPatched", Status: metav1.ConditionTrue, Reason: "CLIAppliedResourceChange",
			Message: "dorgu CLI applied resources.limits.memory=" + memory, LastTransitionTime: metav1.Now(),
		})
		g.Expect(k8sClient.Status().Update(ctx, &fresh)).To(Succeed())
	}).Should(Succeed())
}
