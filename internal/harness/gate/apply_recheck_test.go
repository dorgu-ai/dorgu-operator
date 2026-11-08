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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// B5 fix round, operator side: F1/F3 (apply re-runs the gate and the history must
// be on record), F5 (divergence), F6 (generation), F8 (rollback reads ticket and
// status.gate), V2 (action class).

// --- F1: a ticket is honoured only with approval history and a re-derived patch ---

func TestF1_AForgedTicketOnAnActionNobodyApprovedIsRefused(t *testing.T) {
	forged := ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApprovedAt = nil })

	problem := checkTicket(t, standardGate(t), forged)

	require.NotNil(t, problem, "a Ticketed action with no approvedAt was never approved")
	assert.Equal(t, GuardrailNotApproved, problem.Guardrail)
}

func TestF1_AForgedTicketWithoutTheProposedConditionIsRefused(t *testing.T) {
	forged := ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Conditions = nil })

	problem := checkTicket(t, standardGate(t), forged)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailNoProposalRecord, problem.Guardrail)
}

func TestF1_AForgedTicketWithoutStatusGateIsRefused(t *testing.T) {
	forged := ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Gate = nil })

	problem := checkTicket(t, standardGate(t), forged)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailNoProposalRecord, problem.Guardrail)
}

func TestF1_AStatusGateThatIsNotTheApprovalRecheckIsRefused(t *testing.T) {
	forged := ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Gate.Phase = string(PhaseProposal) })

	problem := checkTicket(t, standardGate(t), forged)

	require.NotNil(t, problem, "the apply-time record is the approval re-check, not the proposal")
	assert.Equal(t, GuardrailNoProposalRecord, problem.Guardrail)
}

func TestF1_ATicketPatchThatIsNotWhatTheGateDerivesIsRefused(t *testing.T) {
	// 999Gi passes the shape and allowlist checks, and nothing in the spec asks
	// for it: the plan is 384Mi.
	forged := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.Patch = memPatch("999Gi")
	})

	problem := checkTicket(t, standardGate(t), forged)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailTicketMismatch, problem.Guardrail)
}

func TestF1_ATicketContainerThatIsNotTheGatesIsRefused(t *testing.T) {
	forged := ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.Container = "sidecar" })

	problem := checkTicket(t, standardGate(t), forged)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailTicketMismatch, problem.Guardrail)
}

func TestF1_ApplyReRunsTheGateOnLiveState(t *testing.T) {
	// The ticket was legitimate when issued. Since then the live limit was cut to
	// 64Mi, so 384Mi is 6x live: the cap removes the only change and nothing is left.
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("64Mi")))

	problem := checkTicket(t, g, ticketedAction())

	require.NotNil(t, problem, "apply must re-measure; the ticket is not a standing permission")
	assert.Equal(t, GuardrailNothingLeft, problem.Guardrail)
}

func TestF1_ApplyRefusesATicketTheCapWouldNowNarrow(t *testing.T) {
	// The plan raises memory and cpu. Live memory is now 150Mi, so the cap (300Mi)
	// drops the 384Mi change; the gate would now issue a cpu-only ticket, and the
	// memory+cpu ticket on the action is not it.
	live := deployment("150Mi", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = mustQuantity("250m")
	})
	g := newTestGate(t, fakeClient(t, persona("256Mi"), live))
	cpu := personaUpdateStep(jsonOf(`{"spec":{"resources":{"limits":{"cpu":"300m"}}}}`))
	cpu.Order, cpu.ID = 2, "step-2"
	both := jsonOf(`{"spec":{"resources":{"limits":{"cpu":"300m","memory":"384Mi"}}}}`)
	ra := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Steps = []dorguv1.RemediationStep{personaUpdateStep(memPatch("384Mi")), cpu}
		ra.Status.ApplyTicket.Patch = both
	})

	problem := checkTicket(t, g, ra)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailTicketMismatch, problem.Guardrail)
}

func TestF3_ApplyRefusesWhenTheModeWasFlippedToObserve(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"),
		clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModeObserve })))

	problem := checkTicket(t, g, ticketedAction())

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailSelfHealingMode, problem.Guardrail)
}

func TestF3_ApplyRefusesAProtectedNamespaceAddedSinceApproval(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")), WithProtectedNamespaces(testNS))

	problem := checkTicket(t, g, ticketedAction())

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailDenyList, problem.Guardrail)
}

func TestF1_ATicketWithoutAnIssueTimeIsRefused(t *testing.T) {
	forged := ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.IssuedAt = nil })

	problem := checkTicket(t, standardGate(t), forged)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailTicket, problem.Guardrail)
}

// --- F6: the ticket carries the observed generation and the issue time ------------

func TestF6_TheTicketCarriesTheObservedGenerationAndTheIssueTime(t *testing.T) {
	live := deployment("256Mi", func(d *appsv1.Deployment) { d.Generation = 5 })
	g := newTestGate(t, fakeClient(t, persona("256Mi"), live))

	d := approvalRun(t, g, proposedAction())
	require.True(t, d.Allowed(), d.SkipReason())
	ticket, err := d.Ticket()
	require.NoError(t, err)

	assert.Equal(t, int64(5), ticket.ObservedGeneration)
	require.NotNil(t, ticket.IssuedAt)
	assert.True(t, ticket.IssuedAt.Time.Equal(fixedNow), "IssuedAt is the gate's own decision time")
}

// --- F5: divergence is judged on apiserver-sourced evidence, erring toward diverged ---

