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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// C6b (review F-8): after a person's ticketed rollback completes, the planner must not
// offer the value the rollback undid. What the gate remembers is read only from what
// the operator wrote: the rollback action's provenance and its operator-only
// annotation, the rollback's own apply ticket, and the original action's apply ticket
// and recorded baseline. Nothing a proposer or a person can write counts.

const (
	rollbackActionName = "ra-rollback"
	anotherPersona     = "an-unrelated-persona"
)

// completedRollback is the action a person's rollback of appliedAction() becomes once
// it has finished: provenance rollback, the operator's annotation, and a ticket that
// wrote the baseline back.
func completedRollback(mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	ra := sibling(rollbackActionName, "Completed", func(ra *dorguv1.RemediationAction) {
		ra.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: appliedActionName}
		ra.Spec.Provenance = &dorguv1.RemediationProvenance{
			Proposal: "p-rollback", Proposer: dorguv1.ProposerIdentity{Username: "system:serviceaccount:dorgu-system:dorgu-operator"},
			Source: dorguv1.ProposalSourceRollback, RequestedBy: requester,
		}
		ra.Status.Gate = &dorguv1.GateStatus{Phase: "ApprovalRecheck", Result: "Pass", PrePatchState: memPatch(wroteMemory)}
		ra.Status.ApplyTicket = &dorguv1.ApplyTicket{WorkloadUID: "uid-1", Container: testPersona, Patch: memPatch(beforeMemory)}
	})
	for _, m := range mutate {
		m(ra)
	}
	return ra
}

func sibs(actions ...*dorguv1.RemediationAction) []dorguv1.RemediationAction {
	out := make([]dorguv1.RemediationAction, 0, len(actions))
	for _, a := range actions {
		out = append(out, *a)
	}
	return out
}

func TestRolledBackValues_ACompletedRollbackMarksTheValueItUndid(t *testing.T) {
	got := rolledValues(sibs(appliedAction(), completedRollback()))

	require.Len(t, got, 1)
	assert.Equal(t, memLimit, got[0].Field)
	assert.Equal(t, wroteMemory, got[0].Value, "the value the original ticket wrote, which the rollback undid")
	assert.Equal(t, appliedActionName, got[0].Action)
	assert.Equal(t, rollbackActionName, got[0].Rollback)
}

func TestRolledBackValues_OnlyACompletedRollbackCounts(t *testing.T) {
	for _, phase := range []string{
		"", "Pending", "Approved", "Ticketed", "Applying", "Verifying", "Acknowledged", "Failed", "Rejected", "Expired", "RolledBack",
	} {
		t.Run("rollback in phase "+phase, func(t *testing.T) {
			rb := completedRollback(func(ra *dorguv1.RemediationAction) { ra.Status.Phase = phase })
			assert.Empty(t, rolledValues(sibs(appliedAction(), rb)),
				"a rollback that did not complete undid nothing, so it must not mark a value refused")
		})
	}
}

func TestRolledBackValues_AForgedRecordMarksNothing(t *testing.T) {
	cases := map[string]func(*dorguv1.RemediationAction){
		"the annotation on an action that is not a rollback (no provenance)": func(ra *dorguv1.RemediationAction) {
			ra.Spec.Provenance = nil
		},
		"the annotation on an action from the planner": func(ra *dorguv1.RemediationAction) {
			ra.Spec.Provenance.Source = dorguv1.ProposalSourceDorguPlanner
		},
		"the annotation on an action from an agent": func(ra *dorguv1.RemediationAction) {
			ra.Spec.Provenance.Source = dorguv1.ProposalSourceAgent
		},
		"provenance rollback without the annotation": func(ra *dorguv1.RemediationAction) {
			ra.Annotations = nil
		},
		"an annotation naming no action": func(ra *dorguv1.RemediationAction) {
			ra.Annotations[dorguv1.AnnotationRollbackOf] = "ra-does-not-exist"
		},
		"a rollback that holds no apply ticket": func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket = nil
		},
		"a rollback ticket with no patch": func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.Patch = nil
		},
		"a rollback ticket that wrote something other than the recorded baseline": func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.Patch = memPatch("384Mi")
		},
		"a rollback ticket for a field the original did not change": func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.Patch = jsonOf(`{"spec":{"resources":{"limits":{"cpu":"250m"}}}}`)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Empty(t, rolledValues(sibs(appliedAction(), completedRollback(mutate))))
		})
	}
}

