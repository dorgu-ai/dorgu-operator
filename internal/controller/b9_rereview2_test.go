package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// Re-review 2 (head 8301cb3). Independent tests, not part of the PR.

func rejectedRA(t *testing.T, e *recurrenceEnv, name, incident, answers string, rejectedAgo time.Duration) *dorguv1.RemediationAction {
	t.Helper()
	ra := raFor(name, incident, RemediationPhaseRejected)
	ra.CreationTimestamp = metav1.NewTime(time.Now().Add(-20 * time.Minute))
	ra.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	if answers != "" {
		ra.Annotations = map[string]string{gate.AnnotationAnswersSignal: answers}
	}
	require.NoError(t, e.c.Create(context.Background(), ra))
	ra.Status.Phase = RemediationPhaseRejected
	ra.Status.Conditions = []metav1.Condition{{
		Type: ConditionRejected, Status: metav1.ConditionTrue, Reason: ReasonUserRejected,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-rejectedAgo)),
	}}
	require.NoError(t, e.c.Status().Update(context.Background(), ra))
	return ra
}

func (e *recurrenceEnv) nonRejectedActions() int {
	n := 0
	for _, a := range e.actions() {
		if a.Status.Phase != RemediationPhaseRejected {
			n++
		}
	}
	return n
}

// Q1 control: a rejection of the SAME shape holds. Variant A: an editor (update on
// remediationactions, no status verb) rewrites the answers-signal annotation to a
// milder shape and the rejection is released. Variant B: the same verb could already
// release it by editing spec.incidentRef (pre-existing, no CRD immutability rule).
func TestRereview2_Q1_AnnotationForgeryReleasesARejection_Characterization(t *testing.T) {
	setup := func(t *testing.T) (*recurrenceEnv, *dorguv1.RemediationAction) {
		e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
		e.setOOM()
		e.cycle() // creates the incident and a real OOM proposal
		for _, a := range e.actions() {
			require.NoError(t, e.c.Delete(context.Background(), &a))
		}
		inc := e.onlyOpen()
		ra := rejectedRA(t, e, "rejected-oom", inc.Name, "OOMKilled", 10*time.Minute)
		return e, ra
	}

	t.Run("control: same shape rejection holds", func(t *testing.T) {
		e, _ := setup(t)
		e.cycle()
		assert.Zero(t, e.nonRejectedActions(), "a 10 minute old no to OOMKilled holds")
	})
	t.Run("annotation edited to a milder shape releases it", func(t *testing.T) {
		e, ra := setup(t)
		var live dorguv1.RemediationAction
		require.NoError(t, e.c.Get(context.Background(), client.ObjectKeyFromObject(ra), &live))
		live.Annotations[gate.AnnotationAnswersSignal] = "ReadinessNotReady"
		require.NoError(t, e.c.Update(context.Background(), &live))
		e.cycle()
		t.Logf("proposals after the annotation edit: %d", e.nonRejectedActions())
		assert.NotZero(t, e.nonRejectedActions(), "released (a new Pending proposal; still needs a human)")
	})
	t.Run("pre-existing: spec.incidentRef edited releases it too (same verb)", func(t *testing.T) {
		e, ra := setup(t)
		var live dorguv1.RemediationAction
		require.NoError(t, e.c.Get(context.Background(), client.ObjectKeyFromObject(ra), &live))
		live.Spec.IncidentRef.Name = "something-else-0123456789ab"
		require.NoError(t, e.c.Update(context.Background(), &live))
		e.cycle()
		assert.NotZero(t, e.nonRejectedActions())
	})
	t.Run("unannotated rejection released by a forged escalated-at on the incident", func(t *testing.T) {
		e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
		e.setOOM()
		e.cycle()
		for _, a := range e.actions() {
			require.NoError(t, e.c.Delete(context.Background(), &a))
		}
		inc := e.onlyOpen()
		rejectedRA(t, e, "rejected-by-hand", inc.Name, "", 10*time.Minute)
		e.cycle()
		require.Zero(t, e.nonRejectedActions(), "control: held")
		var im dorguv1.IncidentMemory
		require.NoError(t, e.c.Get(context.Background(), client.ObjectKeyFromObject(&inc), &im))
		if im.Annotations == nil {
			im.Annotations = map[string]string{}
		}
		im.Annotations[gate.AnnotationEscalatedAt] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		require.NoError(t, e.c.Update(context.Background(), &im))
		e.cycle()
		assert.NotZero(t, e.nonRejectedActions(), "released by an annotation on the incident")
	})
	t.Run("pre-existing: a lowered spec.severity on the incident releases it too", func(t *testing.T) {
		// What the gate already trusted before N-3: incident.Spec.Severity is user-editable.
		e, _ := setup(t)
		inc := e.onlyOpen()
		var im dorguv1.IncidentMemory
		require.NoError(t, e.c.Get(context.Background(), client.ObjectKeyFromObject(&inc), &im))
		im.Spec.Severity = "info"
		require.NoError(t, e.c.Update(context.Background(), &im))
		e.cycle()
		t.Logf("severity after cycle: %s, proposals: %d", e.onlyOpen().Spec.Severity, e.nonRejectedActions())
	})
}

