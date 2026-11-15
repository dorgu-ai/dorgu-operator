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

package intake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// C6 (P2-D10): a rollback proposal carries no steps the operator reads. The operator
// derives them from what it recorded before it applied the referenced action, makes
// the action as itself (proposer = the operator, source = rollback) and names the
// person who asked in requestedBy.

const (
	pendingPhase  = "Pending"
	appliedName   = "ra-applied"
	rollbackAlice = "alice@example.com"
)

func jsonRaw(raw string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(raw)} }

func memoryPatch(v string) *apiextensionsv1.JSON {
	return jsonRaw(`{"spec":{"resources":{"limits":{"memory":"` + v + `"}}}}`)
}

// appliedRA is a Completed action whose ticket raised the web persona's memory from
// 256Mi to 512Mi, as the operator recorded it.
func appliedRA(mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: appliedName, Namespace: testNS},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef:  dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "web", Namespace: testNS},
			IncidentRef: dorguv1.IncidentReference{Name: "inc", Namespace: testNS},
			Explanation: "raise memory", Confidence: "0.9",
			Provenance: &dorguv1.RemediationProvenance{
				Proposal: "orig", Proposer: dorguv1.ProposerIdentity{Username: testOperator}, Source: dorguv1.ProposalSourceDorguPlanner,
			},
		},
		Status: dorguv1.RemediationActionStatus{
			Phase: "Completed",
			Gate: &dorguv1.GateStatus{
				Phase: "ApprovalRecheck", Result: "Pass", PrePatchState: memoryPatch("256Mi"),
			},
			ApplyTicket: &dorguv1.ApplyTicket{WorkloadUID: "dep-uid", Container: "web", Patch: memoryPatch("512Mi")},
		},
	}
	for _, m := range mutate {
		m(ra)
	}
	return ra
}

func webPersona(memory string) *dorguv1.ApplicationPersona {
	p := persona("web")
	p.Spec.Resources.Limits.Memory = memory
	return p
}

func webDeployment(memory string) *appsv1.Deployment {
	labels := map[string]string{"app": "web"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: testNS, UID: "dep-uid", Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "web",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
					},
				}}},
			},
		},
	}
}

// rollbackProposal is what `dorgu remediation rollback` writes: no steps, source
// rollback, the person as proposer.
func rollbackProposal(name, user, key string) *v1alpha1.RemediationProposal {
	return &v1alpha1.RemediationProposal{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: v1alpha1.RemediationProposalSpec{
			Proposer:       dorguv1.ProposerIdentity{Username: user},
			Source:         dorguv1.ProposalSourceRollback,
			IdempotencyKey: key,
			Target:         v1alpha1.ProposalTarget{Kind: "ApplicationPersona", Namespace: testNS, Name: "web"},
			RollbackOf:     &v1alpha1.ActionRef{Name: appliedName},
			Rationale:      "the new limit made the node unschedulable",
		},
	}
}

func rollbackObjects(extra ...client.Object) []client.Object {
	return append([]client.Object{webPersona("512Mi"), webDeployment("512Mi"), appliedRA()}, extra...)
}

func (w *world) actions(t *testing.T) []dorguv1.RemediationAction {
	t.Helper()
	var list dorguv1.RemediationActionList
	require.NoError(t, w.c.List(context.Background(), &list))
	return list.Items
}

func requireRollbackRejected(t *testing.T, out *Outcome, reason string) {
	t.Helper()
	require.NotNil(t, out)
	require.Equal(t, v1alpha1.PhaseRejected, out.Phase, out.Message)
	require.NotNil(t, out.Rejection)
	assert.Equal(t, v1alpha1.StageIntake, out.Rejection.Stage)
	assert.Equal(t, reason, out.Rejection.Reason, out.Message)
	assert.NotEmpty(t, out.Message)
}

