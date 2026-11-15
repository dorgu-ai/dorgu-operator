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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// C6 review F-1: the gate rules on a ticketed rollback like on any plan, except for
// two things that only make sense for a plan that undoes an action: the action being
// undone is not a failed-cooldown sibling of its own rollback, and a person's undo is
// not a proposal observe mode withholds. Everything else still applies.

const (
	otherNamespace = "another-namespace"
	requester      = "carol@example.com"
)

// rolledBackAgo is the action the operator's own advisory sends people to
// `dorgu remediation rollback` for: applied, verification degraded, the persona put
// back, the Deployment still carrying the change.
func rolledBackAgo(age time.Duration) *dorguv1.RemediationAction {
	at := metav1.NewTime(fixedNow.Add(-age))
	return appliedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.Phase = "RolledBack"
		ra.Status.AppliedAt = &at
		ra.CreationTimestamp = at
	})
}

// rollbackCandidate is the candidate the intake builds for a rollback of ra.
func rollbackCandidate(t *testing.T, g *Gate, ra *dorguv1.RemediationAction) Candidate {
	t.Helper()
	d, problem := derive(t, g, ra)
	require.Nil(t, problem)
	cand := planCandidate(d.Steps...)
	cand.Name = "rollback-of-" + ra.Name
	cand.Facts = Facts{SuggestedAction: ClassResourceAdjustment}
	cand.RollbackOf = ra.Name
	return cand
}

func TestRollbackGate_TheActionBeingUndoneIsNotItsOwnFailedCooldown(t *testing.T) {
	for _, age := range []time.Duration{time.Minute, 5 * time.Minute, 29 * time.Minute} {
		t.Run(fmt.Sprint(age), func(t *testing.T) {
			ra := rolledBackAgo(age)
			g := rollbackGate(t, deployment(wroteMemory), persona(beforeMemory), clusterPersona(nil), ra)

			dec := run(t, g, rollbackCandidate(t, g, ra), PhaseProposal)

			require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
			assert.Equal(t, ResultPass, dec.Result)
			assert.Equal(t, "pass", verdictKind(dec, GuardrailFailedCooldown))
		})
	}
}

func TestRollbackGate_WithoutRollbackOfTheSameSiblingStillCoolsDown(t *testing.T) {
	// The exemption is the candidate's doing, not the sibling's: a plan that does not
	// say it undoes this action is held for the cooldown as before.
	ra := rolledBackAgo(5 * time.Minute)
	g := rollbackGate(t, deployment(wroteMemory), persona(beforeMemory), clusterPersona(nil), ra)
	cand := rollbackCandidate(t, g, ra)
	cand.RollbackOf = ""

	dec := run(t, g, cand, PhaseProposal)

	require.False(t, dec.Allowed())
	_, guardrail := dec.RefusedBy()
	assert.Equal(t, GuardrailFailedCooldown, guardrail)
}

func TestRollbackGate_AnotherFailedActionStillBlocksTheRollback(t *testing.T) {
	// Only the action being undone is excused. Another remediation that went wrong
	// minutes ago leaves the persona just as unsettled, whoever asks.
	ra := rolledBackAgo(5 * time.Minute)
	other := sibling("ra-other-failed", "Failed", func(s *dorguv1.RemediationAction) {
		at := metav1.NewTime(fixedNow.Add(-4 * time.Minute))
		s.Status.AppliedAt = &at
	})
	g := rollbackGate(t, deployment(wroteMemory), persona(beforeMemory), clusterPersona(nil), ra, other)

	dec := run(t, g, rollbackCandidate(t, g, ra), PhaseProposal)

	require.False(t, dec.Allowed())
	stage, guardrail := dec.RefusedBy()
	assert.Equal(t, StageGuardrails, stage)
	assert.Equal(t, GuardrailFailedCooldown, guardrail)
	assert.Contains(t, dec.SkipReason(), "ra-other-failed", "it is the other action that holds the persona")
}

func TestRollbackGate_AnotherRolledBackActionBlocksTheRollback(t *testing.T) {
	ra := rolledBackAgo(5 * time.Minute)
	other := sibling("ra-other-rolledback", "RolledBack")
	g := rollbackGate(t, deployment(wroteMemory), persona(beforeMemory), clusterPersona(nil), ra, other)

	dec := run(t, g, rollbackCandidate(t, g, ra), PhaseProposal)

	require.False(t, dec.Allowed())
	assert.Contains(t, dec.SkipReason(), "ra-other-rolledback")
}

func TestRollbackGate_ARollbackDoesNotExcuseAnActionInAnotherNamespaceOfTheSameName(t *testing.T) {
	// The name is only meaningful in the candidate's namespace.
	ra := rolledBackAgo(5 * time.Minute)
	cand := rollbackCandidate(t, rollbackGate(t, deployment(wroteMemory), persona(beforeMemory), clusterPersona(nil), ra), ra)

	crossNS := sibling(ra.Name, "RolledBack", func(s *dorguv1.RemediationAction) {
		s.Namespace = otherNamespace
		s.Spec.PersonaRef.Namespace = testNS
	})
	g := rollbackGate(t, deployment(wroteMemory), persona(beforeMemory), clusterPersona(nil), crossNS)

	dec := run(t, g, cand, PhaseProposal)

	require.False(t, dec.Allowed(), "an action of the same name in another namespace is a different action")
}

