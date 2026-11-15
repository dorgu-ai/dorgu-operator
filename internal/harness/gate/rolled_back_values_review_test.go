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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// The review round for C6b (PR 70): the memory of a rolled-back value must keep the
// CR-01 invariant, belong to the change and not the number, end on a clock, and read
// the same at approval as at proposal.

// secondStepID names the step after the persona change in these plans.
const secondStepID = "step-2"

func cpuLive(d *appsv1.Deployment) {
	d.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("250m")
}

func cpuStep() dorguv1.RemediationStep {
	step := personaUpdateStep(jsonOf(`{"spec":{"resources":{"limits":{"cpu":"300m"}}}}`))
	step.Order, step.ID = 2, secondStepID
	return step
}

func oomCandidate(steps ...dorguv1.RemediationStep) Candidate {
	cand := planCandidate(steps...)
	cand.Final = true
	cand.Facts.TargetPath = memLimit
	return cand
}

// completedAgo is a completed rollback that finished age ago, by the operator's own
// Verified condition (the transition that moves an action to Completed).
func completedAgo(age time.Duration) *dorguv1.RemediationAction {
	return completedRollback(func(ra *dorguv1.RemediationAction) {
		ra.CreationTimestamp = metav1.NewTime(fixedNow.Add(-age - time.Hour))
		ra.Status.Conditions = []metav1.Condition{{
			Type: "Verified", Status: metav1.ConditionTrue, Reason: "HealthRestored",
			LastTransitionTime: metav1.NewTime(fixedNow.Add(-age)),
		}}
	})
}

func gateAfterRollback(t *testing.T, rb *dorguv1.RemediationAction, memory string, opts ...Option) *Gate {
	t.Helper()
	c := fakeClient(t, deployment(memory, cpuLive), persona(memory), clusterPersona(nil), appliedAction(), rb)
	return newTestGate(t, c, opts...)
}

// --- F-1: the nothing-applies invariant ---

func TestRolledBackValue_AWithheldDiagnosedChangeIsNotRescuedByAnUnrelatedStep(t *testing.T) {
	// An OOM recurs after a person rolled 256Mi to 512Mi back. The plan carries the
	// memory step (withheld) and a CPU step. The CPU step is not the fix for an OOM, and
	// it must not turn the incident into an auto-executable CPU-only proposal.
	g := gateAfterRollback(t, completedRollback(), beforeMemory)

	dec := run(t, g, oomCandidate(personaUpdateStep(memPatch(wroteMemory)), cpuStep()), PhaseProposal)

	require.False(t, dec.Allowed(), "a CPU-only plan was proposed for an out-of-memory incident")
	_, guardrail := dec.RefusedBy()
	assert.Equal(t, GuardrailRolledBackValue, guardrail)
	assert.Contains(t, dec.SkipReason(), "rolled back by a person")
	assert.Contains(t, dec.SkipReason(), wroteMemory)
}

func TestRolledBackValue_TheControlTheCapStrippedMemoryStepIsRefusedByNothingApplies(t *testing.T) {
	// The same plan with the memory step stripped by the 2x cap instead (the CR-01
	// case). The two outcomes must agree: neither leaves a CPU-only proposal.
	g := gateAfterRollback(t, completedRollback(), beforeMemory)

	dec := run(t, g, oomCandidate(personaUpdateStep(memPatch("2Gi")), cpuStep()), PhaseProposal)

	require.False(t, dec.Allowed())
	_, guardrail := dec.RefusedBy()
	assert.Equal(t, GuardrailNothingApplies, guardrail)
}

func TestRolledBackValue_AdviceInPlaceOfTheWithheldChangeIsStillTheConsideredOutcome(t *testing.T) {
	// Nothing else would apply and a person has advice to act on: that is advice, not a
	// plan that failed to produce a fix, and it is not blamed twice.
	g := gateAfterRollback(t, completedRollback(), beforeMemory)

	dec := run(t, g, oomCandidate(personaUpdateStep(memPatch(wroteMemory)), advisoryStep()), PhaseProposal)

	require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
	assert.Equal(t, ResultClamp, dec.Result)
	assert.Equal(t, "absent", verdictKind(dec, GuardrailNothingApplies))
	assert.False(t, hasAutoApplicable(dec))
}

