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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

// C6 review round: OP-25 to OP-27 name three defences the first five scenarios did not
// reach (the live-drift refusal, the rollback-of-rollback check, "never partial"), and
// the honest-path tests below keep the defences from also stopping the rollbacks they
// protect: right after the operator's own auto-rollback, and under observe mode.

func liveMemory(t *testing.T, ns string) string {
	t.Helper()
	var d appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: appName}, &d); err != nil {
		t.Fatal(err)
	}
	q := d.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	return q.String()
}

func setLiveMemory(t *testing.T, ns, memory string) {
	t.Helper()
	var d appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: appName}, &d); err != nil {
		t.Fatal(err)
	}
	d.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse(memory)
	if err := k8sClient.Update(context.Background(), &d); err != nil {
		t.Fatalf("setting the live memory to %s: %v", memory, err)
	}
}

func setPersonaMemory(t *testing.T, ns, memory string) {
	t.Helper()
	p := getPersona(t, ns)
	p.Spec.Resources.Limits.Memory = memory
	if err := k8sClient.Update(context.Background(), p); err != nil {
		t.Fatalf("setting the persona memory to %s: %v", memory, err)
	}
}

// OP-25: a person edits the workload after the change and before the rollback. A
// restore of the recorded value would overwrite their edit, so it is refused, whichever
// side they edited: the live Deployment (raised further, 768Mi) or the persona (1Gi).
// Nothing is made, and neither edit is touched.
func TestOP25RollbackOverAPersonsEdit(t *testing.T) {
	p := intakeProcessor()

	liveNS := appliedFixture(t, "op-25-live")
	setLiveMemory(t, liveNS, "768Mi")
	live := process(t, p, rollbackProposalFor(liveNS, "rb-live-edit", alicePerson, rollbackRA))

	personaNS := appliedFixture(t, "op-25-persona")
	setPersonaMemory(t, personaNS, "1Gi")
	edited := process(t, p, rollbackProposalFor(personaNS, "rb-persona-edit", alicePerson, rollbackRA))

	liveRefused := live.Rejection != nil && live.Rejection.Reason == dorguv1alpha1.ReasonRollbackWorkloadDrifted
	personaRefused := edited.Rejection != nil && edited.Rejection.Reason == dorguv1alpha1.ReasonRollbackWorkloadDrifted
	untouched := liveMemory(t, liveNS) == "768Mi" && personaMemory(t, personaNS) == "1Gi" &&
		len(rollbackActions(t, liveNS)) == 0 && len(rollbackActions(t, personaNS)) == 0
	Expect(t, "OP-25", liveRefused && personaRefused && untouched, fmt.Sprintf(
		"rollback over a live edit: %v; over a persona edit: %v; edits untouched: %v",
		live.Rejection, edited.Rejection, untouched))
}

// OP-26: a rollback of a rollback. The first rollback completed and put the workload
// back; asking to roll that back would re-apply the change it undid, with a restore
// derived from a record that was never a change Dorgu proposed. Re-applying is a new
// proposal a person makes, so this is refused, and no second rollback action is made.
func TestOP26RollbackOfARollback(t *testing.T) {
	ns := appliedFixture(t, "op-26")
	p := intakeProcessor()
	first := process(t, p, rollbackProposalFor(ns, "rb-first", alicePerson, rollbackRA))
	if first.Action == nil {
		t.Fatalf("the first rollback was not converted: %s", first.Message)
	}
	rb := first.Action
	approveAs(t, rb, alicePerson)
	if _, err := reconcile(t, newRemediationController(), rb); err != nil {
		t.Fatalf("approving the first rollback: %v", err)
	}
	setLiveMemory(t, ns, baselineMemory)
	setPhase(t, rb, "Completed")

	second := process(t, p, rollbackProposalFor(ns, "rb-of-rb", alicePerson, rb.Name))

	Expect(t, "OP-26", second.Rejection != nil && second.Rejection.Reason == dorguv1alpha1.ReasonRollbackOfRollback &&
		len(rollbackActions(t, ns)) == 1, fmt.Sprintf(
		"rollback of rollback %s: rejection %v; rollback actions %d", rb.Name, second.Rejection, len(rollbackActions(t, ns))))
}

