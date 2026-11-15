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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// C6 fix round 2: a rollback is whole or refused, never partial, wherever the gate runs
// for it. These cover the approval re-check and the apply-time re-run (intake has its own
// tests, over the same PartialRollback).

// appliedActionName is the action the rollback fixtures undo.
const appliedActionName = "ra-applied"

const twoFieldRestore = `{"spec":{"resources":{"limits":{"cpu":"250m","memory":"256Mi"}}}}`

func twoFieldStep() dorguv1.RemediationStep {
	return dorguv1.RemediationStep{
		Order: 1, ID: "rollback-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
		Patch: jsonOf(twoFieldRestore),
	}
}

func TestPartialRollback_IsEmptyOnlyForAPlainPassOfEveryStepUnchanged(t *testing.T) {
	want := []dorguv1.RemediationStep{twoFieldStep()}
	whole := func() *Decision {
		return &Decision{Result: ResultPass, Steps: []dorguv1.RemediationStep{{
			Order: 1, ID: "rollback-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
			// Reordered keys and spacing: the same patch.
			Patch: jsonOf(`{ "spec": {"resources": {"limits": {"memory":"256Mi", "cpu":"250m"}}} }`),
		}}}
	}
	require.Empty(t, PartialRollback(whole(), want, nil), "an identical plan is whole")

	cases := map[string]func(*Decision){
		"a clamp":           func(d *Decision) { d.Result = ResultClamp },
		"a refusal":         func(d *Decision) { d.Result = ResultRefuse },
		"an unknown result": func(d *Decision) { d.Result = "" },
		"a step dropped":    func(d *Decision) { d.Steps = nil },
		"a step added":      func(d *Decision) { d.Steps = append(d.Steps, d.Steps[0]) },
		"a field stripped":  func(d *Decision) { d.Steps[0].Patch = jsonOf(`{"spec":{"resources":{"limits":{"cpu":"250m"}}}}`) },
		"a value changed": func(d *Decision) {
			d.Steps[0].Patch = jsonOf(`{"spec":{"resources":{"limits":{"cpu":"250m","memory":"300Mi"}}}}`)
		},
		"a patch removed":          func(d *Decision) { d.Steps[0].Patch = nil },
		"made non-executable":      func(d *Decision) { d.Steps[0].AutoExecutable = false },
		"another step type":        func(d *Decision) { d.Steps[0].Type = dorguv1.StepTypeManual },
		"a patch that is not JSON": func(d *Decision) { d.Steps[0].Patch = jsonOf(`{`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dec := whole()
			mutate(dec)
			msg := PartialRollback(dec, want, nil)
			assert.NotEmpty(t, msg)
			assert.Contains(t, msg, "spec.resources.limits.cpu, spec.resources.limits.memory", "it names what the restore puts back, read from the plan")
			assert.Contains(t, msg, "nothing was applied")
		})
	}
}

func TestPartialRollback_NamesTheRestoredFieldsItIsGiven(t *testing.T) {
	dec := &Decision{Result: ResultClamp}
	assert.Contains(t, PartialRollback(dec, []dorguv1.RemediationStep{twoFieldStep()}, []string{"spec.resources.limits.memory"}), "restore of spec.resources.limits.memory as derived")
	assert.Contains(t, PartialRollback(dec, nil, nil), "the recorded values", "with nothing to name it still says what it refused")
}

func TestPartialRollback_QuotesWhatTheGateSaid(t *testing.T) {
	dec := &Decision{Result: ResultClamp}
	dec.clamp(StageGuardrails, GuardrailBlastRadius, "blast-radius clamp: 88% decrease (max 50%)", nil)

	msg := PartialRollback(dec, []dorguv1.RemediationStep{twoFieldStep()}, nil)

	assert.Contains(t, msg, "The gate said (blast-radius): blast-radius clamp: 88% decrease (max 50%)")
}

// twoFieldRollbackAction is an approved, operator-made rollback restoring memory and CPU.
func twoFieldRollbackAction(mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	base := func(ra *dorguv1.RemediationAction) {
		ra.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: appliedActionName}
		ra.Spec.Provenance = &dorguv1.RemediationProvenance{
			Proposer: dorguv1.ProposerIdentity{Username: "system:serviceaccount:dorgu-system:dorgu-operator"},
			Source:   dorguv1.ProposalSourceRollback, RequestedBy: requester,
		}
		ra.Spec.Steps = []dorguv1.RemediationStep{twoFieldStep()}
		ra.Status.ApprovedBy = requester
	}
	return proposedAction(append([]func(*dorguv1.RemediationAction){base}, mutate...)...)
}

