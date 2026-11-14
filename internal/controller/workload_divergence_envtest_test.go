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
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// Re-review N2, on a real API server: a field deletion moves metadata.generation
// but leaves no fresh managedFields stamp, so only the generation bound sees it.
var _ = Describe("a field deleted under a ticket", func() {
	const personaName = "n2-field-deletion"

	It("reads as diverged although managedFields shows no write after the ticket", func() {
		replicas := int32(1)
		labels := map[string]string{"app.kubernetes.io/name": personaName}
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: personaName, Namespace: "default", Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name:  personaName,
						Image: "nginx:1.27",
						Env:   []corev1.EnvVar{{Name: "FEATURE_FLAG", Value: "on"}},
						Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						}},
					}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deploy, client.FieldOwner("kubectl-client-side-apply"))).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, deploy))).To(Succeed()) })

		// The ticket is written against the Deployment as it is now. Its clock is
		// ahead of the apiserver's by more than the skew margin, as the real
		// workload is older than the ticket.
		issued := metav1.NewTime(time.Now().Add(2 * gate.ClockSkewMargin))
		action := &dorguv1.RemediationAction{
			ObjectMeta: metav1.ObjectMeta{Name: "n2-ra", Namespace: "default"},
			Spec: dorguv1.RemediationActionSpec{
				PersonaRef: dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: personaName, Namespace: "default"},
			},
			Status: dorguv1.RemediationActionStatus{ApplyTicket: &dorguv1.ApplyTicket{
				WorkloadUID: string(deploy.UID), Container: personaName,
				ObservedResourceVersion: deploy.ResourceVersion, ObservedGeneration: deploy.Generation,
				IssuedAt: &issued, ExpiresAt: metav1.NewTime(issued.Add(gate.DefaultTicketTTL)),
			}},
		}
		g := envtestGate(logr.Discard())

		verdict, err := g.WorkloadDivergence(ctx, action)
		Expect(err).NotTo(HaveOccurred())
		Expect(verdict).ToNot(HaveOccurred(), "control: nothing has touched the Deployment since the ticket")

		// A JSON-patch remove of the container's env, by a manager that is not Dorgu's.
		remove := client.RawPatch(types.JSONPatchType, []byte(`[{"op":"remove","path":"/spec/template/spec/containers/0/env"}]`))
		Expect(k8sClient.Patch(ctx, deploy, remove, client.FieldOwner("attacker"))).To(Succeed())
		Expect(deploy.Generation).To(BeNumerically(">", action.Status.ApplyTicket.ObservedGeneration),
			"the deletion moved the spec generation")

		// The only trace in managedFields is the older creator's entry, which the
		// deletion did not re-stamp: nothing in it is newer than the ticket.
		var live appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &live)).To(Succeed())
		for _, mf := range live.ManagedFields {
			Expect(mf.Manager).NotTo(Equal("attacker"), "the apiserver leaves no entry for a pure deletion")
			Expect(mf.Time.Time.Before(issued.Add(-gate.ClockSkewMargin))).To(BeTrue(),
				"no stamp is newer than the ticket, so the stamps alone read as clean")
		}

		verdict, err = g.WorkloadDivergence(ctx, action)
		Expect(err).NotTo(HaveOccurred())
		Expect(verdict).To(HaveOccurred(), "the generation bound must catch a deletion that leaves no stamp")
		Expect(verdict.Guardrail).To(Equal(gate.GuardrailLiveWorkload))
		Expect(verdict.Reason).To(ContainSubstring("generation"))
	})
})
