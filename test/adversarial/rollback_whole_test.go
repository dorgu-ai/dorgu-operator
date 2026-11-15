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

package adversarial

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

// C6 fix round 2: "whole or refused, never partial" holds at every point the gate runs
// for a rollback (the request, the approval and the apply), not only at intake.

func twoFieldPatch(memory, cpu string) string {
	return fmt.Sprintf(`{"spec":{"resources":{"limits":{"memory":%q,"cpu":%q}}}}`, memory, cpu)
}

// twoFieldAppliedFixture is appliedFixture for an action that changed memory and CPU
// together: a real approved, ticketed action (baseline 256Mi and 250m, written 512Mi and
// 500m), the persona and the workload given the change, the action ended Completed.
func twoFieldAppliedFixture(t *testing.T, id string) string {
	t.Helper()
	ns := newNamespace(t, id)
	createPersona(t, ns)
	createIncident(t, ns)
	deploy := createDeployment(t, ns)

	setPersonaMemory(t, ns, "256Mi")
	p := getPersona(t, ns)
	p.Spec.Resources.Limits.CPU = "250m"
	if err := k8sClient.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	limits := deploy.Spec.Template.Spec.Containers[0].Resources.Limits
	limits[corev1.ResourceMemory] = resource.MustParse("256Mi")
	limits[corev1.ResourceCPU] = resource.MustParse("250m")
	if err := k8sClient.Update(context.Background(), deploy); err != nil {
		t.Fatal(err)
	}

	ra := newRA(ns, rollbackRA, withProposer(operatorUsername), func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action.Patch = rawJSON(twoFieldPatch("512Mi", "500m"))
		ra.Spec.Action.PrePatchState = rawJSON(twoFieldPatch("256Mi", "250m"))
	})
	ra = createRA(t, ra, "Pending")
	approveAs(t, ra, alicePerson)
	if _, err := reconcile(t, newRemediationController(), ra); err != nil {
		t.Fatalf("applying the fixture action: %v", err)
	}
	if ra.Status.ApplyTicket == nil || ra.Status.Gate == nil || ra.Status.Gate.PrePatchState == nil {
		t.Fatalf("the fixture action has no real ticket or baseline (phase %q)", ra.Status.Phase)
	}

	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatal(err)
	}
	limits = deploy.Spec.Template.Spec.Containers[0].Resources.Limits
	limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
	limits[corev1.ResourceCPU] = resource.MustParse("500m")
	if err := k8sClient.Update(context.Background(), deploy); err != nil {
		t.Fatalf("giving the workload the change: %v", err)
	}
	setPhase(t, ra, "Completed")
	return ns
}

// OP-28: a rollback of two fields is requested whole and cleared whole, and then a person
// raises the live memory before the approver says yes. At approval the gate's decrease cap
// would clamp memory (restoring 256Mi over 2Gi is an 88% decrease) and keep CPU, so the
// ticket would restore CPU and leave memory: half of what Dorgu changed, a state nobody
// asked for. It must be refused at approval: no ticket, nothing written, the persona as
// the change left it.
func TestOP28RollbackPartialAtApproval(t *testing.T) {
	ns := twoFieldAppliedFixture(t, "op-28")
	out := process(t, intakeProcessor(), rollbackProposalFor(ns, "rb-two-fields", alicePerson, rollbackRA))
	if out.Action == nil {
		t.Fatalf("the whole two-field rollback was not converted: %+v %s", out.Rejection, out.Message)
	}
	rb := out.Action

	setLiveMemory(t, ns, "2Gi")
	approveAs(t, rb, alicePerson)
	_, err := reconcile(t, newRemediationController(), rb)

	refused := false
	for _, c := range rb.Status.Conditions {
		if c.Type == dorguv1.ConditionGateRefused && c.Reason == dorguv1alpha1.ReasonRollbackWouldBePartial {
			refused = true
		}
	}
	untouched := personaMemory(t, ns) == "512Mi" && personaCPU(t, ns) == "500m" && liveMemory(t, ns) == "2Gi"
	Expect(t, "OP-28", err == nil && refused && rb.Status.Phase == "Failed" && rb.Status.ApplyTicket == nil && untouched,
		fmt.Sprintf("two-field rollback after a live edit: err %v, phase %q, ticket %v, refused as partial %v, "+
			"persona %s/%s, live %s", err, rb.Status.Phase, rb.Status.ApplyTicket != nil, refused,
			personaMemory(t, ns), personaCPU(t, ns), liveMemory(t, ns)))
}
