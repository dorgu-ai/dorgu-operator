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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// --- R1: the gate measures exactly what the executor applies --------------------

func TestR1_ActionPatchIsMeasuredEvenWhenStepsArePresent(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	cand := withActionPatch(planCandidate(personaUpdateStep(memPatch("384Mi"))), string(memPatch("999Gi").Raw))

	d := run(t, g, cand, PhaseProposal)

	assert.NotEqual(t, "999Gi", actionValues(d)[memLimit],
		"the executor applies Action.Patch, so a 999Gi value there must not pass the blast radius")
	assert.NotEqual(t, ResultPass, d.Result)
}

func TestR1_ActionPatchGetsTheAllowlistToo(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	cand := withActionPatch(planCandidate(personaUpdateStep(memPatch("384Mi"))),
		`{"spec":{"policies":{"security":{"runAsNonRoot":false}},"resources":{"limits":{"memory":"384Mi"}}}}`)

	d := run(t, g, cand, PhaseProposal)

	assert.Equal(t, []string{memLimit}, PatchLeafPaths(d.Action.Patch.Raw),
		"only the allowlisted resource change may survive in the action the executor applies")
}

func TestR1_NothingLeftInActionPatchStillCountsAsAChange(t *testing.T) {
	// At approval the plan's only appliable change is Action.Patch, and it is now
	// far outside the cap: nothing is left to apply, and the run must say so.
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	cand := withActionPatch(planCandidate(), string(memPatch("999Gi").Raw))
	cand.Steps = nil

	d := run(t, g, cand, PhaseApprovalRecheck)

	_, guardrail := refusedBy(t, d)
	assert.Equal(t, GuardrailBlastRadius, guardrail)
}

// --- R2: gone or ambiguous at approval refuses ----------------------------------

func recheckCandidate(uid string) Candidate {
	cand := planCandidate(personaUpdateStep(memPatch("384Mi")))
	cand.Facts.PriorObserved = &dorguv1.GateObserved{WorkloadUID: uid}
	return cand
}

func TestR2_DeletedWorkloadRefusesAtApproval(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"))) // the Deployment is gone

	stage, guardrail := refusedBy(t, run(t, g, recheckCandidate("uid-1"), PhaseApprovalRecheck))

	assert.Equal(t, StageGround, stage)
	assert.Equal(t, GuardrailWorkloadGone, guardrail)
}

func TestR2_AmbiguousWorkloadRefusesAtApproval(t *testing.T) {
	second := deployment("64Mi", func(d *appsv1.Deployment) { d.Name = "web-canary"; d.UID = "uid-2" })
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), second))

	stage, guardrail := refusedBy(t, run(t, g, recheckCandidate("uid-1"), PhaseApprovalRecheck))

	assert.Equal(t, StageGround, stage)
	assert.Equal(t, GuardrailWorkloadAmbiguous, guardrail)
}

func TestR2_ReplacedWorkloadStillRefuses(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))

	stage, guardrail := refusedBy(t, run(t, g, recheckCandidate("uid-OTHER"), PhaseApprovalRecheck))

	assert.Equal(t, StageGround, stage)
	assert.Equal(t, GuardrailLiveWorkload, guardrail)
}

// --- R3: a read error, or a field with no baseline, never passes -----------------

func TestR3_DeploymentReadErrorAtProposalFailsClosed(t *testing.T) {
	c := failingListClient(t, isKind[*appsv1.DeploymentList], persona("256Mi"))
	g := newTestGate(t, c)

	d, err := g.Run(context.Background(), planCandidate(personaUpdateStep(memPatch("9999Ti"))), PhaseProposal)

	require.Error(t, err, "an API read error means the gate cannot decide, and must not hand back a pass")
	assert.Nil(t, d)
}

func TestR3_FieldWithNoBaselineCannotPassBlastRadius_Steps(t *testing.T) {
	noLimits := persona("")
	noLimits.Spec.Resources = nil
	g := newTestGate(t, fakeClient(t, noLimits)) // no Deployment either: nothing to measure against

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("9999Ti"))), PhaseProposal)

	assert.NotEqual(t, ResultPass, d.Result)
	for _, step := range d.Steps {
		assert.NotEqual(t, "9999Ti", stepPatchValue(t, step), "an unmeasured value must not survive")
	}
	assert.Equal(t, "clamp", verdictKind(d, GuardrailNoBaseline))
}

func TestR3_FieldWithNoBaselineCannotPassBlastRadius_LegacyAction(t *testing.T) {
	noLimits := persona("")
	noLimits.Spec.Resources = nil
	g := newTestGate(t, fakeClient(t, noLimits))

	d := run(t, g, legacyCandidate(memPatch("9999Ti")), PhaseProposal)

	_, guardrail := refusedBy(t, d)
	assert.Equal(t, GuardrailNoBaseline, guardrail)
}

// --- R4: a missing persona refuses at proposal too --------------------------------

func TestR4_MissingPersonaRefusesAtProposal(t *testing.T) {
	g := newTestGate(t, fakeClient(t, deployment("256Mi"))) // no ApplicationPersona

	stage, guardrail := refusedBy(t, run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal))

	assert.Equal(t, StageGround, stage)
	assert.Equal(t, GuardrailPersona, guardrail)
}

func TestR4_MissingPersonaRefusesALegacyActionAtProposal(t *testing.T) {
	g := newTestGate(t, fakeClient(t, deployment("256Mi")))

	_, guardrail := refusedBy(t, run(t, g, legacyCandidate(memPatch("384Mi")), PhaseProposal))

	assert.Equal(t, GuardrailPersona, guardrail)
}

func TestR4_MissingPersonaDoesNotRefuseAnAdvisoryOnlyIntent(t *testing.T) {
	// The preflight intent carries no plan; it must reach the planner.
	g := newTestGate(t, fakeClient(t))
	intent := Candidate{Target: target(), Namespace: testNS}

	assert.NotEqual(t, GuardrailPersona, func() string { _, gr := run(t, g, intent, PhaseProposal).RefusedBy(); return gr }())
}
