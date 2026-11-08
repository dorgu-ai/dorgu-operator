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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func TestGround_ReadsTheLiveWorkloadAndPersona(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("128Mi")))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("192Mi"))), PhaseProposal)

	require.NotNil(t, d.WorkloadRef)
	assert.Equal(t, testPersona, d.WorkloadRef.Name)
	require.NotNil(t, d.Observation)
	require.NotNil(t, d.Persona)
	assert.Equal(t, "uid-1", d.Observed.WorkloadUID)
	assert.Equal(t, int64(3), d.Observed.PersonaGeneration)
	assert.Equal(t, "128Mi", d.Observed.LiveValues.Limits.Memory, "the LIVE value, not the persona's 256Mi")
	assert.NotNil(t, d.Observed.ObservedAt)
}

func TestGround_PrePatchStateIsLiveNeverTheAuthors(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("128Mi")))
	step := personaUpdateStep(memPatch("192Mi"))
	step.PrePatchState = memPatch("999Gi") // the author's claim
	d := run(t, g, planCandidate(step), PhaseProposal)

	require.NotNil(t, d.PrePatchState)
	assert.Equal(t, "128Mi", PatchLeafValues(d.PrePatchState.Raw)[memLimit])
}

func TestGround_UnresolvedWorkloadFallsBackToThePersonaBaseline(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"))) // no Deployment
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	assert.False(t, Resolved(d.WorkloadRef))
	assert.False(t, d.Ownership.Resolved)
	assert.Equal(t, dorguv1.ManagedByUnknown, d.Ownership.ManagedBy)
	assert.Empty(t, d.Observed.WorkloadUID)
	assert.Equal(t, "256Mi", PatchLeafValues(d.PrePatchState.Raw)[memLimit], "the persona's value is the baseline when nothing live resolves")
}

func TestGround_NoNamespaceMeansNoRead(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("128Mi")))
	// An intent (no plan yet) reaches the deny-list, which refuses a namespace it
	// cannot determine.
	intent := Candidate{Target: target(), Namespace: ""}
	intent.Target.Namespace = ""

	d := run(t, g, intent, PhaseProposal)

	assert.False(t, Resolved(d.WorkloadRef))
	assert.Equal(t, "refuse", verdictKind(d, GuardrailDenyList), "an undeterminable namespace fails closed")

	// A plan with no namespace has no persona to land on, and is refused earlier.
	plan := planCandidate(personaUpdateStep(memPatch("192Mi")))
	plan.Target.Namespace, plan.Namespace = "", ""
	assert.Equal(t, ResultRefuse, run(t, g, plan, PhaseProposal).Result)
}

func TestGround_AmbiguousWorkloadIsUnresolvedNotGuessed(t *testing.T) {
	second := deployment("64Mi", func(d *appsv1.Deployment) { d.Name = "web-canary"; d.UID = "uid-2" })
	second.Labels = map[string]string{"app": testPersona}
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("128Mi"), second))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("192Mi"))), PhaseApprovalRecheck)

	// Ambiguity is not a read failure: it degrades to unresolved even at approval.
	assert.NotNil(t, d)
}

func TestGround_ReadFailureFailsClosedAtBothPhases(t *testing.T) {
	c := failingListClient(t, isKind[*appsv1.DeploymentList], persona("256Mi"))
	g := newTestGate(t, c)
	cand := planCandidate(personaUpdateStep(memPatch("300Mi")))

	for _, phase := range []Phase{PhaseProposal, PhaseApprovalRecheck} {
		d, err := g.Run(t.Context(), cand, phase)
		require.Error(t, err, "%s: an unreadable workload means the gate cannot decide", phase)
		assert.Nil(t, d)
	}
}

func TestGround_PersonaReadFailureIsReturned(t *testing.T) {
	c := interceptorGetFailure(t)
	g := newTestGate(t, c)
	_, err := g.Run(t.Context(), planCandidate(), PhaseProposal)
	require.Error(t, err)
}

