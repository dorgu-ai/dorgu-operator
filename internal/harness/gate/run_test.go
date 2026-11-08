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
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func TestPipelineOrder(t *testing.T) {
	g := newTestGate(t, fakeClient(t))
	assert.Equal(t,
		[]string{StageValidate, StageGround, StageOwnership, StageGuardrails, StageModePolicy, StageDedupCooldown},
		g.Stages())
}

func TestRun_NilGateCannotClearAnything(t *testing.T) {
	var g *Gate
	_, err := g.Run(context.Background(), planCandidate(), PhaseProposal)
	require.Error(t, err)
}

func TestRun_CleanPlanPassesEveryStage(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	cand := planCandidate(personaUpdateStep(memPatch("384Mi")))
	cand.Facts.Incident = &dorguv1.IncidentMemory{}
	cand.Facts.Incident.Name = incidentOne

	d := run(t, g, cand, PhaseProposal)

	require.True(t, d.Allowed(), d.SkipReason())
	assert.Equal(t, ResultPass, d.Result)
	assert.Empty(t, d.SkipReason())
	stage, guardrail := d.RefusedBy()
	assert.Empty(t, stage)
	assert.Empty(t, guardrail)
	assert.Nil(t, d.Refusal())
	require.Len(t, d.Steps, 1)
	assert.Equal(t, "384Mi", stepPatchValue(t, d.Steps[0]))

	stages := map[string]bool{}
	for _, v := range d.Verdicts {
		stages[v.Stage] = true
	}
	for _, s := range []string{StageValidate, StageGround, StageOwnership, StageGuardrails, StageModePolicy, StageDedupCooldown} {
		assert.True(t, stages[s], "stage %s recorded no verdict", s)
	}
}

func TestRun_DoesNotModifyTheCandidate(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	step := personaUpdateStep(memPatch("4Gi"))
	step.Safety = []dorguv1.StepSafety{{Rule: "forged", Verdict: "accepted"}}
	cand := planCandidate(step)

	d := run(t, g, cand, PhaseProposal)

	assert.Equal(t, "4Gi", stepPatchValue(t, cand.Steps[0]), "the caller's plan is untouched")
	assert.Len(t, cand.Steps[0].Safety, 1, "the caller's verdicts are untouched")
	assert.NotEqual(t, "4Gi", stepPatchValue(t, d.Steps[0]), "the decision carries the clamped plan")
}

func TestRun_StopsAtTheFirstRefusingStage(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi")))
	cand := planCandidate()
	cand.Target.Name = ""

	d := run(t, g, cand, PhaseProposal)

	assert.False(t, d.Allowed())
	assert.Equal(t, ResultRefuse, d.Result)
	stage, guardrail := d.RefusedBy()
	assert.Equal(t, StageValidate, stage)
	assert.Equal(t, GuardrailTarget, guardrail)
	assert.Nil(t, verdict(d, GuardrailLiveWorkload), "nothing after the refusing stage may run")
	assert.Equal(t, d.Refusal().Reason, d.SkipReason(), "a non-guardrail refusal reports its own reason")
}

func TestRun_StageErrorMeansNoDecision(t *testing.T) {
	c := failingListClient(t, isKind[*dorguv1.RemediationActionList], persona("256Mi"))
	g := newTestGate(t, c)

	d, err := g.Run(context.Background(), planCandidate(), PhaseProposal)

	require.Error(t, err, "a gate that could not list siblings cannot clear a plan")
	assert.Nil(t, d)
	assert.Contains(t, err.Error(), StageGuardrails)
}

func TestDecision_StatusRendersTheRecord(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	status := d.Status()

	assert.Equal(t, "Proposal", status.Phase)
	assert.Equal(t, "Pass", status.Result)
	assert.Equal(t, metav1Time(fixedNow), status.EvaluatedAt.Time)
	require.NotNil(t, status.Observed)
	assert.Equal(t, "uid-1", status.Observed.WorkloadUID)
	assert.Equal(t, "42", status.Observed.WorkloadResourceVersion)
	assert.Equal(t, int64(3), status.Observed.PersonaGeneration)
	require.NotNil(t, status.Observed.LiveValues)
	assert.Equal(t, "256Mi", status.Observed.LiveValues.Limits.Memory)
	require.NotNil(t, status.Ownership)
	assert.True(t, status.Ownership.Resolved)
	assert.NotEmpty(t, status.Verdicts)
	require.NotNil(t, status.PrePatchState)
	assert.Equal(t, "256Mi", PatchLeafValues(status.PrePatchState.Raw)[memLimit])

	// The record is a copy: editing it does not edit the decision.
	status.Verdicts[0].Reason = "edited"
	assert.NotEqual(t, "edited", d.Verdicts[0].Reason)
}

func TestVerdictReasonIsBounded(t *testing.T) {
	d := newDecision(&Candidate{}, PhaseProposal, metav1Now())
	d.pass("S", "g", string(make([]byte, 5000)), nil)
	assert.LessOrEqual(t, len(d.Verdicts[0].Reason), maxReasonLength+3, "the CRD bounds a reason")
	assert.Equal(t, "short", truncate("short", 10))
}

func TestPersonaKey_DefaultsTheNamespaceToTheActions(t *testing.T) {
	cand := &Candidate{Target: dorguv1.PersonaReference{Name: "web"}, Namespace: "ra-ns"}
	assert.Equal(t, client.ObjectKey{Namespace: "ra-ns", Name: "web"}, personaKey(cand))
	cand.Target.Namespace = "persona-ns"
	assert.Equal(t, client.ObjectKey{Namespace: "persona-ns", Name: "web"}, personaKey(cand))
}