func TestRolledBackValues_TheOriginalMustBeTheOperatorsOwnRecordOfAnAppliedChange(t *testing.T) {
	cases := map[string]func(*dorguv1.RemediationAction){
		"the original never held a ticket":     func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket = nil },
		"the original recorded no baseline":    func(ra *dorguv1.RemediationAction) { ra.Status.Gate.PrePatchState = nil },
		"the original is itself a rollback":    func(ra *dorguv1.RemediationAction) { ra.Spec.Provenance.Source = dorguv1.ProposalSourceRollback },
		"the original is in another namespace": func(ra *dorguv1.RemediationAction) { ra.Namespace = otherNamespace },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Empty(t, rolledValues(sibs(appliedAction(mutate), completedRollback())))
		})
	}
}

func TestRolledBackValues_OnlyTheFieldsTheRollbackRestoredAreMarked(t *testing.T) {
	original := appliedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.Gate.PrePatchState = jsonOf(`{"spec":{"resources":{"limits":{"memory":"256Mi","cpu":"250m"}}}}`)
		ra.Status.ApplyTicket.Patch = jsonOf(`{"spec":{"resources":{"limits":{"memory":"512Mi","cpu":"500m"}}}}`)
	})
	rb := completedRollback(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.Patch = memPatch(beforeMemory)
	})

	got := rolledValues(sibs(original, rb))

	require.Len(t, got, 1, "the cpu change was not undone, so it is not refused")
	assert.Equal(t, memLimit, got[0].Field)

	rbBoth := completedRollback(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.Patch = jsonOf(`{"spec":{"resources":{"limits":{"memory":"256Mi","cpu":"250m"}}}}`)
	})
	assert.Len(t, rolledValues(sibs(original, rbBoth)), 2)
}

func TestRolledBackValues_TheSameQuantityInAnotherNotationIsTheSameValue(t *testing.T) {
	original := appliedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.Patch = memPatch("536870912") })
	rb := completedRollback(func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.Patch = memPatch("268435456") })
	original.Status.Gate.PrePatchState = memPatch("256Mi")

	require.Len(t, rolledValues(sibs(original, rb)), 1)
}

func TestRolledBackValues_TwoRollbacksOfOneValueAreListedOnce(t *testing.T) {
	again := completedRollback(func(ra *dorguv1.RemediationAction) { ra.Name = "ra-rollback-2" })
	got := rolledValues(sibs(appliedAction(), completedRollback(), again))
	assert.Len(t, got, 1)
}

func TestRolledBackValues_NothingToRememberWithoutSiblings(t *testing.T) {
	assert.Empty(t, rolledValues(nil))
	assert.Empty(t, rolledValues(sibs(appliedAction())))
}

// --- the guardrail ---

// recurrenceGate is a gate over a cluster where memory was raised to 512Mi, a person
// rolled it back, and the Deployment and persona hold 256Mi again.
func recurrenceGate(t *testing.T) *Gate {
	t.Helper()
	original := appliedAction()
	original.Status.Phase = "Completed"
	objs := []clientObject{deployment(beforeMemory), persona(beforeMemory), clusterPersona(nil), original, completedRollback()}
	return rollbackGate(t, objs...)
}

func advisoryStep() dorguv1.RemediationStep {
	return dorguv1.RemediationStep{
		Order: 2, ID: secondStepID, Type: dorguv1.StepTypeManual, Description: "read the logs", Rationale: "oom",
	}
}

func TestRolledBackValue_APlanThatOnlyRepeatsItIsRefusedWithASentenceForPeople(t *testing.T) {
	g := recurrenceGate(t)

	dec := run(t, g, planCandidate(personaUpdateStep(memPatch(wroteMemory))), PhaseProposal)

	require.False(t, dec.Allowed())
	stage, guardrail := dec.RefusedBy()
	assert.Equal(t, StageGuardrails, stage)
	assert.Equal(t, GuardrailRolledBackValue, guardrail)
	reason := dec.SkipReason()
	assert.Contains(t, reason, wroteMemory)
	assert.Contains(t, reason, memLimit)
	assert.Contains(t, reason, "rolled back by a person")
	assert.Contains(t, reason, rollbackActionName)
}

func TestRolledBackValue_ALegacyActionThatRepeatsItIsRefused(t *testing.T) {
	g := recurrenceGate(t)

	dec := run(t, g, legacyCandidate(memPatch(wroteMemory)), PhaseProposal)

	require.False(t, dec.Allowed())
	_, guardrail := dec.RefusedBy()
	assert.Equal(t, GuardrailRolledBackValue, guardrail)
}