func TestRolledBackValue_ADifferentValueForTheDiagnosedFieldStillSatisfiesTheInvariant(t *testing.T) {
	g := gateAfterRollback(t, completedRollback(), beforeMemory)

	dec := run(t, g, oomCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
	assert.Equal(t, "pass", verdictKind(dec, GuardrailNothingApplies))
}

// --- F-2: the mark is the change, not the number ---

func TestRolledBackValue_TheSameValueFromAnotherStartingPointIsNotWithheld(t *testing.T) {
	// The person's decision was about 256Mi to 512Mi. A right-size down from 1Gi, or a
	// raise from 384Mi, reaches the same number by a different decision and is held to the
	// normal gate.
	for name, live := range map[string]string{"a right-size down from 1Gi": "1Gi", "a raise from 384Mi": "384Mi"} {
		t.Run(name, func(t *testing.T) {
			g := gateAfterRollback(t, completedRollback(), live)

			dec := run(t, g, planCandidate(personaUpdateStep(memPatch(wroteMemory))), PhaseProposal)

			assert.Equal(t, "absent", verdictKind(dec, GuardrailRolledBackValue), dec.SkipReason())
			require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
			assert.Equal(t, wroteMemory, stepPatchValue(t, dec.Steps[0]))
			assert.True(t, dec.Steps[0].AutoExecutable)
		})
	}
}

func TestRolledBackValue_TheSameValueFromTheSameStartingPointIsWithheld(t *testing.T) {
	g := gateAfterRollback(t, completedRollback(), beforeMemory)

	for name, cand := range map[string]Candidate{
		"a step":          planCandidate(personaUpdateStep(memPatch(wroteMemory))),
		"the legacy form": legacyCandidate(memPatch(wroteMemory)),
	} {
		t.Run(name, func(t *testing.T) {
			dec := run(t, g, cand, PhaseProposal)
			require.False(t, dec.Allowed())
			_, guardrail := dec.RefusedBy()
			assert.Equal(t, GuardrailRolledBackValue, guardrail)
		})
	}
}

func TestRolledBackValue_TheLegacyActionFromAnotherStartingPointIsNotWithheld(t *testing.T) {
	g := gateAfterRollback(t, completedRollback(), "1Gi")

	dec := run(t, g, legacyCandidate(memPatch(wroteMemory)), PhaseProposal)

	assert.Equal(t, "absent", verdictKind(dec, GuardrailRolledBackValue), dec.SkipReason())
}

func TestMatchesRolledBack_NeedsTheSameMoveNotJustTheSameValue(t *testing.T) {
	rolled := []RolledBackValue{{Field: memLimit, From: "256Mi", Value: "512Mi"}}

	_, ok := MatchesRolledBack(rolled, memLimit, "256Mi", "512Mi")
	assert.True(t, ok)
	_, ok = MatchesRolledBack(rolled, memLimit, "268435456", "536870912")
	assert.True(t, ok, "the same quantities in another notation are the same move")

	for name, c := range map[string]struct{ field, from, value string }{
		"another starting point":    {memLimit, "1Gi", "512Mi"},
		"no starting point known":   {memLimit, "", "512Mi"},
		"another value":             {memLimit, "256Mi", "384Mi"},
		"another field":             {cpuLimit, "256Mi", "512Mi"},
		"a near value (N-1, exact)": {memLimit, "256Mi", "513Mi"},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := MatchesRolledBack(rolled, c.field, c.from, c.value)
			assert.False(t, ok)
		})
	}
}

func TestRolledBackValues_RecordTheBaselineTheyMovedFrom(t *testing.T) {
	got := rolledValues(sibs(appliedAction(), completedRollback()))

	require.Len(t, got, 1)
	assert.Equal(t, beforeMemory, got[0].From)
	assert.Equal(t, wroteMemory, got[0].Value)
}

// --- F-3: the memory ends ---

func TestRolledBackValues_LapseWhenTheLifetimeFromTheRollbacksCompletionIsUp(t *testing.T) {
	const ttl = 48 * time.Hour
	rb := completedAgo(0)
	completed := fixedNow

	for name, c := range map[string]struct {
		now  time.Time
		want int
	}{
		"just completed":          {completed, 1},
		"one nanosecond to go":    {completed.Add(ttl - time.Nanosecond), 1},
		"exactly at the boundary": {completed.Add(ttl), 0},
		"after the boundary":      {completed.Add(ttl + time.Second), 0},
	} {
		t.Run(name, func(t *testing.T) {
			got := RolledBackValues(sibs(appliedAction(), rb), c.now, ttl)
			assert.Len(t, got, c.want)
		})
	}

	got := RolledBackValues(sibs(appliedAction(), rb), completed, ttl)
	require.Len(t, got, 1)
	assert.True(t, got[0].Until.Equal(completed.Add(ttl)), "the mark says when it lapses: %v", got[0].Until)
}