// OP-27: a rollback that could only be partial. The recorded change is out of contract
// (the ticket wrote 768Mi over a 256Mi baseline, 3x where the gate allows 2x), so the
// restore is a 67% decrease the cap clamps. It must be refused, not made as a Pending
// action that applies nothing (or half of it): a rollback is whole or it is not made,
// and the cap is not relaxed to make it fit.
func TestOP27RollbackThatCouldOnlyBePartial(t *testing.T) {
	ns := appliedFixture(t, "op-27")
	var ra dorguv1.RemediationAction
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: rollbackRA}, &ra); err != nil {
		t.Fatal(err)
	}
	ra.Status.ApplyTicket.Patch = memoryPatch("768Mi")
	if err := k8sClient.Status().Update(context.Background(), &ra); err != nil {
		t.Fatalf("recording the out-of-contract ticket: %v", err)
	}
	setLiveMemory(t, ns, "768Mi")
	setPersonaMemory(t, ns, "768Mi")

	out := process(t, intakeProcessor(), rollbackProposalFor(ns, "rb-partial", alicePerson, rollbackRA))

	Expect(t, "OP-27", out.Rejection != nil && out.Rejection.Reason == dorguv1alpha1.ReasonRollbackWouldBePartial &&
		len(rollbackActions(t, ns)) == 0 && liveMemory(t, ns) == "768Mi", fmt.Sprintf(
		"out-of-contract rollback: rejection %v, action %v, rollback actions %d",
		out.Rejection, out.Action != nil, len(rollbackActions(t, ns))))
}

// Not scenarios: the honest paths the defences must leave open.

// The advisory sends a person to `dorgu remediation rollback` minutes after the operator
// rolled the action back. The action being undone is not a failed-cooldown sibling of
// its own rollback, at proposal or at approval.
func TestRollbackRightAfterTheAutoRollbackFlowsThrough(t *testing.T) {
	ns := appliedFixture(t, "rb-after-auto")
	var ra dorguv1.RemediationAction
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: rollbackRA}, &ra); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	ra.Status.AppliedAt = &now
	ra.Status.Phase = "RolledBack"
	if err := k8sClient.Status().Update(context.Background(), &ra); err != nil {
		t.Fatalf("ending the action RolledBack: %v", err)
	}
	setPersonaMemory(t, ns, baselineMemory) // what the auto-rollback restores; the Deployment keeps 512Mi

	out := process(t, intakeProcessor(), rollbackProposalFor(ns, "rb", alicePerson, rollbackRA))
	if out.Action == nil {
		t.Fatalf("a rollback minutes after an auto-rollback was refused: %+v %s", out.Rejection, out.Message)
	}
	rb := out.Action
	approveAs(t, rb, alicePerson)
	_, err := reconcile(t, newRemediationController(), rb)

	if err != nil || rb.Status.ApplyTicket == nil {
		t.Fatalf("the approval re-check refused the rollback: phase %q err %v conditions %+v",
			rb.Status.Phase, err, rb.Status.Conditions)
	}
}

// Observe mode withholds what Dorgu volunteers, not a person's undo of Dorgu's own change.
func TestRollbackInObserveModeFlowsThrough(t *testing.T) {
	ns := appliedFixture(t, "rb-observe")
	cp := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "observe-" + ns},
		Spec: dorguv1.ClusterPersonaSpec{Name: "observe", Policies: &dorguv1.ClusterPolicies{
			SelfHealing: &dorguv1.SelfHealingPolicy{Mode: dorguv1.SelfHealingModeObserve},
		}},
	}
	mustCreate(t, cp)
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cp) })

	out := process(t, intakeProcessor(), rollbackProposalFor(ns, "rb", alicePerson, rollbackRA))
	if out.Action == nil {
		t.Fatalf("a person's rollback was withheld in observe mode: %+v %s", out.Rejection, out.Message)
	}
	rb := out.Action
	approveAs(t, rb, alicePerson)
	_, err := reconcile(t, newRemediationController(), rb)

	if err != nil || rb.Status.ApplyTicket == nil {
		t.Fatalf("the approval re-check withheld the rollback in observe mode: phase %q err %v conditions %+v",
			rb.Status.Phase, err, rb.Status.Conditions)
	}
}
