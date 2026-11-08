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

package controller

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B9 review F-3: Resolved incidents older than the retention are deleted, except
// the newest of every fault (so a recurrence still links and numbers on from it)
// and any a non-terminal RemediationAction names.

const (
	day         = 24 * time.Hour
	pruneBase   = "im-shop-api-oomkilled-a476546e10c8"
	pruneOther  = "im-shop-web-crashloopbackoff-0123456789ab"
	retention30 = 30 * day
)

func gateGeneration(name string) int { return gate.IncidentGeneration(name) }

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}

func cappedCountDelta(t *testing.T, before float64) float64 {
	t.Helper()
	return cappedCount(t) - before
}

// resolvedAt builds a Resolved incident last seen the given time ago.
func resolvedAt(name, signal string, lastSeenAgo time.Duration) *dorguv1.IncidentMemory {
	im := activeIncident()
	im.Namespace = recurrenceNS
	im.Name = name
	im.Labels[LabelSignal] = signal
	im.Labels[LabelPhase] = PhaseResolved
	im.Spec.Detection.Signal = signal
	seen := metav1.NewTime(time.Now().Add(-lastSeenAgo))
	im.Spec.Detection.FirstSeen = seen
	im.Spec.Detection.LastSeen = seen
	im.Status.Phase = PhaseResolved
	return im
}

func raFor(name, incident, phase string) *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: recurrenceNS},
		Spec:       dorguv1.RemediationActionSpec{IncidentRef: dorguv1.IncidentReference{Name: incident, Namespace: recurrenceNS}},
		Status:     dorguv1.RemediationActionStatus{Phase: phase},
	}
}

func pruneReconciler(t *testing.T, retention time.Duration, objs ...client.Object) (*HealthCheckReconciler, client.Client) {
	t.Helper()
	c := noiseClient(t, objs...)
	r := newNoiseReconciler(c)
	r.ResolvedRetention = retention
	return r, c
}

func exists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var im dorguv1.IncidentMemory
	err := c.Get(context.Background(), client.ObjectKey{Namespace: recurrenceNS, Name: name}, &im)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestPrune_DeletesOldResolvedIncidentsButKeepsTheNewestOfEachFault(t *testing.T) {
	r, c := pruneReconciler(t, retention30,
		resolvedAt(pruneBase, "OOMKilled", 60*day),
		resolvedAt(pruneBase+"-r1", "OOMKilled", 45*day),
		resolvedAt(pruneBase+"-r2", "OOMKilled", 40*day),   // the newest of its fault, and old
		resolvedAt(pruneOther, "CrashLoopBackOff", 90*day), // the only one of its fault, and old
	)
	deletedBefore := metricValue(t, "dorgu_incident_prune_total", map[string]string{"result": "deleted"})

	require.NoError(t, r.pruneResolved(context.Background(), time.Now()))

	assert.False(t, exists(t, c, pruneBase), "old, not the newest: gone")
	assert.False(t, exists(t, c, pruneBase+"-r1"))
	assert.True(t, exists(t, c, pruneBase+"-r2"), "the newest of its fault is kept so numbering and links go on from it")
	assert.True(t, exists(t, c, pruneOther), "a fault's only record is its newest")
	assert.EqualValues(t, 2, metricValue(t, "dorgu_incident_prune_total", map[string]string{"result": "deleted"})-deletedBefore)
}

func TestPrune_KeepsRecentResolvedIncidents(t *testing.T) {
	r, c := pruneReconciler(t, retention30,
		resolvedAt(pruneBase, "OOMKilled", 20*day),
		resolvedAt(pruneBase+"-r1", "OOMKilled", 2*day),
	)
	require.NoError(t, r.pruneResolved(context.Background(), time.Now()))
	assert.True(t, exists(t, c, pruneBase), "inside the retention")
	assert.True(t, exists(t, c, pruneBase+"-r1"))
}

func TestPrune_NeverDeletesAnIncidentAnActiveRemediationNames(t *testing.T) {
	for _, phase := range []string{"", RemediationPhasePending, RemediationPhaseApproved, RemediationPhaseTicketed,
		RemediationPhaseApplying, RemediationPhaseVerifying} {
		t.Run("phase "+phase, func(t *testing.T) {
			r, c := pruneReconciler(t, retention30,
				resolvedAt(pruneBase, "OOMKilled", 60*day),
				resolvedAt(pruneBase+"-r1", "OOMKilled", 40*day),
				raFor("ra-1", pruneBase, phase),
			)
			keptBefore := metricValue(t, "dorgu_incident_prune_total", map[string]string{"result": "kept_referenced"})

			require.NoError(t, r.pruneResolved(context.Background(), time.Now()))

			assert.True(t, exists(t, c, pruneBase), "a %q RemediationAction still names it", phase)
			assert.EqualValues(t, 1, metricValue(t, "dorgu_incident_prune_total", map[string]string{"result": "kept_referenced"})-keptBefore)
		})
	}
}

func TestPrune_ATerminalRemediationDoesNotProtectAnIncident(t *testing.T) {
	for _, phase := range []string{RemediationPhaseCompleted, RemediationPhaseFailed, RemediationPhaseRolledBack,
		RemediationPhaseRejected, "Expired", RemediationPhaseAcknowledged} {
		t.Run(phase, func(t *testing.T) {
			r, c := pruneReconciler(t, retention30,
				resolvedAt(pruneBase, "OOMKilled", 60*day),
				resolvedAt(pruneBase+"-r1", "OOMKilled", 40*day),
				raFor("ra-1", pruneBase, phase),
			)
			require.NoError(t, r.pruneResolved(context.Background(), time.Now()))
			assert.False(t, exists(t, c, pruneBase))
		})
	}
}