func TestRolledBackValues_TheLifetimeCountsFromCompletionNotFromCreation(t *testing.T) {
	// A rollback created long ago but completed a minute ago is a minute old.
	rb := completedAgo(time.Minute)
	rb.CreationTimestamp = metav1.NewTime(fixedNow.Add(-30 * 24 * time.Hour))

	assert.Len(t, rolledValues(sibs(appliedAction(), rb)), 1)
}

func TestRolledBackValue_GateWithholdsUntilTheConfiguredLifetimeIsUp(t *testing.T) {
	cases := map[string]struct {
		age      time.Duration
		ttl      []Option
		withheld bool
	}{
		"default lifetime, six days and 23 hours": {6*24*time.Hour + 23*time.Hour, nil, true},
		"default lifetime, seven days":            {7 * 24 * time.Hour, nil, false},
		"default lifetime, eight days":            {8 * 24 * time.Hour, nil, false},
		"one hour, 30 minutes in":                 {30 * time.Minute, []Option{WithRolledBackValueTTL(time.Hour)}, true},
		"one hour, 61 minutes in":                 {61 * time.Minute, []Option{WithRolledBackValueTTL(time.Hour)}, false},
		"a zero lifetime keeps the default":       {24 * time.Hour, []Option{WithRolledBackValueTTL(0)}, true},
		"a negative lifetime keeps the default":   {24 * time.Hour, []Option{WithRolledBackValueTTL(-time.Hour)}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			g := gateAfterRollback(t, completedAgo(c.age), beforeMemory, c.ttl...)

			dec := run(t, g, planCandidate(personaUpdateStep(memPatch(wroteMemory))), PhaseProposal)

			if c.withheld {
				assert.Equal(t, "refuse", verdictKind(dec, GuardrailRolledBackValue))
				return
			}
			assert.Equal(t, "absent", verdictKind(dec, GuardrailRolledBackValue), dec.SkipReason())
			assert.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
		})
	}
}

func TestRolledBackValueTTL_DefaultsToSevenDays(t *testing.T) {
	assert.Equal(t, 7*24*time.Hour, DefaultRolledBackValueTTL)
	assert.Equal(t, DefaultRolledBackValueTTL, rollbackGate(t).RolledBackValueTTL())
	assert.Equal(t, 36*time.Hour, newTestGate(t, fakeClient(t), WithRolledBackValueTTL(36*time.Hour)).RolledBackValueTTL())
}

func TestRolledBackValueMessage_NamesTheRollbackAndWhenTheMarkLapses(t *testing.T) {
	until := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

	msg := RolledBackMessage(RolledBackValue{Field: memLimit, Value: "512Mi", Action: "ra-a", Rollback: "ra-b", Until: until})

	assert.Contains(t, msg, "ra-b")
	assert.Contains(t, msg, "ra-a")
	assert.Contains(t, msg, "until 2026-11-20 09:30 UTC")
	assert.NotContains(t, msg, "will not propose it again.", "it is not forever")
	assert.NotContains(t, msg, "—")
}

func TestRolledBackValue_TheRefusalSaysWhenItLapses(t *testing.T) {
	g := gateAfterRollback(t, completedAgo(time.Hour), beforeMemory)

	dec := run(t, g, planCandidate(personaUpdateStep(memPatch(wroteMemory))), PhaseProposal)

	want := fixedNow.Add(-time.Hour).Add(DefaultRolledBackValueTTL).UTC().Format("2006-01-02 15:04 UTC")
	assert.Contains(t, dec.SkipReason(), "until "+want)
}

// --- F-4: at approval, the right cause ---

func TestRolledBackValue_ApprovalOfAPlanWhoseOnlyChangeWasWithheldIsRefusedAsRolledBack(t *testing.T) {
	// Proposed before the rollback completed, Pending when it did. At approval the step
	// is withheld; the advice beside it must not turn the refusal into "nothing left
	// measured against the live workload".
	g := gateAfterRollback(t, completedRollback(), beforeMemory)

	for name, steps := range map[string][]dorguv1.RemediationStep{
		"with advice": {personaUpdateStep(memPatch(wroteMemory)), advisoryStep()},
		"alone":       {personaUpdateStep(memPatch(wroteMemory))},
	} {
		t.Run(name, func(t *testing.T) {
			dec := run(t, g, oomCandidate(steps...), PhaseApprovalRecheck)

			require.False(t, dec.Allowed())
			stage, guardrail := dec.RefusedBy()
			assert.Equal(t, StageGuardrails, stage)
			assert.Equal(t, GuardrailRolledBackValue, guardrail)
			assert.Equal(t, "absent", verdictKind(dec, GuardrailNothingLeft))
			assert.Contains(t, dec.SkipReason(), "rolled back by a person")
			assert.NotContains(t, dec.SkipReason(), "the live workload")
		})
	}
}