func TestRolledBackValue_TheRepeatedValueIsWithdrawnAndTheAdviceStays(t *testing.T) {
	g := recurrenceGate(t)

	dec := run(t, g, planCandidate(personaUpdateStep(memPatch("536870912")), advisoryStep()), PhaseProposal)

	require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
	assert.Equal(t, ResultClamp, dec.Result)
	assert.Equal(t, "clamp", verdictKind(dec, GuardrailRolledBackValue))
	require.Len(t, dec.Steps, 2)
	step := dec.Steps[0]
	assert.False(t, step.AutoExecutable)
	assert.Nil(t, step.Patch, "the value is gone from the patch, so nothing can advertise or apply it")
	require.Len(t, step.Safety, 1)
	entry := step.Safety[0]
	assert.Equal(t, dorguv1.SafetyRulePlanValidation, entry.Rule)
	assert.Equal(t, dorguv1.SafetyVerdictRejected, entry.Verdict)
	assert.Equal(t, memLimit, entry.Field)
	assert.Equal(t, "536870912", entry.Requested)
	assert.Empty(t, entry.Permitted)
	assert.Contains(t, entry.Message, "rolled back by a person")
	assert.Contains(t, step.Description, entry.Message, "Dorgu's own sentence replaces the plan's description of this step")
	assert.Equal(t, dorguv1.StepTypeManual, dec.Steps[1].Type)
	assert.Equal(t, "read the logs", dec.Steps[1].Description, "advice the person did not decide on is untouched")
}

func TestRolledBackValue_ADifferentValueIsStillAllowedSubjectToTheNormalGate(t *testing.T) {
	g := recurrenceGate(t)

	dec := run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
	assert.Equal(t, ResultPass, dec.Result)
	assert.Equal(t, "384Mi", stepPatchValue(t, dec.Steps[0]))
	assert.True(t, dec.Steps[0].AutoExecutable)
	assert.Equal(t, "absent", verdictKind(dec, GuardrailRolledBackValue))
}

func TestRolledBackValue_ADifferentValueIsStillHeldToTheCap(t *testing.T) {
	g := recurrenceGate(t)

	dec := run(t, g, planCandidate(personaUpdateStep(memPatch("768Mi"))), PhaseProposal)

	_, guardrail := dec.RefusedBy()
	assert.Equal(t, "clamp", verdictKind(dec, GuardrailBlastRadius), "the cap still rules on a value that was not rolled back (%s)", guardrail)
}

func TestRolledBackValue_ApprovalAndApplyRefuseAPendingPlanThatRepeatsIt(t *testing.T) {
	// A plan proposed before the rollback completed, still Pending, must not be
	// ticketed afterwards.
	g := recurrenceGate(t)
	for _, phase := range []Phase{PhaseApprovalRecheck} {
		dec := run(t, g, planCandidate(personaUpdateStep(memPatch(wroteMemory))), phase)
		require.False(t, dec.Allowed(), "phase %v", phase)
		_, guardrail := dec.RefusedBy()
		assert.Equal(t, GuardrailRolledBackValue, guardrail)
	}
}

func TestRolledBackValue_ARollbackItselfIsNeverWithheld(t *testing.T) {
	// Rolling back the next action must be able to restore a value an earlier rollback
	// refused: what a rollback restores is the operator's own record.
	g := recurrenceGate(t)
	cand := planCandidate(personaUpdateStep(memPatch(wroteMemory)))
	cand.RollbackOf = "ra-some-later-action"

	dec := run(t, g, cand, PhaseProposal)

	assert.Equal(t, "absent", verdictKind(dec, GuardrailRolledBackValue))
}

func TestRolledBackValue_AForgedRecordLeavesTheValueOffered(t *testing.T) {
	cases := map[string]*dorguv1.RemediationAction{
		"a rollback that is still Pending": completedRollback(func(ra *dorguv1.RemediationAction) { ra.Status.Phase = "Pending" }),
		"a rollback that Failed":           completedRollback(func(ra *dorguv1.RemediationAction) { ra.Status.Phase = "Failed" }),
		"an annotation on a planner action": completedRollback(func(ra *dorguv1.RemediationAction) {
			ra.Spec.Provenance.Source = dorguv1.ProposalSourceDorguPlanner
		}),
		"an annotation with no provenance": completedRollback(func(ra *dorguv1.RemediationAction) { ra.Spec.Provenance = nil }),
	}
	for name, rb := range cases {
		t.Run(name, func(t *testing.T) {
			original := appliedAction()
			g := rollbackGate(t, deployment(beforeMemory), persona(beforeMemory), clusterPersona(nil), original, rb)

			dec := run(t, g, planCandidate(personaUpdateStep(memPatch(wroteMemory))), PhaseProposal)

			assert.Equal(t, "absent", verdictKind(dec, GuardrailRolledBackValue), "the value stays on offer: %s", dec.SkipReason())
		})
	}
}

