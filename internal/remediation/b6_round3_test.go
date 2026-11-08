package remediation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B6 re-review 2 (round 3), NEW-1 and NEW-2. The probes are the reviewer's.

const (
	r3CPUPatch    = `{"spec":{"resources":{"limits":{"cpu":"400m"}}}}`
	r3MemPatch    = `{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`
	r3MemReqPatch = `{"spec":{"resources":{"requests":{"memory":"256Mi"}}}}`
)

// r3Clone adds another RemediationAction on the same persona, for another incident,
// with the given patch and phase.
func r3Clone(t *testing.T, p *Proposer, src *dorguv1.RemediationAction, name, incident, patch, phase string) {
	t.Helper()
	ra := src.DeepCopy()
	require.NoError(t, p.client.Get(context.Background(), client.ObjectKeyFromObject(src), ra))
	ra.ObjectMeta = metav1.ObjectMeta{Name: name, Namespace: src.Namespace}
	ra.Spec.IncidentRef.Name = incident
	j := &apiextensionsv1.JSON{Raw: []byte(patch)}
	ra.Spec.Action.Patch = j
	for i := range ra.Spec.Steps {
		ra.Spec.Steps[i].Patch = j
	}
	ra.Status = dorguv1.RemediationActionStatus{}
	require.NoError(t, p.client.Create(context.Background(), ra))
	setPhase(t, p, ra, phase)
}

// r3Second proposes for the incident, moves its own RA to ownPhase, optionally adds
// another incident's RA, and returns the next cycle's answer.
func r3Second(t *testing.T, ownPhase, patch, otherName, otherPhase string) *ProposalResult {
	t.Helper()
	p, incident := memhogWorld(t)
	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	first, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, first.Proposed)
	setPhase(t, p, first.Action, ownPhase)
	if patch != "" {
		r3Clone(t, p, first.Action, otherName, "other-incident", patch, otherPhase)
	}
	second, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	return second
}

// NEW-1: the incident already has a proposal. Another incident's fix on ANOTHER
// field is in flight (the human approved it first). The incident's own proposal is
// still outstanding, so the answer is the deferral, whichever order the siblings
// are listed in and whichever phase the other fix is in.
func TestB6R3_AnOutstandingProposalIsDeferredEvenWhileAnotherIncidentsFixIsInFlight(t *testing.T) {
	cases := map[string]struct{ ownPhase, otherName, otherPhase string }{
		"A Pending, B approved":                 {"Pending", "aaa-other", "Approved"},
		"A Pending, B ticketed":                 {"Pending", "aaa-other", "Ticketed"},
		"A Pending, B applying (sorted last)":   {"Pending", "zzz-other", "Applying"},
		"A verifying, B ticketed (sorted 1st)":  {"Verifying", "aaa-other", "Ticketed"},
		"A verifying, B ticketed (sorted last)": {"Verifying", "zzz-other", "Ticketed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			second := r3Second(t, tc.ownPhase, r3CPUPatch, tc.otherName, tc.otherPhase)

			require.False(t, second.Proposed)
			assert.Equal(t, StageDeferred, second.Stage,
				"the incident's own proposal exists; %s/%s would be persisted as a refusal", second.Stage, second.Reason)
			assert.False(t, gate.IsStage(second.Stage))
		})
	}
}

// The refusal still fires when EVERY in-flight sibling is another incident on
// another field, and the incident has no active proposal of its own.
func TestB6R3_AnotherIncidentsFixOnAnotherFieldStillRefusesAnIncidentWithNoProposal(t *testing.T) {
	for _, patch := range []string{r3CPUPatch, r3MemReqPatch} {
		second := r3Second(t, "Completed", patch, "aaa-other", "Ticketed")

		assert.Equal(t, gate.StageGuardrails, second.Stage)
		assert.Equal(t, gate.GuardrailConcurrent, second.Reason)
	}
	// The same field, by another incident, is the redundant fix: a deferral.
	same := r3Second(t, "Completed", r3MemPatch, "aaa-other", "Ticketed")
	assert.Equal(t, StageDeferred, same.Stage)
}

// NEW-2: with maxRemediationsPerHour at 1 the incident's own outstanding proposal
// counted against the limit, so the cycle after the first proposal was "refused" by
// the rate limit. The incident's own active proposal is not another remediation.
func TestB6R3_TheRateLimitDoesNotCountTheIncidentsOwnActiveProposal(t *testing.T) {
	for _, phase := range []string{"Pending", "Approved", "Ticketed", "Applying", "Verifying"} {
		t.Run(phase, func(t *testing.T) {
			p, incident := r3LimitOneWorld(t)
			diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
			first, err := p.Propose(context.Background(), diag, incident)
			require.NoError(t, err)
			require.True(t, first.Proposed)
			r3JustCreated(t, p, first.Action)
			setPhase(t, p, first.Action, phase)

			second, err := p.Propose(context.Background(), diag, incident)
			require.NoError(t, err)

			assert.Equal(t, StageDeferred, second.Stage, "got %s/%s", second.Stage, second.Reason)
		})
	}
}

// A FINISHED proposal of the same incident still counts: the limit exists to stop
// a propose, fail, propose loop.
func TestB6R3_AFinishedProposalOfTheSameIncidentStillCountsAgainstTheRateLimit(t *testing.T) {
	p, incident := r3LimitOneWorld(t)
	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)
	first, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, first.Proposed)
	r3JustCreated(t, p, first.Action)
	setPhase(t, p, first.Action, "Expired")

	second, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)

	assert.Equal(t, gate.StageGuardrails, second.Stage)
	assert.Equal(t, gate.GuardrailRateLimit, second.Reason)
}

func r3LimitOneWorld(t *testing.T) (*Proposer, *dorguv1.IncidentMemory) {
	t.Helper()
	p, incident := memhogWorld(t)
	cp := &dorguv1.ClusterPersona{ObjectMeta: metav1.ObjectMeta{Name: "cp"}, Spec: dorguv1.ClusterPersonaSpec{Name: "cp",
		Policies: &dorguv1.ClusterPolicies{SelfHealing: &dorguv1.SelfHealingPolicy{
			Enabled: true, Mode: dorguv1.SelfHealingModePropose, MaxRemediationsPerHour: 1}}}}
	require.NoError(t, p.client.Create(context.Background(), cp))
	return p, incident
}

// r3JustCreated stamps the RA as created now: the fake client leaves the
// creation time zero, which the rate limit's one-hour window would not count.
func r3JustCreated(t *testing.T, p *Proposer, action *dorguv1.RemediationAction) {
	t.Helper()
	var fresh dorguv1.RemediationAction
	require.NoError(t, p.client.Get(context.Background(), client.ObjectKeyFromObject(action), &fresh))
	fresh.CreationTimestamp = metav1.Now()
	require.NoError(t, p.client.Update(context.Background(), &fresh))
}