func TestRollback_BecomesAnActionTheOperatorProposedForAPerson(t *testing.T) {
	w := newWorld(t, Config{}, rollbackObjects()...)
	w.create(t, rollbackProposal("rb", rollbackAlice, "rb-1"))

	out := w.process(t, "rb", nil)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	ra := out.Action
	require.NotNil(t, ra.Spec.Provenance)
	assert.Equal(t, testOperator, ra.Spec.Provenance.Proposer.Username, "the operator wrote this plan, so it is the proposer")
	assert.Equal(t, dorguv1.ProposalSourceRollback, ra.Spec.Provenance.Source)
	assert.Equal(t, rollbackAlice, ra.Spec.Provenance.RequestedBy, "requestedBy is the authenticated person, as admission verified it")
	assert.Equal(t, "rb", ra.Spec.Provenance.Proposal)
	assert.Nil(t, ra.Spec.Provenance.Agent)
	assert.Equal(t, appliedName, ra.Annotations[dorguv1.AnnotationRollbackOf])
	assert.Equal(t, pendingPhase, ra.Status.Phase, "it waits for a person's approval like any other action")
	require.NotNil(t, ra.Spec.Approval)
	assert.True(t, ra.Spec.Approval.Required)
	require.Len(t, ra.Spec.Steps, 1)
	assert.True(t, ra.Spec.Steps[0].AutoExecutable)
	assert.Equal(t, "256Mi", gate.PatchLeafValues(ra.Spec.Steps[0].Patch.Raw)[gate.ResourcePathMemory],
		"the restore is the baseline the operator recorded")
	require.NotNil(t, ra.Status.Gate, "the gate ran on live state")
	assert.Equal(t, "Proposal", ra.Status.Gate.Phase)
	assert.Contains(t, ra.Spec.Explanation, "unschedulable", "the requester's reason is shown, as text")
	assert.Equal(t, 1, w.open.calls, "the rollback gets its own incident, so the original's resolution is untouched")
}

func TestRollback_ForgedStepsAreNeverRead(t *testing.T) {
	w := newWorld(t, Config{}, rollbackObjects()...)
	p := rollbackProposal("rb-forged", rollbackAlice, "k")
	// The CRD refuses steps on a rollback; this is a cluster whose CRD does not.
	p.Spec.Steps = []dorguv1.RemediationStep{{
		Order: 1, ID: "evil", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true, Description: "evil",
		Patch: memoryPatch("8Gi"),
	}}
	w.create(t, p)

	out := w.process(t, "rb-forged", nil)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	require.Len(t, out.Action.Spec.Steps, 1)
	step := out.Action.Spec.Steps[0]
	assert.NotEqual(t, "evil", step.ID)
	assert.Equal(t, "256Mi", gate.PatchLeafValues(step.Patch.Raw)[gate.ResourcePathMemory])
	assert.NotContains(t, string(step.Patch.Raw), "8Gi")
}

func TestRollback_AClaimedIncidentOrAgentIsNotRecorded(t *testing.T) {
	w := newWorld(t, Config{}, append(rollbackObjects(), incident("inc", "OOMKilled"))...)
	p := rollbackProposal("rb-claims", rollbackAlice, "k")
	p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "inc", Namespace: testNS}
	p.Spec.Agent = &dorguv1.AgentInfo{Name: "trusted-agent"}
	w.create(t, p)

	out := w.process(t, "rb-claims", nil)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Nil(t, out.Action.Spec.Provenance.Agent)
	assert.Equal(t, "opened", out.Action.Spec.IncidentRef.Name, "the rollback answers its own incident, not a claimed one")
}

func TestRollback_OnlyAPersonMayAsk(t *testing.T) {
	for name, user := range map[string]string{
		"a service account":    "system:serviceaccount:apps:agent",
		"the operator itself":  testOperator,
		"another system: user": "system:kube-controller-manager",
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, Config{}, rollbackObjects()...)
			w.create(t, rollbackProposal("rb", user, "k"))

			out := w.process(t, "rb", nil)

			requireRollbackRejected(t, out, v1alpha1.ReasonRollbackNotHuman)
			assert.Empty(t, w.actions(t)[1:], "no action was made")
			assert.Zero(t, w.open.calls, "a refused rollback opens no incident")
		})
	}
}