// Q2 variant (N-2 through the lineage): a Pending advisory belongs to the PREVIOUS
// generation of the fault. supersedePending matches IncidentRef.Name exactly, so the
// escalated recurrence's OOM proposal is deferred behind it by the gate's lineage
// dedup, which is exactly N-2 again.
func TestRereview2_N2_PendingAdvisoryOfAnEarlierGenerationDefersTheEscalation(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	gen0 := e.onlyOpen()
	ra := advisoryFor(t, e, gen0.Name, RemediationPhasePending)
	e.resolve(gen0.Name)

	e.setReadiness()
	e.cycle() // recurrence, generation 1
	gen1 := e.onlyOpen()
	require.NotEqual(t, gen0.Name, gen1.Name)

	e.setOOM()
	e.cycle()
	require.Equal(t, "OOMKilled", e.onlyOpen().Spec.Detection.Signal)

	assert.NotZero(t, e.otherActions(ra.Name),
		"the incident is OOMKilled/critical but the OOM proposal is deferred behind generation 0's advisory (%s)",
		e.actionNamed(ra.Name).Status.Phase)
}

// Q3: a signal that flaps between shapes. Each escalation supersedes the outstanding
// Pending proposal and mints another, each de-escalation reshapes the incident. With
// the default rate limit (5 per hour per persona) the proposals run out and the last
// escalation supersedes the standing proposal and cannot replace it.
func TestRereview2_Q3_FlappingBetweenShapesChurnsProposalsThenStarves(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	rec := record.NewFakeRecorder(1000)
	e.r.Recorder = rec

	for range 12 {
		e.setOOM()
		e.cycle()
		stampCreationNow(t, e)
		e.setReadiness() // OOM stale, container not Ready: de-escalates
		e.cycle()
	}
	e.setOOM()
	e.cycle()

	var pending, superseded, total int
	for _, a := range e.actions() {
		total++
		switch a.Status.Phase {
		case RemediationPhasePending:
			pending++
		case RemediationPhaseExpired:
			superseded++
		}
	}
	evs := drainEvents(rec)
	supersededEvents := 0
	for _, ev := range evs {
		if hasEvent([]string{ev}, "Normal", EventReasonSuperseded) {
			supersededEvents++
		}
	}
	t.Logf("12 flaps: incidents=%d (open %d) RAs=%d pending=%d expired=%d supersede-events=%d total-events=%d",
		len(e.incidents()), len(e.open()), total, pending, superseded, supersededEvents, len(evs))
	assert.Len(t, e.open(), 1, "still one incident")
	assert.Equal(t, "OOMKilled", e.onlyOpen().Spec.Detection.Signal)
	assert.Equal(t, 1, pending, "a critical OOM incident should end with a standing proposal for a human to answer")
}

