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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// existingRA builds a RemediationAction that targets the given incident, carries
// the persona-name label the dedup List filters on, and sits in the given phase.
func existingRA(name, namespace, personaName, incidentName, phase string) *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelPersonaName: personaName},
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{Name: incidentName, Namespace: namespace},
			PersonaRef: dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      personaName,
				Namespace: namespace,
			},
			Action: dorguv1.RemediationActionDetail{Type: "persona-update"},
		},
		Status: dorguv1.RemediationActionStatus{Phase: phase},
	}
}

// TestProposer_Dedup_SkipsWhenActiveExists verifies the proposer does NOT create
// a second RemediationAction when an active (Pending) one already targets the
// incident — the core fix for the per-cycle remediation multiplicity (WS8 F2).
func TestProposer_Dedup_SkipsWhenActiveExists(t *testing.T) {
	activePhases := []string{"", "Pending", "Approved", "Applying", "Verifying"}
	for _, phase := range activePhases {
		t.Run("phase="+phaseLabel(phase), func(t *testing.T) {
			scheme := newTestScheme()
			persona := newTestPersona("default", "my-app", "256Mi", "500m")
			incident := newTestIncident("default", "oom", "my-app", "OOMKilled")
			ra := existingRA("ra-existing", "default", "my-app", incident.Name, phase)

			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(persona, ra).
				WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

			safety := NewSafetyChecker(c, testLogger())
			// A working planner is wired to prove dedup runs BEFORE the AI path.
			p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{plan: threeStepPlan()}))

			result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityCritical), incident)
			require.NoError(t, err)
			assert.False(t, result.Proposed, "must not propose a second remediation")
			assert.Contains(t, result.SkipReason, "active remediation already exists")

			var list dorguv1.RemediationActionList
			require.NoError(t, c.List(context.Background(), &list))
			assert.Len(t, list.Items, 1, "exactly the pre-existing remediation, no duplicate")
		})
	}
}

// TestProposer_Dedup_ProposesWhenPriorTerminal verifies a terminal prior
// remediation (Completed) does NOT block a fresh proposal — a recurrence after
// an earlier fix must still be actionable.
func TestProposer_Dedup_ProposesWhenPriorTerminal(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")
	ra := existingRA("ra-old", "default", "my-app", incident.Name, "Completed")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona, ra).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger())

	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityWarning), incident)
	require.NoError(t, err)
	assert.True(t, result.Proposed, "terminal prior remediation must not block a new one")
}

// TestProposer_Dedup_IgnoresOtherIncident verifies dedup is scoped per-incident:
// an active remediation for a DIFFERENT incident does not suppress this one.
func TestProposer_Dedup_IgnoresOtherIncident(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")
	other := existingRA("ra-other", "default", "my-app", "im-different", "Pending")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona, other).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger())

	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityWarning), incident)
	require.NoError(t, err)
	assert.True(t, result.Proposed, "an unrelated incident's remediation must not suppress this one")
}

// TestProposer_AIXorRule_ValidPlan verifies that when the planner returns a valid
// plan, exactly one ai-anthropic RemediationAction is created and no rule-based
// one is also emitted (WS8 F2 AI-xor-rule).
func TestProposer_AIXorRule_ValidPlan(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{plan: threeStepPlan()}))

	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	var list dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &list))
	require.Len(t, list.Items, 1, "exactly one remediation, no rule-based duplicate")
	assert.Equal(t, dorguv1.PlanSourceAIAnthropic, list.Items[0].Spec.PlanSource)
	assert.NotEmpty(t, list.Items[0].Spec.Steps, "AI plan carries ordered steps")
}

// TestProposer_AIXorRule_PlannerError verifies that when the planner errors,
// exactly one rule-based RemediationAction is created and zero AI ones (WS8 F2).
func TestProposer_AIXorRule_PlannerError(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "oom", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	safety := NewSafetyChecker(c, testLogger())
	p := NewProposer(c, safety, testLogger(), WithPlanner(&stubPlanner{err: errors.New("model down")}))

	result, err := p.Propose(context.Background(), newOOMDiagnosis("default", "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	var list dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &list))
	require.Len(t, list.Items, 1, "exactly one rule-based remediation")
	assert.NotEqual(t, dorguv1.PlanSourceAIAnthropic, list.Items[0].Spec.PlanSource, "no AI plan when planner errors")
	assert.Empty(t, list.Items[0].Spec.Steps, "rule-based path emits a single Action, not Steps")
}

func phaseLabel(p string) string {
	if p == "" {
		return "empty"
	}
	return p
}
