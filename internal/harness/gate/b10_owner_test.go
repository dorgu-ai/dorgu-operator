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

package gate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// B10 (b): a workload a deployer owns gets advice, never an applied change.

func TestDeployerOwned(t *testing.T) {
	for managedBy, want := range map[string]bool{
		dorguv1.ManagedByHelm: true, dorguv1.ManagedByArgoCD: true, dorguv1.ManagedByFlux: true, dorguv1.ManagedByKustomize: true,
		dorguv1.ManagedByUnmanaged: false, dorguv1.ManagedByUnknown: false, "": false,
	} {
		assert.Equal(t, want, DeployerOwned(&dorguv1.WorkloadRef{ManagedBy: managedBy}), managedBy)
	}
	assert.False(t, DeployerOwned(nil))
}

func TestOwnerApproval_AllowsAnAdvisoryDecisionWithNoTicketAndStopsEarly(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi", helmOwned)))

	d := approvalRun(t, g, proposedAction())

	require.True(t, d.Allowed(), d.SkipReason())
	assert.True(t, d.AdvisoryByOwnership())
	_, err := d.Ticket()
	assert.Error(t, err)
	assert.Equal(t, "pass", verdictKind(d, GuardrailOwnerAdvisory))
	assert.Equal(t, "absent", verdictKind(d, GuardrailBlastRadius), "the later stages measure a change that will not be made, so they did not run")
	assert.Equal(t, "absent", verdictKind(d, GuardrailSelfHealingMode))
	for _, step := range d.Steps {
		assert.False(t, step.AutoExecutable)
		assert.Nil(t, step.Patch)
		assert.Nil(t, step.PrePatchState)
	}
	assert.Equal(t, dorguv1.ActionTypeNotification, d.Action.Type)
}

func TestOwnerApproval_LegacyActionOnlyPlanIsNeutralisedToo(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi", helmOwned)))
	ra := proposedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Steps = nil
		ra.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch("384Mi")}
	})

	d := approvalRun(t, g, ra)

	require.True(t, d.Allowed(), d.SkipReason())
	assert.True(t, d.AdvisoryByOwnership())
}

func TestOwnerApproval_AnAdvisoryPlanOnAnOwnedWorkloadIsLeftAlone(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi", helmOwned)))
	ra := proposedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Steps = []dorguv1.RemediationStep{{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "read the logs"}}
	})

	d := approvalRun(t, g, ra)

	require.True(t, d.Allowed(), d.SkipReason())
	assert.False(t, d.AdvisoryByOwnership(), "nothing was taken out of an advisory plan")
}

func TestOwnerApproval_UnmanagedStillGetsATicket(t *testing.T) {
	g := standardGate(t)

	d := approvalRun(t, g, proposedAction())

	require.True(t, d.Allowed(), d.SkipReason())
	assert.False(t, d.AdvisoryByOwnership())
	_, err := d.Ticket()
	assert.NoError(t, err)
}

func TestOwnerProposal_AnAutoStepOnAnOwnedWorkloadIsRefused(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi", helmOwned)))
	cand := planCandidate(personaUpdateStep(memPatch("384Mi")))
	cand.Final = true

	d := run(t, g, cand, PhaseProposal)

	assert.Equal(t, ResultRefuse, d.Result)
	stage, guardrail := d.RefusedBy()
	assert.Equal(t, StageOwnership, stage)
	assert.Equal(t, GuardrailOwnerAutoStep, guardrail)
}

// Not final: the proposer is still shaping the plan, and the first run measures it as written.
func TestOwnerProposal_ANonFinalPlanIsMeasuredNotRefused(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi", helmOwned)))

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	assert.True(t, d.Allowed(), d.SkipReason())
	assert.Equal(t, "pass", verdictKind(d, GuardrailBlastRadius))
}

// An advisory final plan for an owned workload is the considered outcome, not a plan that applies nothing by mistake.
func TestOwnerProposal_AnAdvisoryPlanForAnOwnedWorkloadPassesCompleteness(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi", helmOwned)))
	cand := planCandidate(dorguv1.RemediationStep{Order: 1, ID: "step-1", Type: dorguv1.StepTypeConfigChange, Description: "set it in the values"})
	cand.Final = true
	cand.Facts.TargetPath = memLimit

	d := run(t, g, cand, PhaseProposal)

	assert.True(t, d.Allowed(), d.SkipReason())
	assert.Equal(t, "pass", verdictKind(d, GuardrailNothingApplies))

	// The same plan for an unmanaged workload is refused: nothing in it applies the diagnosed change.
	g = newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	d = run(t, g, cand, PhaseProposal)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailNothingApplies))
}

func TestStepsWithRefusedCommand(t *testing.T) {
	steps := []dorguv1.RemediationStep{
		{Order: 1, Command: "kubectl get pods"},
		{Order: 2, Command: "kubectl get pods -o jsonpath='{.items}'"},
		{Order: 3},
		{Order: 4, Command: "helm upgrade x"},
	}
	assert.Equal(t, []int{1, 3}, StepsWithRefusedCommand(steps))
	assert.Empty(t, StepsWithRefusedCommand(steps[:1]))
}