func TestRolledBackValue_AnotherPersonasRollbackDoesNotApply(t *testing.T) {
	original := appliedAction(func(ra *dorguv1.RemediationAction) { ra.Spec.PersonaRef.Name = anotherPersona })
	rb := completedRollback(func(ra *dorguv1.RemediationAction) { ra.Spec.PersonaRef.Name = anotherPersona })
	g := rollbackGate(t, deployment(beforeMemory), persona(beforeMemory), clusterPersona(nil), original, rb)

	dec := run(t, g, planCandidate(personaUpdateStep(memPatch(wroteMemory))), PhaseProposal)

	assert.Equal(t, "absent", verdictKind(dec, GuardrailRolledBackValue))
}

func TestRolledBackValue_AnUnreadableClusterFailsTheRunRatherThanOfferingTheValue(t *testing.T) {
	c := failingListClient(t, isKind[*dorguv1.RemediationActionList], deployment(beforeMemory), persona(beforeMemory), clusterPersona(nil))
	g := newTestGate(t, c)

	_, err := g.Run(t.Context(), planCandidate(personaUpdateStep(memPatch(wroteMemory))), PhaseProposal)

	require.Error(t, err)
}

func TestRolledBackValues_GateMethodListsWhatTheProposerMustNotSizeAgain(t *testing.T) {
	g := recurrenceGate(t)
	cand := planCandidate(personaUpdateStep(memPatch(wroteMemory)))

	got, err := g.RolledBackValues(t.Context(), &cand)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, wroteMemory, got[0].Value)

	cand.RollbackOf = appliedActionName
	got, err = g.RolledBackValues(t.Context(), &cand)
	require.NoError(t, err)
	assert.Empty(t, got, "a rollback candidate is never withheld")
}

func TestRolledBackValueMessage_IsDorgusOwnSentence(t *testing.T) {
	msg := RolledBackMessage(RolledBackValue{Field: memLimit, Value: "512Mi", Action: "ra-a", Rollback: "ra-b"})
	for _, want := range []string{"512Mi", memLimit, "rolled back by a person", "ra-a", "ra-b"} {
		assert.Contains(t, msg, want)
	}
	assert.NotContains(t, msg, "—")
}

func TestRolledBackValue_AValueOnTheStepAndTheActionIsSaidOnceAndNothingElseIsBlamed(t *testing.T) {
	// The proposer's candidate carries the plan twice: as a step and as the derived
	// action. One withheld value is one sentence, and the plan is not ALSO refused as
	// "applies nothing" for the very reason that was just stated.
	g := recurrenceGate(t)
	cand := planCandidate(personaUpdateStep(memPatch(wroteMemory)))
	cand.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch(wroteMemory)}
	cand.Final = true
	cand.Facts.TargetPath = memLimit

	dec := run(t, g, cand, PhaseProposal)

	require.False(t, dec.Allowed())
	assert.Equal(t, 1, strings.Count(dec.SkipReason(), "Dorgu left out"), dec.SkipReason())
	assert.Equal(t, "absent", verdictKind(dec, GuardrailNothingApplies), "the plan applies nothing because a person said no, which is already stated")
}

func TestRolledBackValue_AWithheldValueForAnotherFieldDoesNotExcuseAPlanThatChangesNothingDiagnosed(t *testing.T) {
	// A person rolled back the memory limit. The incident is about CPU, and the plan
	// carries a memory step that is withheld and nothing for CPU: it still applies
	// nothing for what was diagnosed, and is refused as such.
	g := recurrenceGate(t)
	cand := planCandidate(personaUpdateStep(memPatch(wroteMemory)), advisoryStep())
	cand.Final = true
	cand.Facts.TargetPath = cpuLimit

	dec := run(t, g, cand, PhaseProposal)

	assert.Equal(t, "refuse", verdictKind(dec, GuardrailNothingApplies))
}

// rolledValues is RolledBackValues at the test clock with the default lifetime.
func rolledValues(actions []dorguv1.RemediationAction) []RolledBackValue {
	return RolledBackValues(actions, fixedNow, DefaultRolledBackValueTTL)
}