func TestRolledBackValue_ApprovalKeepsWhatIsLeftWhenAnotherChangeStillApplies(t *testing.T) {
	// Narrowing at approval is allowed when something is still left to apply.
	g := gateAfterRollback(t, completedRollback(), beforeMemory)

	dec := run(t, g, planCandidate(personaUpdateStep(memPatch(wroteMemory)), cpuStep()), PhaseApprovalRecheck)

	require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
	assert.Equal(t, "clamp", verdictKind(dec, GuardrailRolledBackValue))
	assert.True(t, hasAutoApplicable(dec))
}

// --- F-6: a rollback is never withheld, at approval or at apply ---

// rollbackOfASmallerValue is a rollback of an action that right-sized memory DOWN from
// 512Mi to 256Mi: undoing it restores 512Mi on a workload that holds 256Mi, which is
// exactly the move (256Mi to 512Mi) another person's rollback marked.
func rollbackOfASmallerValue() (*dorguv1.RemediationAction, *dorguv1.RemediationAction) {
	smaller := appliedAction(func(ra *dorguv1.RemediationAction) {
		ra.Name = "ra-right-size"
		ra.Status.Gate.PrePatchState = memPatch(wroteMemory)
		ra.Status.ApplyTicket.Patch = memPatch(beforeMemory)
	})
	observed := metav1.NewTime(fixedNow)
	rb := sibling("ra-undo-right-size", "Approved", func(ra *dorguv1.RemediationAction) {
		ra.Generation = 1
		ra.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: smaller.Name}
		ra.Spec.Provenance = &dorguv1.RemediationProvenance{
			Proposal: "p-undo", Proposer: dorguv1.ProposerIdentity{Username: "system:serviceaccount:dorgu-system:dorgu-operator"},
			Source: dorguv1.ProposalSourceRollback, RequestedBy: requester,
		}
		ra.Spec.Steps = []dorguv1.RemediationStep{personaUpdateStep(memPatch(wroteMemory))}
		ra.Status.ApprovedBy = requester
		ra.Status.ApprovedAt = &observed
		ra.Status.Gate = &dorguv1.GateStatus{
			Phase: "Proposal", Result: "Pass", Observed: &dorguv1.GateObserved{WorkloadUID: "uid-1", ObservedAt: &observed},
		}
		ra.Status.Conditions = []metav1.Condition{{
			Type: dorguv1.ConditionProposed, Status: metav1.ConditionTrue, ObservedGeneration: 1,
			LastTransitionTime: observed, Reason: "Proposed",
		}}
	})
	return smaller, rb
}

func TestRolledBackValue_ARollbackRestoringAMarkedValueIsNeverWithheldAtApprovalOrApply(t *testing.T) {
	smaller, rb := rollbackOfASmallerValue()
	c := fakeClient(t, deployment(beforeMemory, cpuLive), persona(beforeMemory), clusterPersona(nil),
		appliedAction(), completedRollback(), smaller, rb)
	g := newTestGate(t, c)
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// The mark exists: an ordinary plan for the same move is withheld.
	plain := run(t, g, oomCandidate(personaUpdateStep(memPatch(wroteMemory))), PhaseProposal)
	require.Equal(t, "refuse", verdictKind(plain, GuardrailRolledBackValue))

	for name, build := range map[string]func(*dorguv1.RemediationAction, string) Candidate{
		"approval": ApprovalCandidate,
		"apply": func(ra *dorguv1.RemediationAction, hash string) Candidate {
			// The apply-time run compares against the record the approval re-check wrote.
			ticketed := ra.DeepCopy()
			ticketed.Status.Phase = "Ticketed"
			ticketed.Status.Gate.Phase = string(PhaseApprovalRecheck)
			return TicketCandidate(ticketed, hash)
		},
	} {
		t.Run(name, func(t *testing.T) {
			cand := build(rb, hash)
			require.Equal(t, smaller.Name, cand.RollbackOf, "provenance and annotation make it a rollback")

			dec := run(t, g, cand, PhaseApprovalRecheck)

			assert.Equal(t, "absent", verdictKind(dec, GuardrailRolledBackValue), dec.SkipReason())
			require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
			assert.Equal(t, wroteMemory, stepPatchValue(t, dec.Steps[0]))
			assert.True(t, dec.Steps[0].AutoExecutable)
		})
	}

	t.Run("the same action with the rollback record stripped is withheld", func(t *testing.T) {
		stripped := rb.DeepCopy()
		stripped.Annotations = nil

		dec := run(t, g, ApprovalCandidate(stripped, hash), PhaseApprovalRecheck)

		assert.Equal(t, "refuse", verdictKind(dec, GuardrailRolledBackValue))
	})
}
