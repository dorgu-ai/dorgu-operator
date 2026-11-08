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

package remediation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// The proposer builds candidates and writes what the gate allows. These tests
// pin the hand-off: the gate's facts reach status.gate, and what the gate
// refuses never reaches the object that is written.

func TestProposer_RulePath_RecordsTheGateDecisionOnStatus(t *testing.T) {
	p, incident := memhogWorld(t)

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	var stored dorguv1.RemediationAction
	require.NoError(t, p.client.Get(context.Background(), client.ObjectKeyFromObject(result.Action), &stored))
	g := stored.Status.Gate
	require.NotNil(t, g, "status.gate is written with the object")

	assert.Equal(t, "Proposal", g.Phase)
	assert.Equal(t, string(gate.ResultPass), g.Result)
	require.NotNil(t, g.Observed)
	require.NotNil(t, g.Observed.LiveValues)
	assert.Equal(t, "64Mi", g.Observed.LiveValues.Limits.Memory)
	require.NotNil(t, g.Ownership)
	assert.True(t, g.Ownership.Resolved)
	assert.NotEmpty(t, g.Verdicts)
	require.NotNil(t, g.PrePatchState)
	assert.Equal(t, "64Mi", gate.PatchLeafValues(g.PrePatchState.Raw)[gate.ResourcePathMemory],
		"the baseline is the live value")
	assert.Equal(t, "Pending", stored.Status.Phase)

	// The old spec fields are still populated for one release.
	require.NotNil(t, stored.Spec.WorkloadRef)
	assert.NotNil(t, stored.Spec.Action.PrePatchState)
}

func TestProposer_VerificationAndRollbackAreTheOperatorsNotThePlans(t *testing.T) {
	p, incident := memhogWorld(t)
	p.planner = &stubPlanner{plan: cleanRoomOOMPlan()}

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	rb := result.Action.Spec.Rollback
	require.NotNil(t, rb)
	assert.True(t, rb.Enabled)
	assert.Equal(t, gate.DefaultHealthCheckAfter, rb.HealthCheckAfter.Duration)
}

// OP-07 at proposal time: a plan that smuggles a policy change in beside its
// resource change keeps the resource change and loses the policy change.
func TestProposer_AIPath_StripsAPolicyChangeSmuggledIntoAPlan(t *testing.T) {
	p, incident := memhogWorld(t)
	p.planner = &stubPlanner{plan: &planner.RemediationPlan{
		RootCause: "memory limit too low", Confidence: 0.9,
		Steps: []planner.PlannedStep{{
			Order: 1, Type: dorguv1.StepTypePersonaUpdate, Description: "Raise memory",
			Patch: json.RawMessage(`{"spec":{"policies":{"security":{"runAsNonRoot":false}},` +
				`"resources":{"limits":{"memory":"96Mi","cpu":null}}}}`),
		}},
	}}

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	var all []string
	for _, step := range result.Action.Spec.Steps {
		if step.Patch != nil {
			all = append(all, gate.PatchLeafPaths(step.Patch.Raw)...)
		}
	}
	assert.Equal(t, []string{gate.ResourcePathMemory}, all, "only the memory quantity survives")
	assert.Equal(t, string(gate.ResultClamp), result.Action.Status.Gate.Result)
}

func TestProposer_GateRefusalIsReportedAsASkip(t *testing.T) {
	persona := personaWithLimits(groundedPersona, "200m", "64Mi")
	incident := newTestIncident(defaultNamespace, "oom", groundedPersona, "OOMKilled")
	cp := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: dorguv1.ClusterPersonaSpec{Name: "c", Policies: &dorguv1.ClusterPolicies{
			SelfHealing: &dorguv1.SelfHealingPolicy{Mode: dorguv1.SelfHealingModeObserve},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).WithRuntimeObjects(persona, cp).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)
	require.NoError(t, err)

	assert.False(t, result.Proposed)
	assert.Equal(t, gate.SkipReasonObserveMode, result.SkipReason)
	assert.False(t, result.Suppressed, "observe mode is not a rejection")
	require.NotNil(t, result.Decision)
	stage, _ := result.Decision.RefusedBy()
	assert.Equal(t, gate.StageModePolicy, stage)
}

func TestProposer_RejectionCooldownIsReportedAsSuppressed(t *testing.T) {
	persona := personaWithLimits(groundedPersona, "200m", "64Mi")
	incident := newTestIncident(defaultNamespace, "oom", groundedPersona, "OOMKilled")
	declined := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: "ra-declined", Namespace: defaultNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef:  dorguv1.PersonaReference{Kind: kindApplicationPersona, Name: groundedPersona, Namespace: defaultNamespace},
			IncidentRef: dorguv1.IncidentReference{Name: incident.Name, Namespace: incident.Namespace},
		},
		Status: dorguv1.RemediationActionStatus{Phase: "Rejected"},
	}
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).WithRuntimeObjects(persona, declined).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)
	require.NoError(t, err)

	assert.False(t, result.Proposed)
	assert.True(t, result.Suppressed)
}

func TestProposer_GateErrorIsAnErrorNotAPass(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).Build()
	p := NewProposer(c, nil, testLogger())
	_, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical),
		newTestIncident(defaultNamespace, "oom", groundedPersona, "OOMKilled"))
	require.Error(t, err, "a proposer with no gate cannot clear anything")
}

func TestProposer_ExposesItsGateAndPlanner(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).Build()
	g := gate.New(c, testLogger())
	p := NewProposer(c, g, testLogger(), WithPlanner(&stubPlanner{}))
	assert.Same(t, g, p.Gate())
	assert.NotNil(t, p.Planner())
}