func divergedBy(t *testing.T, entries []metav1.ManagedFieldsEntry) *TicketProblem {
	t.Helper()
	changed := deployment("256Mi", func(d *appsv1.Deployment) { d.ResourceVersion = "99" })
	g := newTestGate(t, withManagedFields(t, entries, persona("256Mi"), changed))
	problem, err := g.WorkloadDivergence(context.Background(), ticketedAction())
	require.NoError(t, err)
	return problem
}

func TestF5_EmptyManagedFieldsOnAMovedWorkloadIsDiverged(t *testing.T) {
	assert.NotNil(t, divergedBy(t, nil), "a moved resourceVersion with no evidence of who wrote it is not clean")
}

func TestF5_AnEntryWithNoTimestampIsDiverged(t *testing.T) {
	entry := metav1.ManagedFieldsEntry{Manager: "kubectl-edit", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1"}
	assert.NotNil(t, divergedBy(t, []metav1.ManagedFieldsEntry{entry}))
}

func TestF5_AnOperatorClockAheadOfTheAPIServerStillCatchesTheWrite(t *testing.T) {
	// The operator stamped the ticket at 12:00:00 by its clock, which runs ahead of
	// the apiserver's: a write the apiserver stamps 3s earlier may still have
	// happened after the ticket.
	skewed := managed("kubectl-edit", fixedNow.Add(-3*time.Second), "")
	assert.NotNil(t, divergedBy(t, []metav1.ManagedFieldsEntry{skewed}))
}

func TestF5_TheDeploymentControllersRevisionStampIsNotSomeoneElse(t *testing.T) {
	rollout := stampedBy(kubeControllerManager, fieldsRevisionAnnotation)
	assert.Nil(t, divergedBy(t, []metav1.ManagedFieldsEntry{stampedBy(DorguFieldManager, fieldsTicketMemory), rollout}),
		"the Deployment controller stamps the revision annotation after the CLI's own patch")
	assert.NotNil(t, divergedBy(t, []metav1.ManagedFieldsEntry{managed("kube-controller-manager-x", fixedNow.Add(time.Second), "")}))
}

func TestF5_AWriteLongBeforeTheTicketIsNotDivergence(t *testing.T) {
	old := managed("kubectl-edit", fixedNow.Add(-time.Hour), "")
	assert.Nil(t, divergedBy(t, []metav1.ManagedFieldsEntry{old, stampedBy(DorguFieldManager, fieldsTicketMemory)}))
}

func TestF5_OnlyTheExactDorguManagerNameIsDorgus(t *testing.T) {
	for _, name := range []string{"dorgu-gitops", "dorgu-operator", "Dorgu", "DORGU", "dorgu ", "dorguX"} {
		entry := managed(name, fixedNow.Add(time.Second), "")
		assert.NotNil(t, divergedBy(t, []metav1.ManagedFieldsEntry{entry}), "manager %q is not Dorgu's CLI", name)
	}
}

func TestF5_AnUnchangedResourceVersionNeedsNoEvidence(t *testing.T) {
	g := newTestGate(t, withManagedFields(t, nil, persona("256Mi"), deployment("256Mi")))
	problem, err := g.WorkloadDivergence(context.Background(), ticketedAction())
	require.NoError(t, err)
	assert.Nil(t, problem)
}

// --- F8: rollback reads the ticket and status.gate, not the spec ------------------

func TestF8_RollbackFieldsComeFromTheTicketNotTheSpecAction(t *testing.T) {
	ra := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: jsonOf(`{"spec":{"resources":{"limits":{"cpu":"9"}}}}`)}
	})
	assert.Equal(t, []string{"spec.resources.limits.memory"}, RolledBackFields(ra),
		"the paths Dorgu wrote are the ticket's, whatever spec.action now says")
}

func TestF8_ARollbackWithoutATicketHasNoFields(t *testing.T) {
	assert.Empty(t, RolledBackFields(ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket = nil })))
}

// --- V2: the plan's action class must be the one the rules recommended -----------

func TestV2_APersonaUpdateUnderANonResourceDiagnosisIsRefused(t *testing.T) {
	g := standardGate(t)
	cand := planCandidate(personaUpdateStep(memPatch("384Mi")))
	cand.Final = true
	cand.Facts.SuggestedAction = "deployment-fix" // what the rules said for an image-pull failure

	d := run(t, g, cand, PhaseProposal)

	assert.Equal(t, ResultRefuse, d.Result)
	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailActionClass, guardrail)
}

func TestV2_APersonaUpdateWithNoRulesActionIsRefused(t *testing.T) {
	g := standardGate(t)
	cand := planCandidate(personaUpdateStep(memPatch("384Mi")))
	cand.Final = true
	cand.Facts.SuggestedAction = ""

	d := run(t, g, cand, PhaseProposal)

	assert.Equal(t, ResultRefuse, d.Result, "no recorded rules action means no class to match")
	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailActionClass, guardrail)
}

func TestV2_AResourceAdjustmentMayCarryAPersonaUpdate(t *testing.T) {
	g := standardGate(t)
	cand := planCandidate(personaUpdateStep(memPatch("384Mi")))
	cand.Final = true
	cand.Facts.SuggestedAction = "resource-adjustment"

	d := run(t, g, cand, PhaseProposal)

	assert.True(t, d.Allowed(), d.SkipReason())
}

func TestV2_AnAdvisoryPlanUnderANonResourceDiagnosisPasses(t *testing.T) {
	g := standardGate(t)
	manual := dorguv1.RemediationStep{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "check the registry credentials"}
	cand := planCandidate(manual)
	cand.Final = true
	cand.Facts.SuggestedAction = "deployment-fix"

	d := run(t, g, cand, PhaseProposal)

	assert.True(t, d.Allowed(), d.SkipReason())
}
