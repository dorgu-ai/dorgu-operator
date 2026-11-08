package remediation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B6 review finding F-1. After a proposal is made, every later 60 s cycle calls
// Propose again for the same open incident. The gate refuses it because the
// proposal is still outstanding. proposeRemediation persists any result whose
// Stage is a gate stage as an IncidentMemory ProposalRefused=True condition, a
// Warning Event and (in `dorgu report`) a proposal-time refusal. So the normal
// "waiting for a human" state is recorded as a refusal.
func TestB6Review_AnOutstandingProposalIsNotARecordableRefusal(t *testing.T) {
	p, incident := memhogWorld(t)
	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)

	first, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, first.Proposed)

	second, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.False(t, second.Proposed)

	t.Logf("second cycle: stage=%q reason=%q gate-stage=%v", second.Stage, second.Reason, gate.IsStage(second.Stage))
	assert.False(t, gate.IsStage(second.Stage),
		"an outstanding proposal answered by %s/%s would be persisted as ProposalRefused=True + a Warning Event "+
			"on the incident, and counted as a refusal by `dorgu report`", second.Stage, second.Reason)
}
