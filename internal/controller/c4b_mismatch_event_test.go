package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// C4b N-3: the container-mismatch skip tells a person to raise the limit of the
// throttled container by hand. That sentence used to reach nobody at default log
// verbosity: it is the proposer's own skip, not a gate refusal, so nothing recorded it.

func mismatchResult() *remediation.ProposalResult {
	return &remediation.ProposalResult{
		Stage: remediation.StagePropose, Reason: remediation.SkipContainerMismatch,
		SkipReason: `the throttled container "sidecar" is not "app"; raise the CPU limit of "sidecar" by hand`,
	}
}

func mismatchReconciler(t *testing.T, result *remediation.ProposalResult) (*HealthCheckReconciler, *record.FakeRecorder, *stubProposer) {
	t.Helper()
	proposer := &stubProposer{result: result}
	r := newHandoffReconciler(t, proposer)
	r.Client = fake.NewClientBuilder().WithScheme(r.Client.Scheme()).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).Build()
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec
	return r, rec, proposer
}

func mismatchWarnings(evs []string) int {
	n := 0
	for _, e := range evs {
		if hasEvent([]string{e}, corev1.EventTypeWarning, EventReasonContainerMismatch) {
			n++
		}
	}
	return n
}

func TestC4bN3_AContainerMismatchSkipIsAnEventOnTheIncident(t *testing.T) {
	r, rec, _ := mismatchReconciler(t, mismatchResult())
	diag, incident := handoffFixtures()
	require.NoError(t, r.Client.Create(context.Background(), incident))

	r.proposeRemediation(context.Background(), diag, incident)

	events := drainEvents(rec)
	require.Equal(t, 1, mismatchWarnings(events), "events: %v", events)
	assert.Contains(t, events[0], `raise the CPU limit of "sidecar" by hand`, "the Event carries the advice")
}

func TestC4bN3_TheEventIsRateLimitedPerIncident(t *testing.T) {
	r, rec, proposer := mismatchReconciler(t, mismatchResult())
	diag, incident := handoffFixtures()
	require.NoError(t, r.Client.Create(context.Background(), incident))

	for range 6 {
		r.proposeRemediation(context.Background(), diag, incident)
	}
	require.Equal(t, 6, proposer.calls)
	assert.Equal(t, 1, mismatchWarnings(drainEvents(rec)),
		"the same skip every cycle is one Event, not one a minute")

	other := incident.DeepCopy()
	other.Name = "im-2"
	other.ResourceVersion = ""
	require.NoError(t, r.Client.Create(context.Background(), other))
	r.proposeRemediation(context.Background(), diag, other)
	assert.Equal(t, 1, mismatchWarnings(drainEvents(rec)),
		"another incident has its own")
}

func TestC4bN3_OtherProposerSkipsStayQuiet(t *testing.T) {
	r, rec, _ := mismatchReconciler(t, &remediation.ProposalResult{
		Stage: remediation.StagePropose, Reason: remediation.SkipNoLiveLimit, SkipReason: "no live limit",
	})
	diag, incident := handoffFixtures()
	require.NoError(t, r.Client.Create(context.Background(), incident))

	r.proposeRemediation(context.Background(), diag, incident)

	assert.Empty(t, drainEvents(rec), "only the container mismatch is surfaced here")
}

func TestC4bN3_NoRecorderIsFine(t *testing.T) {
	r, _, _ := mismatchReconciler(t, mismatchResult())
	r.Recorder = nil
	diag, incident := handoffFixtures()
	require.NoError(t, r.Client.Create(context.Background(), incident))
	assert.NotPanics(t, func() { r.proposeRemediation(context.Background(), diag, incident) })
}