func TestRollbackGate_ObserveModeAllowsAPersonsRollback(t *testing.T) {
	observe := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModeObserve })
	g := rollbackGate(t, deployment(wroteMemory), persona(wroteMemory), observe)
	d, problem := derive(t, g, appliedAction())
	require.Nil(t, problem)
	cand := planCandidate(d.Steps...)
	cand.Facts = Facts{SuggestedAction: ClassResourceAdjustment}

	refused := run(t, g, cand, PhaseProposal)
	require.False(t, refused.Allowed(), "a plan that is not a rollback is still withheld in observe mode")
	stage, guardrail := refused.RefusedBy()
	assert.Equal(t, StageModePolicy, stage)
	assert.Equal(t, GuardrailSelfHealingMode, guardrail)

	cand.RollbackOf = "ra-applied"
	dec := run(t, g, cand, PhaseProposal)

	require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
	assert.Equal(t, ResultPass, dec.Result)
	assert.Equal(t, "pass", verdictKind(dec, GuardrailSelfHealingMode))
}

func TestRollbackGate_ObserveModeStillRunsTheOtherGuardrailsOnARollback(t *testing.T) {
	// Observe lets a person undo; it does not open the rest of the gate.
	observe := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModeObserve })
	busy := sibling("ra-busy", "Applying")
	g := rollbackGate(t, deployment(wroteMemory), persona(wroteMemory), observe, busy)
	d, problem := derive(t, g, appliedAction())
	require.Nil(t, problem)
	cand := planCandidate(d.Steps...)
	cand.Facts = Facts{SuggestedAction: ClassResourceAdjustment}
	cand.RollbackOf = "ra-applied"

	dec := run(t, g, cand, PhaseProposal)

	require.False(t, dec.Allowed())
	_, guardrail := dec.RefusedBy()
	assert.Equal(t, GuardrailConcurrent, guardrail)
}

func TestRollbackGate_TheApprovalRecheckKnowsWhatItUndoes(t *testing.T) {
	// The gate runs again when the rollback is approved. It must reach the same
	// verdict for the same reasons, or the action passes at proposal and fails at
	// approval.
	ra := rolledBackAgo(5 * time.Minute)
	rb := appliedAction(func(r *dorguv1.RemediationAction) {
		r.Name = "rollback-of-ra-applied"
		r.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: ra.Name}
		r.Spec.Provenance.Source = dorguv1.ProposalSourceRollback
		r.Spec.Provenance.RequestedBy = requester
		r.Status = dorguv1.RemediationActionStatus{Phase: "Approved", ApprovedBy: requester}
	})

	cand := ApprovalCandidate(rb, "hash")

	assert.Equal(t, ra.Name, cand.RollbackOf)
}

func TestRollbackGate_TheApprovalCandidateTrustsOnlyAnOperatorMadeRollback(t *testing.T) {
	// The annotation alone is not enough: provenance (immutable, operator-written) must
	// say the action is a rollback, or a stray annotation would excuse a cooldown.
	for name, mutate := range map[string]func(*dorguv1.RemediationAction){
		"no provenance":       func(r *dorguv1.RemediationAction) { r.Spec.Provenance = nil },
		"another source":      func(r *dorguv1.RemediationAction) { r.Spec.Provenance.Source = dorguv1.ProposalSourceDorguPlanner },
		"no annotation":       func(r *dorguv1.RemediationAction) { r.Annotations = nil },
		"an empty annotation": func(r *dorguv1.RemediationAction) { r.Annotations[dorguv1.AnnotationRollbackOf] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			rb := appliedAction(func(r *dorguv1.RemediationAction) {
				r.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: "ra-applied"}
				r.Spec.Provenance.Source = dorguv1.ProposalSourceRollback
			})
			mutate(rb)

			assert.Empty(t, ApprovalCandidate(rb, "hash").RollbackOf)
		})
	}
}

func TestRollbackGate_TheApprovalRecheckClearsARollbackOfARolledBackAction(t *testing.T) {
	ra := rolledBackAgo(5 * time.Minute)
	g := rollbackGate(t, deployment(wroteMemory), persona(beforeMemory), clusterPersona(nil), ra)
	cand := rollbackCandidate(t, g, ra)
	cand.Final = true
	cand.Facts = Facts{}

	dec := run(t, g, cand, PhaseApprovalRecheck)

	require.True(t, dec.Allowed(), "refused: %s", dec.SkipReason())
}

func TestRollbackGate_TheApprovalRecheckWithoutRollbackOfIsRefusedByTheCooldown(t *testing.T) {
	ra := rolledBackAgo(5 * time.Minute)
	g := rollbackGate(t, deployment(wroteMemory), persona(beforeMemory), clusterPersona(nil), ra)
	cand := rollbackCandidate(t, g, ra)
	cand.RollbackOf = ""
	cand.Final = true
	cand.Facts = Facts{}

	dec := run(t, g, cand, PhaseApprovalRecheck)

	require.False(t, dec.Allowed())
	_, guardrail := dec.RefusedBy()
	assert.Equal(t, GuardrailFailedCooldown, guardrail)
}