func TestRollback_RefusalsAtIntake(t *testing.T) {
	cases := map[string]struct {
		objects []client.Object
		mutate  func(*v1alpha1.RemediationProposal)
		reason  string
	}{
		"the action does not exist": {
			objects: []client.Object{webPersona("512Mi"), webDeployment("512Mi")},
			reason:  v1alpha1.ReasonRollbackTargetNotFound,
		},
		"the action is for another persona": {
			objects: rollbackObjects(),
			mutate:  func(p *v1alpha1.RemediationProposal) { p.Spec.Target.Name = "api" },
			reason:  v1alpha1.ReasonRollbackTargetMismatch,
		},
		"the action never applied": {
			objects: []client.Object{webPersona("512Mi"), webDeployment("512Mi"), appliedRA(func(ra *dorguv1.RemediationAction) {
				ra.Status.Phase, ra.Status.ApplyTicket = pendingPhase, nil
			})},
			reason: v1alpha1.ReasonRollbackNotApplied,
		},
		"the action is still verifying": {
			objects: []client.Object{webPersona("512Mi"), webDeployment("512Mi"), appliedRA(func(ra *dorguv1.RemediationAction) {
				ra.Status.Phase = "Verifying"
			})},
			reason: v1alpha1.ReasonRollbackNotApplied,
		},
		"the action is a rollback": {
			objects: []client.Object{webPersona("512Mi"), webDeployment("512Mi"), appliedRA(func(ra *dorguv1.RemediationAction) {
				ra.Spec.Provenance.Source = dorguv1.ProposalSourceRollback
			})},
			reason: v1alpha1.ReasonRollbackOfRollback,
		},
		"the operator recorded no baseline": {
			objects: []client.Object{webPersona("512Mi"), webDeployment("512Mi"), appliedRA(func(ra *dorguv1.RemediationAction) {
				ra.Status.Gate.PrePatchState = nil
				ra.Spec.Action = dorguv1.RemediationActionDetail{
					Type: dorguv1.ActionTypePersonaUpdate, Patch: memoryPatch("512Mi"), PrePatchState: memoryPatch("1Mi"),
				}
			})},
			reason: v1alpha1.ReasonRollbackNoRecordedState,
		},
		"a person raised memory further since": {
			objects: []client.Object{webPersona("512Mi"), webDeployment("768Mi"), appliedRA()},
			reason:  v1alpha1.ReasonRollbackWorkloadDrifted,
		},
		"the Deployment was replaced": {
			objects: []client.Object{webPersona("512Mi"), webDeployment("512Mi"), appliedRA(func(ra *dorguv1.RemediationAction) {
				ra.Status.ApplyTicket.WorkloadUID = "another-uid"
			})},
			reason: v1alpha1.ReasonRollbackWorkloadDrifted,
		},
		"the workload already holds the baseline": {
			objects: []client.Object{webPersona("256Mi"), webDeployment("256Mi"), appliedRA()},
			reason:  v1alpha1.ReasonRollbackNothingToRestore,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, Config{}, tc.objects...)
			p := rollbackProposal("rb", rollbackAlice, "k")
			if tc.mutate != nil {
				tc.mutate(p)
			}
			w.create(t, p)
			before := len(w.actions(t))

			out := w.process(t, "rb", nil)

			requireRollbackRejected(t, out, tc.reason)
			assert.Len(t, w.actions(t), before, "no action was made")
			assert.Zero(t, w.open.calls, "a refused rollback opens no incident")
			got := w.get(t, "rb")
			assert.Equal(t, v1alpha1.PhaseRejected, got.Status.Phase)
			assert.Equal(t, tc.reason, got.Status.Rejection.Reason)
		})
	}
}

func TestRollback_DoubleRollbackIsRefusedWhileOneIsOpen(t *testing.T) {
	w := newWorld(t, Config{}, rollbackObjects()...)
	w.create(t, rollbackProposal("rb-1", rollbackAlice, "k1"))
	first := w.process(t, "rb-1", nil)
	require.Equal(t, v1alpha1.PhaseConverted, first.Phase, first.Message)

	w.create(t, rollbackProposal("rb-2", "bob@example.com", "k2"))
	second := w.process(t, "rb-2", nil)

	requireRollbackRejected(t, second, v1alpha1.ReasonRollbackAlreadyOpen)
	assert.Len(t, w.actions(t), 2, "the original and the first rollback; no second rollback")
}

func TestRollback_AFinishedRollbackDoesNotBlockANewOne(t *testing.T) {
	for _, phase := range []string{"Rejected", "Expired", "Failed"} {
		t.Run(phase, func(t *testing.T) {
			earlier := appliedRA(func(ra *dorguv1.RemediationAction) {
				ra.Name = "ra-rollback-" + phase
				ra.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: appliedName}
				ra.Spec.Provenance.Source = dorguv1.ProposalSourceRollback
				ra.Status.Phase = phase
			})
			w := newWorld(t, Config{}, rollbackObjects(earlier)...)
			w.create(t, rollbackProposal("rb", rollbackAlice, "k"))

			out := w.process(t, "rb", nil)

			assert.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
		})
	}
}

