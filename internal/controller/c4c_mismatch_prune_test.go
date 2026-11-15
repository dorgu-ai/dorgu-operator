package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// C4c I-3: the container-mismatch Event throttle holds one key per incident. It used to
// prune only when it reached 256 keys, so the keys of resolved and deleted incidents
// stayed. A cycle now drops every key whose incident is no longer open.

func mismatchKeyCount(r *HealthCheckReconciler) int {
	if r.mismatchEvents == nil {
		return 0
	}
	return r.mismatchEvents.Len()
}

func newPruneReconciler(t *testing.T) (*HealthCheckReconciler, *dorguv1.IncidentMemory, *dorguv1.IncidentMemory) {
	t.Helper()
	crash := openIncident("platform", "checkout", "im-platform-checkout-crash") // resolves: no deployment, no pods
	keeper := openIncident("platform", "billing", "im-platform-billing-crash")
	r, c := resolverFor(t, crash, keeper, deploymentFor("platform", "billing", 1), crashLoopingPod("platform", "billing-7b98cd89d4-x", "billing"))
	r.Detection = detection.NewEngine(logr.Discard())
	r.Recorder = nil
	for _, im := range []*dorguv1.IncidentMemory{crash, keeper} {
		var live dorguv1.IncidentMemory
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(im), &live))
		r.reportContainerMismatch(&live, mismatchResult())
	}
	require.Equal(t, 2, mismatchKeyCount(r), "both incidents have an Event key")
	return r, crash, keeper
}

func TestC4cI3_AResolvedIncidentsKeyIsDroppedByTheNextCycle(t *testing.T) {
	r, crash, keeper := newPruneReconciler(t)
	_ = keeper

	r.runCycle(context.Background()) // checkout's workload is gone: its incident resolves

	require.Equal(t, PhaseResolved, phaseOf(t, r.Client, crash))
	assert.Equal(t, 1, mismatchKeyCount(r), "the resolved incident's key is gone, the open one's stays")
	ok, _ := r.mismatchEvents.Allow(crash.Namespace+"/"+crash.Name, time.Now())
	assert.True(t, ok, "the resolved incident's throttle entry was forgotten")
	ok, _ = r.mismatchEvents.Allow(keeper.Namespace+"/"+keeper.Name, time.Now())
	assert.False(t, ok, "the open incident is still throttled")
}

func TestC4cI3_ADeletedIncidentsKeyIsDroppedByTheNextCycle(t *testing.T) {
	r, _, keeper := newPruneReconciler(t)
	require.NoError(t, r.Client.Delete(context.Background(), keeper))

	r.runCycle(context.Background())

	ok, _ := r.mismatchEvents.Allow(keeper.Namespace+"/"+keeper.Name, time.Now())
	assert.True(t, ok, "the deleted incident's throttle entry was forgotten")
}

func TestC4cI3_NothingIsListedWhenNothingIsThrottled(t *testing.T) {
	r, _ := resolverFor(t)
	r.Detection = detection.NewEngine(logr.Discard())
	require.NoError(t, r.pruneMismatchEvents(context.Background()))
	assert.Nil(t, r.mismatchEvents, "no Event was ever sent: there is no throttle to prune")
}

func TestC4cI3_ARunOfOpenIncidentsKeepsEveryKey(t *testing.T) {
	r, _, _ := newPruneReconciler(t)
	require.NoError(t, r.pruneMismatchEvents(context.Background()))
	assert.Equal(t, 2, mismatchKeyCount(r), "both are still open before the resolve step ran")
}