func TestGround_ApprovalRefusesAReplacedWorkload(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	cand := planCandidate(personaUpdateStep(memPatch("300Mi")))
	cand.Facts.PriorObserved = &dorguv1.GateObserved{WorkloadUID: "an-older-deployment"}

	d := run(t, g, cand, PhaseApprovalRecheck)

	assert.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailLiveWorkload))
	assert.Equal(t, "an-older-deployment", verdict(d, GuardrailLiveWorkload).Numbers["proposedUID"])
}

func TestGround_ApprovalAcceptsTheSameWorkload(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	cand := planCandidate(personaUpdateStep(memPatch("300Mi")))
	cand.Facts.PriorObserved = &dorguv1.GateObserved{WorkloadUID: "uid-1"}

	d := run(t, g, cand, PhaseApprovalRecheck)

	assert.True(t, d.Allowed(), d.SkipReason())
}

func TestGround_ApprovalRefusesAMissingPersonaOnlyWhenSomethingWouldApply(t *testing.T) {
	g := newTestGate(t, fakeClient(t, deployment("256Mi"))) // no persona

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseApprovalRecheck)
	assert.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailPersona))

	advisory := planCandidate(dorguv1.RemediationStep{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "look"})
	assert.NotEqual(t, "refuse", verdictKind(run(t, g, advisory, PhaseApprovalRecheck), GuardrailPersona))
}

func TestGround_AdvisoryPlanToleratesAMissingPersona(t *testing.T) {
	g := newTestGate(t, fakeClient(t, deployment("256Mi")))
	d := run(t, g, planCandidate(dorguv1.RemediationStep{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "look",
	}), PhaseProposal)
	assert.Nil(t, d.Persona)
	assert.True(t, d.Allowed(), d.SkipReason())
}

func TestGround_NonPersonaTargetsAreNotRead(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi")))
	cand := planCandidate(dorguv1.RemediationStep{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "look"})
	cand.Target.Kind = clusterPersonaKind
	d := run(t, g, cand, PhaseProposal)
	assert.Nil(t, d.Persona)
}

func TestHasAutoApplicable(t *testing.T) {
	apply := personaUpdateStep(memPatch("300Mi"))
	tests := []struct {
		name string
		d    *Decision
		want bool
	}{
		{"an executable persona step", &Decision{Steps: oneStep(apply)}, true},
		{"a step with the flag cleared", &Decision{Steps: oneStep(func() dorguv1.RemediationStep { s := apply; s.AutoExecutable = false; return s }())}, false},
		{"a step with no patch", &Decision{Steps: oneStep(func() dorguv1.RemediationStep { s := apply; s.Patch = nil; return s }())}, false},
		{"legacy persona action", &Decision{Action: dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch("1Gi")}}, true},
		{"legacy notification", &Decision{Action: dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}}, false},
		{"the action the executor writes counts beside steps (R1)", &Decision{
			Steps:  oneStep(func() dorguv1.RemediationStep { s := apply; s.AutoExecutable = false; return s }()),
			Action: dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch("1Gi")},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, hasAutoApplicable(tt.d)) })
	}
}

func oneStep(s dorguv1.RemediationStep) []dorguv1.RemediationStep {
	return []dorguv1.RemediationStep{s}
}

func TestPrePatchFromLive_LegacyActionAndNothingToMeasure(t *testing.T) {
	d := &Decision{
		Action:      dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch("300Mi")},
		WorkloadRef: &dorguv1.WorkloadRef{Name: "web", ObservedResources: &dorguv1.ObservedResources{Limits: &dorguv1.ResourceValues{Memory: "100Mi"}}},
	}
	got := prePatchFromLive(d, nil)
	require.NotNil(t, got)
	assert.Equal(t, "100Mi", PatchLeafValues(got.Raw)[memLimit])

	assert.Nil(t, prePatchFromLive(&Decision{}, nil), "no change, no baseline to record")
}

func TestObservedAtIsStamped(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	d := run(t, g, planCandidate(), PhaseProposal)
	assert.True(t, d.Observed.ObservedAt.Equal(&metav1.Time{Time: fixedNow}))
	_ = client.ObjectKey{}
}