func TestRollback_AnOpenRollbackOfAnotherActionDoesNotBlock(t *testing.T) {
	other := appliedRA(func(ra *dorguv1.RemediationAction) {
		ra.Name = "ra-rollback-other"
		ra.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: "some-other-action"}
		ra.Status.Phase = pendingPhase
	})
	w := newWorld(t, Config{}, rollbackObjects(other)...)
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))

	out := w.process(t, "rb", nil)

	// The other rollback's pending action is an active remediation for the persona, so the
	// gate may refuse on its own concurrency rule; the open-rollback rule is not the reason.
	if out.Phase == v1alpha1.PhaseRejected {
		assert.NotEqual(t, v1alpha1.ReasonRollbackAlreadyOpen, out.Rejection.Reason)
	}
}

func TestRollback_ARetryAfterAPartialFailureFindsItsOwnAction(t *testing.T) {
	w := newWorld(t, Config{}, rollbackObjects()...)
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))
	first := w.process(t, "rb", nil)
	require.Equal(t, v1alpha1.PhaseConverted, first.Phase, first.Message)

	// The operator died after making the action and before recording the proposal.
	prop := w.get(t, "rb")
	prop.Status = v1alpha1.RemediationProposalStatus{Phase: v1alpha1.PhaseReceived}
	require.NoError(t, w.c.Status().Update(context.Background(), prop))

	retry := w.process(t, "rb", nil)

	require.Equal(t, v1alpha1.PhaseConverted, retry.Phase, retry.Message)
	assert.Equal(t, first.Action.Name, retry.Action.Name, "its own action is not 'an open rollback'")
	assert.Len(t, w.actions(t), 2)
}

func TestRollback_TheGateStillJudgesTheDerivedPlan(t *testing.T) {
	// Another remediation is active on the persona: the gate's one-at-a-time rule refuses
	// the rollback like any other plan, and the proposal says which guardrail did.
	busy := appliedRA(func(ra *dorguv1.RemediationAction) {
		ra.Name = "ra-busy"
		ra.Status.Phase = "Applying"
		ra.Status.ApplyTicket = nil
	})
	w := newWorld(t, Config{}, rollbackObjects(busy)...)
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))

	out := w.process(t, "rb", nil)

	require.Equal(t, v1alpha1.PhaseRejected, out.Phase)
	assert.Equal(t, gate.StageGuardrails, out.Rejection.Stage)
	assert.Len(t, w.actions(t), 2, "no rollback action beside the two that existed")
}

func TestRollback_ASourceRollbackClaimWithoutRollbackOfIsAnAgentsProposal(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	p := proposal("claims-rollback", "alice", "k")
	p.Spec.Source = dorguv1.ProposalSourceRollback
	w.create(t, p)

	out := w.process(t, "claims-rollback", nil)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Equal(t, dorguv1.ProposalSourceAgent, out.Action.Spec.Provenance.Source)
	assert.Empty(t, out.Action.Spec.Provenance.RequestedBy)
}

func TestRollback_ReasonsMatchTheGatesAndTheCRDContract(t *testing.T) {
	assert.Equal(t, gate.RollbackReasonNotApplied, v1alpha1.ReasonRollbackNotApplied)
	assert.Equal(t, gate.RollbackReasonOfRollback, v1alpha1.ReasonRollbackOfRollback)
	assert.Equal(t, gate.RollbackReasonNoRecordedState, v1alpha1.ReasonRollbackNoRecordedState)
	assert.Equal(t, gate.RollbackReasonWorkloadDrifted, v1alpha1.ReasonRollbackWorkloadDrifted)
	assert.Equal(t, gate.RollbackReasonNothingToRestore, v1alpha1.ReasonRollbackNothingToRestore)
}

func TestRollback_ARollbackIsReplayedNotDecidedTwice(t *testing.T) {
	w := newWorld(t, Config{}, rollbackObjects()...)
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))
	require.Equal(t, v1alpha1.PhaseConverted, w.process(t, "rb", nil).Phase)

	again := w.process(t, "rb", nil)

	assert.True(t, again.Replayed)
	assert.Len(t, w.actions(t), 2)
}
