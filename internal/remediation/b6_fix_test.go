package remediation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// F-1: the second cycle's answer for an incident whose proposal is outstanding is
// a deferral with its enum reason, not a gate-stage refusal and not a suppression.
func TestB6Fix_AnOutstandingProposalIsDeferredWithItsReason(t *testing.T) {
	p, incident := memhogWorld(t)
	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	first, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, first.Proposed)

	second, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)

	assert.False(t, second.Proposed)
	assert.Equal(t, StageDeferred, second.Stage)
	assert.Equal(t, gate.GuardrailActiveAction, second.Reason)
	assert.False(t, second.Suppressed, "a deferral is not a rejection cooldown")
	require.NotNil(t, second.Decision)
	assert.True(t, second.Decision.Deferred())
}