func TestPrune_NeverTouchesAnOpenIncidentOrOneOnlyLabelledResolved(t *testing.T) {
	open := activeIncident()
	open.Namespace = recurrenceNS
	open.Name = pruneBase + "-r1"
	open.Spec.Detection.LastSeen = metav1.NewTime(time.Now().Add(-90 * day))

	// A user-writable label says Resolved; the status, which the operator owns, says
	// otherwise. Nothing may be deleted on the label's word.
	mislabelled := resolvedAt(pruneBase+"-r2", "OOMKilled", 90*day)
	mislabelled.Status.Phase = PhaseDetected

	r, c := pruneReconciler(t, retention30, resolvedAt(pruneBase, "OOMKilled", 120*day), open, mislabelled,
		resolvedAt(pruneOther, "CrashLoopBackOff", 5*day))
	require.NoError(t, r.pruneResolved(context.Background(), time.Now()))

	assert.True(t, exists(t, c, open.Name), "open incidents are never pruned, however old")
	assert.True(t, exists(t, c, mislabelled.Name), "the label alone does not make an incident Resolved")
}

func TestPrune_ZeroRetentionKeepsEverything(t *testing.T) {
	r, c := pruneReconciler(t, 0,
		resolvedAt(pruneBase, "OOMKilled", 900*day),
		resolvedAt(pruneBase+"-r1", "OOMKilled", 800*day),
	)
	require.NoError(t, r.pruneResolved(context.Background(), time.Now()))
	assert.True(t, exists(t, c, pruneBase))
}

func TestPrune_RunsEachHealthCheckCycle(t *testing.T) {
	r, c := pruneReconciler(t, retention30,
		resolvedAt(pruneBase, "OOMKilled", 60*day),
		resolvedAt(pruneBase+"-r1", "OOMKilled", 40*day),
	)
	r.reconcile(context.Background())
	assert.False(t, exists(t, c, pruneBase))
	assert.True(t, exists(t, c, pruneBase+"-r1"))
}

func TestPrune_ALineageStaysReconstructableAfterPruning(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.r.ResolvedRetention = retention30
	e.cycle()
	for range 3 {
		e.resolve(e.onlyOpen().Name)
		e.cycle()
	}
	// The proposals have all been answered; an outstanding one would keep its
	// incident (see TestPrune_NeverDeletesAnIncidentAnActiveRemediationNames).
	for _, ra := range e.actions() {
		ra.Status.Phase = RemediationPhaseCompleted
		require.NoError(t, e.c.Status().Update(context.Background(), &ra))
	}
	// Age every resolved incident past the retention, then resolve the latest too.
	latest := e.onlyOpen()
	e.resolve(latest.Name)
	old := metav1.NewTime(time.Now().Add(-60 * day))
	for _, im := range e.incidents() {
		fresh := e.get(im.Name)
		fresh.Spec.Detection.LastSeen = old
		fresh.Spec.Detection.FirstSeen = old
		require.NoError(t, e.c.Update(context.Background(), &fresh))
	}
	require.NoError(t, e.r.pruneResolved(context.Background(), time.Now()))

	remaining := e.incidents()
	require.Len(t, remaining, 1, "only the newest of the lineage is left")
	assert.Equal(t, latest.Name, remaining[0].Name)

	// The fault returns: it numbers on from what is left and links to it.
	e.cycle()
	recurrence := e.onlyOpen()
	assert.Equal(t, 4, gate.IncidentGeneration(recurrence.Name))
	assert.Equal(t, latest.Name, recurrence.Annotations[AnnotationRecurrenceOf])
	assert.True(t, gate.SameIncidentLineage(latest.Name, recurrence.Name))
}

func TestPrune_FailuresAreCountedAndDoNotStopTheRest(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(noiseScheme(t)).
		WithObjects(
			resolvedAt(pruneBase, "OOMKilled", 60*day),
			resolvedAt(pruneBase+"-r1", "OOMKilled", 55*day),
			resolvedAt(pruneBase+"-r2", "OOMKilled", 40*day),
		).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetName() == pruneBase {
				return errors.New("the API server said no")
			}
			return inner.Delete(ctx, obj, opts...)
		},
	})
	r := newNoiseReconciler(c)
	r.ResolvedRetention = retention30
	errorsBefore := metricValue(t, "dorgu_incident_prune_total", map[string]string{"result": "error"})

	err := r.pruneResolved(context.Background(), time.Now())

	require.Error(t, err, "a delete that failed is reported")
	assert.False(t, exists(t, c, pruneBase+"-r1"), "the others are still pruned")
	assert.EqualValues(t, 1, metricValue(t, "dorgu_incident_prune_total", map[string]string{"result": "error"})-errorsBefore)
}

func TestPrune_ListFailureIsReported(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(noiseScheme(t)).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*dorguv1.RemediationActionList); ok {
				return errors.New("no RBAC")
			}
			return inner.List(ctx, list, opts...)
		},
	})
	r := newNoiseReconciler(c)
	r.ResolvedRetention = retention30
	require.Error(t, r.pruneResolved(context.Background(), time.Now()),
		"if the RemediationActions cannot be read nothing may be deleted, since a reference could be missed")
}

var _ = detection.SignalOOMKilled