// Q2 races. The CLI approves with a status merge patch carrying no resourceVersion.
// (a) The patch lands between the operator's Get and its Update: the Update must
// conflict and the human's approval must survive.
func TestRereview2_Q2_ApprovalBetweenGetAndUpdateSurvives(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	ra := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhasePending)

	base := e.c
	injected := false
	e.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*dorguv1.RemediationAction); ok && !injected {
				injected = true
				var live dorguv1.RemediationAction
				require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(obj), &live))
				patch := client.MergeFrom(live.DeepCopy())
				live.Status.Phase = RemediationPhaseApproved
				live.Status.ApprovedBy = "cli-user"
				require.NoError(t, cl.Status().Patch(ctx, &live, patch))
			}
			return cl.Status().Update(ctx, obj, opts...)
		},
	})
	e.setOOM()
	e.cycle()

	got := e.actionNamed(ra.Name)
	assert.True(t, injected)
	assert.Equal(t, RemediationPhaseApproved, got.Status.Phase, "the human's approval wins the race")
	for _, c := range got.Status.Conditions {
		assert.NotEqual(t, ReasonSuperseded, c.Reason)
	}
}

// (b) The patch lands after the supersede (the CLI read Pending, then the operator
// expired it). Reconcile restores Expired, issues no ticket, and does not loop.
func TestRereview2_Q2_ApprovalAfterSupersedeIsUndoneOnceAndDoesNotLoop(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	ra := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhasePending)
	e.setOOM()
	e.cycle()
	require.Equal(t, RemediationPhaseExpired, e.actionNamed(ra.Name).Status.Phase)

	live := e.actionNamed(ra.Name)
	patch := client.MergeFrom(live.DeepCopy())
	live.Status.Phase = RemediationPhaseApproved
	live.Status.ApprovedBy = "cli-user"
	live.Status.ApplyTicket = &dorguv1.ApplyTicket{}
	require.NoError(t, e.c.Status().Patch(context.Background(), &live, patch))

	rec := record.NewFakeRecorder(50)
	rc := &RemediationController{Client: e.c, Logger: noiseLogger(), Recorder: rec}
	for range 4 {
		_, err := rc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
		require.NoError(t, err)
	}
	after := e.actionNamed(ra.Name)
	assert.Equal(t, RemediationPhaseExpired, after.Status.Phase)
	assert.Nil(t, after.Status.ApplyTicket)
	assert.Len(t, drainEvents(rec), 1, "one Warning, not one per reconcile")
	// The approval stamps are left on an Expired record (cosmetic): record it.
	t.Logf("approvedBy left on the Expired record: %q", after.Status.ApprovedBy)
}

// (c) Every non-Expired phase is restored, including ones past the ticket.
func TestRereview2_Q2_EveryForcedPhaseOfASupersededProposalIsRestored(t *testing.T) {
	for _, phase := range []string{RemediationPhaseApproved, RemediationPhaseTicketed, RemediationPhaseApplying,
		RemediationPhaseVerifying, RemediationPhaseCompleted, RemediationPhasePending} {
		t.Run(phase, func(t *testing.T) {
			e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
			e.setReadiness()
			e.cycle()
			ra := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhasePending)
			e.setOOM()
			e.cycle()
			live := e.actionNamed(ra.Name)
			live.Status.Phase = phase
			require.NoError(t, e.c.Status().Update(context.Background(), &live))
			rc := &RemediationController{Client: e.c, Logger: noiseLogger(), Recorder: record.NewFakeRecorder(10)}
			_, err := rc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
			require.NoError(t, err)
			assert.Equal(t, RemediationPhaseExpired, e.actionNamed(ra.Name).Status.Phase)
		})
	}
}