// cpuAt500m gives the Deployment a 500m CPU limit: restoring (256Mi, 250m) over it is a
// decrease the 50% cap clamps for memory when the live memory is 2Gi, and leaves CPU alone.
func cpuAt500m(d *appsv1.Deployment) {
	d.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = mustQuantity("500m")
}

func TestWholeRollback_TheApprovalRecheckRefusesARestoreTheCapWouldNarrow(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("2Gi", cpuAt500m)))

	dec := approvalRun(t, g, twoFieldRollbackAction())

	require.False(t, dec.Allowed(), "a ticket for cpu alone would be half the rollback")
	stage, guardrail := dec.RefusedBy()
	assert.Equal(t, StageGuardrails, stage)
	assert.Equal(t, GuardrailRollbackWouldBePartial, guardrail)
	assert.Equal(t, "RollbackWouldBePartial", ReasonEnum(guardrail))
	assert.Contains(t, dec.SkipReason(), "refused, not partially applied")
	assert.Equal(t, "clamp", verdictKind(dec, GuardrailBlastRadius), "the cap itself is in force, not relaxed")
	_, err := dec.Ticket()
	assert.Error(t, err, "no ticket is issued")
}

func TestWholeRollback_TheSameNarrowingOnAnOrdinaryPlanIsStillAClamp(t *testing.T) {
	// The rule is the rollback's: any other plan the cap narrows is issued narrowed.
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("2Gi", cpuAt500m)))
	ra := twoFieldRollbackAction(func(ra *dorguv1.RemediationAction) {
		ra.Annotations = nil
		ra.Spec.Provenance.Source = dorguv1.ProposalSourceDorguPlanner
	})

	dec := approvalRun(t, g, ra)

	require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
	assert.Equal(t, ResultClamp, dec.Result)
}

func TestWholeRollback_AWholeRestoreStillGetsItsTicket(t *testing.T) {
	// Live memory 512Mi: restoring 256Mi is a 50% decrease, inside the cap.
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("512Mi", cpuAt500m)))

	dec := approvalRun(t, g, twoFieldRollbackAction())

	require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
	assert.Equal(t, ResultPass, dec.Result)
	_, err := dec.Ticket()
	assert.NoError(t, err)
}

func TestWholeRollback_TheProposalPhaseLeavesItToIntake(t *testing.T) {
	// Intake rejects with its own reason and makes no action; the gate's own run at the
	// proposal phase reports the clamp and lets intake decide.
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("2Gi", cpuAt500m)))
	cand := planCandidate(twoFieldStep())
	cand.RollbackOf = appliedActionName

	dec := run(t, g, cand, PhaseProposal)

	assert.Equal(t, ResultClamp, dec.Result)
	assert.Nil(t, verdict(dec, GuardrailRollbackWouldBePartial))
}

// The apply-time re-run: a ticket that is exactly what the re-run would now derive is
// honoured, so a ticket narrowed to cpu alone (forged by a writer of status, or issued
// before this rule existed) passes the patch comparison. For a rollback it is refused.
func narrowedRollbackTicket(mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	mutators := append([]func(*dorguv1.RemediationAction){func(ra *dorguv1.RemediationAction) {
		ra.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: appliedActionName}
		ra.Spec.Provenance = &dorguv1.RemediationProvenance{
			Proposer: dorguv1.ProposerIdentity{Username: "system:serviceaccount:dorgu-system:dorgu-operator"},
			Source:   dorguv1.ProposalSourceRollback, RequestedBy: requester,
		}
		ra.Spec.Steps = []dorguv1.RemediationStep{twoFieldStep()}
		ra.Status.ApplyTicket.Patch = jsonOf(`{"spec":{"resources":{"limits":{"cpu":"250m"}}}}`)
	}}, mutate...)
	return ticketedAction(mutators...)
}

func TestWholeRollback_ApplyRefusesATicketThatRestoresOnlyPartOfTheRollback(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("2Gi", cpuAt500m)))

	problem := checkTicket(t, g, narrowedRollbackTicket())

	require.NotNil(t, problem, "a ticket for part of a restore is not honoured")
	assert.Equal(t, GuardrailRollbackWouldBePartial, problem.Guardrail)
}

func TestWholeRollback_ApplyHonoursTheSameNarrowTicketOnAnOrdinaryPlan(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("2Gi", cpuAt500m)))
	ra := narrowedRollbackTicket(func(ra *dorguv1.RemediationAction) {
		ra.Annotations = nil
		ra.Spec.Provenance.Source = dorguv1.ProposalSourceDorguPlanner
	})

	problem := checkTicket(t, g, ra)

	assert.Nil(t, problem, "only a rollback must be whole; a clamped ordinary plan is honoured as before")
}
