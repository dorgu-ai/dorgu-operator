package remediation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B6 re-review N-1. The health-check cycle proposes again for every open incident.
// Once the human approves, the incident's OWN RemediationAction is Approved,
// Ticketed, Applying or Verifying, and the Guardrails stage (which runs before
// DedupCooldown) answered Guardrails/concurrent: a gate stage, persisted as
// ProposalRefused plus a Warning Event for the whole approved-to-verified window.
// The incident's own fix being worked is not a refusal at any phase.
func TestB6R2_TheIncidentsOwnRemediationIsDeferredAtEveryPhase(t *testing.T) {
	phases := []string{"Pending", phaseApproved, phaseTicketed, "Applying", "Verifying"}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			p, incident := memhogWorld(t)
			diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
			first, err := p.Propose(context.Background(), diag, incident)
			require.NoError(t, err)
			require.True(t, first.Proposed)
			setPhase(t, p, first.Action, phase)

			second, err := p.Propose(context.Background(), diag, incident)
			require.NoError(t, err)

			require.False(t, second.Proposed)
			assert.False(t, gate.IsStage(second.Stage),
				"at %s the incident's own fix is in flight: %s/%s would be persisted as a refusal", phase, second.Stage, second.Reason)
			assert.Equal(t, StageDeferred, second.Stage)
			assert.Equal(t, gate.GuardrailActiveAction, second.Reason)
		})
	}
}

// Another incident's fix on a DIFFERENT field holds the persona: the plan is not
// wrong, but the persona is busy, and that is a real guardrail refusal.
func TestB6R2_AnotherIncidentsInFlightFixOnAnotherFieldStaysARefusal(t *testing.T) {
	p, incident := memhogWorld(t)
	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	first, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, first.Proposed)

	// The in-flight action belongs to some other incident and changes the CPU limit.
	ra := first.Action.DeepCopy()
	require.NoError(t, p.client.Get(context.Background(), client.ObjectKeyFromObject(ra), ra))
	ra.Spec.IncidentRef.Name = "some-other-incident"
	cpu := &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"cpu":"400m"}}}}`)}
	ra.Spec.Action.Patch = cpu
	for i := range ra.Spec.Steps {
		ra.Spec.Steps[i].Patch = cpu
	}
	require.NoError(t, p.client.Update(context.Background(), ra))
	setPhase(t, p, ra, "Ticketed")

	second, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)

	assert.Equal(t, gate.StageGuardrails, second.Stage)
	assert.Equal(t, gate.GuardrailConcurrent, second.Reason, "another incident's fix holding the persona is still a refusal")
}

func setPhase(t *testing.T, p *Proposer, action *dorguv1.RemediationAction, phase string) {
	t.Helper()
	var fresh dorguv1.RemediationAction
	require.NoError(t, p.client.Get(context.Background(), client.ObjectKeyFromObject(action), &fresh))
	fresh.Status.Phase = phase
	require.NoError(t, p.client.Status().Update(context.Background(), &fresh))
}