// Q4 prune. (a) The incident is reopened between the live Get and the Delete: the
// resourceVersion precondition must stop the delete.
func TestRereview2_Q4_ReopenBetweenLiveReadAndDeleteIsNotDeleted(t *testing.T) {
	stale := resolvedAt(pruneBase, "OOMKilled", 60*day)
	newest := resolvedAt(pruneBase+"-r1", "OOMKilled", 40*day)
	base := fake.NewClientBuilder().WithScheme(noiseScheme(t)).WithObjects(stale, newest).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}).Build()
	reopened := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if im, ok := obj.(*dorguv1.IncidentMemory); ok && im.Name == pruneBase && !reopened {
				reopened = true
				var live dorguv1.IncidentMemory
				require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(im), &live))
				live.Labels[LabelPhase] = PhaseDetected
				live.Spec.Detection.LastSeen = metav1.Now()
				require.NoError(t, cl.Update(ctx, &live))
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})
	r := newNoiseReconciler(c)
	r.ResolvedRetention = retention30
	_ = r.pruneResolved(context.Background(), time.Now())
	assert.True(t, reopened)
	assert.True(t, exists(t, base, pruneBase), "an incident reopened between the live read and the delete was deleted")
}

// (b) A failed live read, and a failed live RA list, delete nothing.
func TestRereview2_Q4_FailedLiveReadsDeleteNothing(t *testing.T) {
	for name, fail := range map[string]func(live *failing){
		"live get":     func(f *failing) { f.get = true },
		"live RA list": func(f *failing) { f.listRA = true },
	} {
		t.Run(name, func(t *testing.T) {
			stale := resolvedAt(pruneBase, "OOMKilled", 60*day)
			newest := resolvedAt(pruneBase+"-r1", "OOMKilled", 40*day)
			base := fake.NewClientBuilder().WithScheme(noiseScheme(t)).WithObjects(stale, newest).
				WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}).Build()
			f := &failing{}
			fail(f)
			live := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if f.get {
						return errors.New("apiserver down")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*dorguv1.RemediationActionList); ok && f.listRA {
						return errors.New("apiserver down")
					}
					if _, ok := list.(*dorguv1.RemediationActionList); ok {
						f.calls++
					}
					return cl.List(ctx, list, opts...)
				},
			})
			r := newNoiseReconciler(base)
			r.APIReader = live
			r.ResolvedRetention = retention30
			err := r.pruneResolved(context.Background(), time.Now())
			assert.Error(t, err)
			assert.True(t, exists(t, base, pruneBase), "nothing is deleted when the live read fails")
		})
	}
}

type failing struct {
	get, listRA bool
	calls       int
}

// N-1 variants. Two replicas share the container name "api". Replica 1 crash-loops
// while replica 2 is only not Ready, at the same time: two faults, two incidents.
// Then replica 1 recovers: the readiness fault remains, as its own incident, and
// the crash-loop incident resolves rather than staying open and critical.
func TestRereview2_N1_ReplicasShareAContainerNameButNotAFault(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	p1 := dorguv1.ResourceReference{Kind: "Pod", Name: "api-1", Namespace: recurrenceNS}
	p2 := dorguv1.ResourceReference{Kind: "Pod", Name: "api-2", Namespace: recurrenceNS}
	crash := e.signalFor(detection.SignalCrashLoopBackOff, detection.CategoryHealth, detection.SeverityCritical, p1, "api")
	ready := e.signalFor(detection.SignalReadinessNotReady, detection.CategoryHealth, detection.SeverityWarning, p2, "api")

	e.count.signals = []detection.Signal{ready}
	e.cycle()
	e.count.signals = []detection.Signal{crash, ready}
	e.cycle()
	t.Logf("both live: %v", incidentSignals(e.incidents()))
	assert.Len(t, e.open(), 2, "two replicas, two different faults")

	e.count.signals = []detection.Signal{ready}
	e.cycle()
	t.Logf("crash cleared: %v", incidentSignals(e.incidents()))
	kinds := make([]string, 0, len(e.open()))
	for _, im := range e.open() {
		kinds = append(kinds, im.Spec.Detection.Signal)
	}
	assert.Contains(t, kinds, "ReadinessNotReady")
}
