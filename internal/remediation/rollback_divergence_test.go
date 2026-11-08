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

package remediation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// driftedAction is clean-room run #5's rolled-back remediation, reproduced from
// the CRD the tester dumped.
//
//	before approve   persona limits.memory 512Mi   deployment limits.memory 8Mi
//	after approve    persona limits.memory  16Mi   deployment limits.memory 16Mi
//	after RolledBack persona limits.memory 512Mi   deployment limits.memory 16Mi
//
// The persona came back. The Deployment did not, and nothing said so.
func driftedAction(managedBy string) *dorguv1.RemediationAction {
	now := metav1.Now()
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: "ra-drifted", Namespace: "drift"},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind: kindApplicationPersona, Name: "drifted", Namespace: "drift",
			},
			// Everything below is editable by whoever can write the spec, so the
			// rollback report must not read it (F8). It points at a workload and a
			// field that are not the ones the ticket is about.
			WorkloadRef: &dorguv1.WorkloadRef{
				Kind: "Deployment", Name: "decoy", Namespace: "elsewhere", Container: "decoy",
				ManagedBy: managedBy,
				ObservedResources: &dorguv1.ObservedResources{
					Limits: &dorguv1.ResourceValues{CPU: "1", Memory: "1Gi"},
				},
			},
			Action: dorguv1.RemediationActionDetail{
				Type:          dorguv1.ActionTypePersonaUpdate,
				Patch:         mustJSON(`{"spec":{"resources":{"limits":{"cpu":"9"}}}}`),
				PrePatchState: mustJSON(`{"spec":{"resources":{"limits":{"cpu":"1"}}}}`),
			},
			Rollback: &dorguv1.RemediationRollbackSpec{Enabled: true},
		},
		// What the operator wrote: the ticket (the paths and the workload it was
		// bound to) and status.gate (the live values it measured before the heal).
		Status: dorguv1.RemediationActionStatus{
			ApplyTicket: &dorguv1.ApplyTicket{
				WorkloadUID: driftedUID, Container: "drifted",
				Patch:     mustJSON(`{"spec":{"resources":{"limits":{"memory":"16Mi"}}}}`),
				IssuedAt:  &now,
				ExpiresAt: metav1.NewTime(now.Add(10 * time.Minute)),
			},
			Gate: &dorguv1.GateStatus{
				Phase: "ApprovalRecheck", Result: "Pass",
				Observed: &dorguv1.GateObserved{
					WorkloadUID: driftedUID,
					LiveValues: &dorguv1.ObservedResources{
						Limits:   &dorguv1.ResourceValues{CPU: "500m", Memory: "8Mi"},
						Requests: &dorguv1.ResourceValues{CPU: "100m", Memory: "8Mi"},
					},
				},
				PrePatchState: mustJSON(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`),
			},
		},
	}
}

const driftedUID = "uid-drift"

// ownedBy marks a Deployment as owned the way the live detection reads it.
func helmOwned(d *appsv1.Deployment) {
	d.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	d.Annotations = map[string]string{"meta.helm.sh/release-name": "drifted"}
}

// driftedDeployment is the live workload after the heal landed on it.
func driftedDeployment(limitMemory string, mutate ...func(*appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "drifted", Namespace: "drift", UID: driftedUID},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "drifted"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "drifted"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "drifted",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resourceQty("500m"),
								corev1.ResourceMemory: resourceQty(limitMemory),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resourceQty("100m"),
								corev1.ResourceMemory: resourceQty("8Mi"),
							},
						},
					}},
				},
			},
		},
	}
	for _, m := range mutate {
		m(d)
	}
	return d
}

func rollbackFor(objs ...runtime.Object) *Rollback {
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).WithRuntimeObjects(objs...).Build()
	return NewRollback(c, testLogger())
}

// resourceQty parses a quantity for a test fixture.
func resourceQty(value string) resource.Quantity {
	return resource.MustParse(value)
}

// TestRollback_CR502_PersonaOnlyRollbackIsReported is clean-room run #5's
// CR5-02.
//
// The remediation failed verification, the operator rolled the persona back,
// wrote phase RolledBack with "Remediation rolled back due to degraded health",
// and stopped. The Deployment kept the 16Mi the heal had put there. No
// condition, no event, no log line, no CLI hint. "RolledBack" is a strong word
// and a user reads it as "we undid it"; they do not then go and diff the
// Deployment. Meanwhile the persona and the cluster now disagree in a NEW
// direction, and the next proposal re-anchors its blast radius on the
// still-changed live value.
func TestRollback_CR502_PersonaOnlyRollbackIsReported(t *testing.T) {
	r := rollbackFor(driftedDeployment("16Mi"))
	action := driftedAction(dorguv1.ManagedByUnmanaged)

	outcome := r.InspectRollback(context.Background(), action)

	require.True(t, outcome.Diverged(), "the Deployment still holds the healed value")
	require.Len(t, outcome.Divergences, 1)
	assert.Equal(t, "resources.limits.memory", outcome.Divergences[0].Field)
	assert.Equal(t, "16Mi", outcome.Divergences[0].Live)
	assert.Equal(t, "8Mi", outcome.Divergences[0].Intended)

	message := outcome.Message()
	assert.Contains(t, message, "drift/drifted", "the message names the workload")
	assert.Contains(t, message, `"drifted"`, "and the container")
	assert.Contains(t, message, "resources.limits.memory", "and the field")
	assert.Contains(t, message, "16Mi", "and what is live")
	assert.Contains(t, message, "8Mi", "and what it held before the remediation")
	assert.Contains(t, message, "kubectl set resources deployment/drifted",
		"and what to run, because this workload is unmanaged")
	assert.Equal(t, ReasonWorkloadDiverged, outcome.Reason())
}

// TestRollback_CR502_OwnedWorkloadGetsNoWriteCommand keeps the ownership
// discipline intact on the way out. Handing a Helm user a `kubectl set
// resources` is the F-02 defect, and a rollback advisory is not an exemption
// from it.
func TestRollback_CR502_OwnedWorkloadGetsNoWriteCommand(t *testing.T) {
	r := rollbackFor(driftedDeployment("16Mi", helmOwned))
	action := driftedAction(dorguv1.ManagedByHelm)

	outcome := r.InspectRollback(context.Background(), action)

	require.True(t, outcome.Diverged())
	message := outcome.Message()
	assert.NotContains(t, message, "kubectl set")
	assert.NotContains(t, message, "kubectl patch")
	assert.Contains(t, message, "Helm release")
	assert.Contains(t, message, "8Mi", "the value to put back is still named")
}

// TestRollback_CR502_NoDivergenceWhenTheWorkloadNeverChanged covers the case
// the condition must stay quiet for: the heal was applied to the persona and
// never reached the Deployment, so the rollback really did undo everything.
func TestRollback_CR502_NoDivergenceWhenTheWorkloadNeverChanged(t *testing.T) {
	r := rollbackFor(driftedDeployment("8Mi"))
	action := driftedAction(dorguv1.ManagedByUnmanaged)

	outcome := r.InspectRollback(context.Background(), action)

	assert.False(t, outcome.Diverged())
	assert.Equal(t, ReasonWorkloadRestored, outcome.Reason())
	assert.Contains(t, outcome.Message(), "matches")
}

// TestRollback_CR502_UnreadableWorkloadSaysSo refuses to report a clean
// rollback it could not verify. An unreadable workload is not a restored one.
func TestRollback_CR502_UnreadableWorkloadSaysSo(t *testing.T) {
	r := rollbackFor()
	action := driftedAction(dorguv1.ManagedByUnmanaged)

	outcome := r.InspectRollback(context.Background(), action)

	assert.False(t, outcome.Diverged())
	assert.Equal(t, ReasonWorkloadUnreadable, outcome.Reason())
	assert.Contains(t, outcome.Message(), "could not")
}

// TestRollback_CR502_MultipleFieldsAreAllNamed makes sure the message does not
// stop at the first field, which is the shape checkBlastRadius was wrong in
// once already.
func TestRollback_CR502_MultipleFieldsAreAllNamed(t *testing.T) {
	deploy := driftedDeployment("16Mi")
	deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resourceQty("16Mi")

	r := rollbackFor(deploy)
	action := driftedAction(dorguv1.ManagedByUnmanaged)
	action.Status.ApplyTicket.Patch = mustJSON(
		`{"spec":{"resources":{"limits":{"memory":"16Mi"},"requests":{"memory":"16Mi"}}}}`)

	outcome := r.InspectRollback(context.Background(), action)

	require.Len(t, outcome.Divergences, 2)
	assert.Equal(t, "resources.limits.memory", outcome.Divergences[0].Field)
	assert.Equal(t, "resources.requests.memory", outcome.Divergences[1].Field)
	assert.Contains(t, outcome.Message(), "--limits=memory=8Mi")
	assert.Contains(t, outcome.Message(), "--requests=memory=8Mi")
}

// F8: the report is built from the ticket and status.gate. A replaced Deployment
// (another UID) is not the one the ticket was bound to, so Dorgu will not say
// whether it carries the change.
func TestF8_ADeploymentWithAnotherUIDIsNotComparedToTheTicket(t *testing.T) {
	replaced := driftedDeployment("16Mi", func(d *appsv1.Deployment) { d.UID = "uid-someone-else" })
	r := rollbackFor(replaced)

	outcome := r.InspectRollback(context.Background(), driftedAction(dorguv1.ManagedByUnmanaged))

	assert.False(t, outcome.Diverged())
	assert.Equal(t, ReasonWorkloadUnreadable, outcome.Reason())
	assert.Contains(t, outcome.Message(), "not the Deployment the ticket")
}

// F8: with no ticket there is nothing to say Dorgu wrote, so nothing diverged.
func TestF8_NoTicketMeansNothingToCompare(t *testing.T) {
	r := rollbackFor(driftedDeployment("16Mi"))
	action := driftedAction(dorguv1.ManagedByUnmanaged)
	action.Status.ApplyTicket = nil

	outcome := r.InspectRollback(context.Background(), action)

	assert.False(t, outcome.Diverged())
	assert.Equal(t, ReasonWorkloadUnreadable, outcome.Reason())
}
