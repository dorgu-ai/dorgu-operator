package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B9 PR #60 re-review (head c084586). Tests the fix round's judgement calls.

func (e *recurrenceEnv) signalFor(typ detection.SignalType, cat detection.Category, sev detection.Severity,
	pod dorguv1.ResourceReference, container string,
) detection.Signal {
	return detection.Signal{
		Type: typ, Severity: sev, Category: cat, Source: "stub",
		Message:  "container " + container + ": " + string(typ),
		Resource: pod,
		Metadata: map[string]string{"container": container},
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS,
		},
		DetectedAt: time.Now(),
	}
}

// (a) Two containers of ONE pod with two different faults: the app OOMs, the
// sidecar is merely not Ready. Merging by shared pod folds the sidecar's fault into
// the OOM incident: it gets no incident, no proposal of its own, and nothing on the
// incident names it.
func TestRereview_A_TwoContainersOnePod_TheMilderFaultKeepsAnIncident(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.count.signals = []detection.Signal{
		e.signalFor(detection.SignalOOMKilled, detection.CategoryResource, detection.SeverityCritical, e.pod, "app"),
		e.signalFor(detection.SignalReadinessNotReady, detection.CategoryHealth, detection.SeverityWarning, e.pod, "sidecar"),
	}
	e.cycle()
	e.cycle()

	signals := incidentSignals(e.incidents())
	assert.Contains(t, signals, "health/ReadinessNotReady",
		"the sidecar's readiness fault has no incident; the incidents are %v", signals)
}

// (a, continued) The app recovers; the sidecar is still not Ready. The merged
// incident stays open for as long as the sidecar is not Ready, still labelled
// OOMKilled / critical, its root cause the OOM that is gone, and with no
// proposal for what is actually failing.
func TestRereview_A_AMergedIncidentKeepsTheDeadFaultsLabelWhileOnlyTheMilderOneRemains(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.count.signals = []detection.Signal{
		e.signalFor(detection.SignalOOMKilled, detection.CategoryResource, detection.SeverityCritical, e.pod, "app"),
		e.signalFor(detection.SignalReadinessNotReady, detection.CategoryHealth, detection.SeverityWarning, e.pod, "sidecar"),
	}
	e.cycle()

	// Only the sidecar fault remains, for longer than the resolution grace. The two
	// containers' faults are two incidents (re-review N-1), so the app's, whose
	// signal has gone, is the one aged here; the test as adopted called onlyOpen().
	e.count.signals = e.count.signals[1:]
	require.Len(t, e.open(), 2, "one incident per container: %v", incidentSignals(e.incidents()))
	for _, im := range e.open() {
		if im.Spec.Detection.Signal == "OOMKilled" {
			im.Spec.Detection.LastSeen = metav1.NewTime(time.Now().Add(-time.Hour))
			require.NoError(t, e.c.Update(context.Background(), &im))
		}
	}
	e.cycle()

	open := e.open()
	require.Len(t, open, 1)
	assert.Equal(t, "ReadinessNotReady", open[0].Spec.Detection.Signal,
		"the only live fault is the sidecar's readiness, but the open incident says %s / %s",
		open[0].Spec.Detection.Signal, open[0].Spec.Severity)
}

// (a) A fault on a rolled pod. The open incident names pod api-abc (crash loop).
// A rollout replaces it with api-def, which is first just not Ready. No pod in
// common, but the same container of the same application and the old pod's fault
// has ended, so it is the one fault: one incident. (Adopted as a characterization
// of two incidents, "PLAUSIBLE, low"; N-1 turns it into the intended behaviour.)
func TestRereview_A_ARolledPodMergesIntoTheFaultItReplaces(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setCrashLoop()
	e.cycle()
	require.Len(t, e.open(), 1)

	rolled := dorguv1.ResourceReference{Kind: "Pod", Name: "api-def", Namespace: recurrenceNS}
	e.count.signals = []detection.Signal{
		e.signalFor(detection.SignalReadinessNotReady, detection.CategoryHealth, detection.SeverityWarning, rolled, "api"),
	}
	e.cycle()

	assert.Len(t, e.open(), 1, "one crash-looping app, one incident: %v", incidentSignals(e.incidents()))
}

// (b) An advisory proposal made for the milder shape is Pending. The incident
// escalates to OOMKilled, whose proposal (the only auto-applicable one) is then
// deferred behind it, for as long as the advisory one waits (pendingTTL).
func TestRereview_B_AnOutstandingAdvisoryProposalDefersTheWorseShapesProposal(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	incident := e.onlyOpen()

	// What the AI planner leaves behind for a health incident: a Pending RA.
	ra := raFor("advisory-for-readiness", incident.Name, RemediationPhasePending)
	ra.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	ra.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	require.NoError(t, e.c.Create(context.Background(), ra))
	require.NoError(t, e.c.Status().Update(context.Background(), ra))

	e.setOOM()
	e.cycle()
	escalated := e.onlyOpen()
	require.Equal(t, "OOMKilled", escalated.Spec.Detection.Signal)

	var oomActions int
	for _, a := range e.actions() {
		if a.Name != ra.Name {
			oomActions++
		}
	}
	assert.NotZero(t, oomActions,
		"the incident is now OOMKilled / critical but its only proposal is the advisory one made for readiness; the OOM fix is not proposed")
}

// (d) Pruning trusts a cached Resolved phase and deletes on a UID precondition
// only. If the live incident was reopened after the cached list was read, the
// delete removes an open incident. Not reachable in the current code (the reopen
// targets the newest generation, which pruning keeps), so this is hardening: a
// ResourceVersion precondition or a live read makes it safe by construction.
func TestRereview_D_PruneDoesNotDeleteAnIncidentThatIsOpenLive_Hardening(t *testing.T) {
	stale := resolvedAt(pruneBase, "OOMKilled", 60*day)
	newest := resolvedAt(pruneBase+"-r1", "OOMKilled", 40*day)
	base := fake.NewClientBuilder().WithScheme(noiseScheme(t)).WithObjects(stale, newest).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}).Build()

	// The live object of the old incident is open again, last seen now.
	var live dorguv1.IncidentMemory
	require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: recurrenceNS, Name: pruneBase}, &live))
	snapshot := live.DeepCopy()
	live.Labels[LabelPhase] = PhaseDetected
	live.Spec.Detection.LastSeen = metav1.Now()
	require.NoError(t, base.Update(context.Background(), &live))
	live.Status.Phase = PhaseDetected
	require.NoError(t, base.Status().Update(context.Background(), &live))

	// The cache the pruner lists from still shows the Resolved snapshot.
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if l, ok := list.(*dorguv1.IncidentMemoryList); ok {
				l.Items = []dorguv1.IncidentMemory{*snapshot, *newest}
				return nil
			}
			return cl.List(ctx, list, opts...)
		},
	})
	r := newNoiseReconciler(c)
	r.ResolvedRetention = retention30

	require.NoError(t, r.pruneResolved(context.Background(), time.Now()))

	assert.True(t, exists(t, base, pruneBase), "an incident that is open on the API server was deleted on a stale cached read")
}

// (b) A human rejected the proposal made for the milder shape ten minutes ago. The
// incident then escalates to OOMKilled / critical. The cooldown lifts "when the live
// diagnosis is more severe than the incident the user declined", but the escalation
// has just raised the incident's own severity to the diagnosis's, so the comparison
// is equal and the no to a readiness plan suppresses the OOM proposal.
func TestRereview_B_ARejectionOfTheMilderShapeDoesNotSuppressTheWorseShape(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	incident := e.onlyOpen()

	ra := raFor("rejected-readiness", incident.Name, "Rejected")
	ra.CreationTimestamp = metav1.NewTime(time.Now().Add(-20 * time.Minute))
	ra.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	require.NoError(t, e.c.Create(context.Background(), ra))
	ra.Status.Phase = "Rejected"
	ra.Status.Conditions = []metav1.Condition{{
		Type: "Rejected", Status: metav1.ConditionTrue, Reason: "Rejected",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
	}}
	require.NoError(t, e.c.Status().Update(context.Background(), ra))

	e.setOOM()
	e.cycle()
	require.Equal(t, "OOMKilled", e.onlyOpen().Spec.Detection.Signal)

	var oomActions int
	for _, a := range e.actions() {
		if a.Name != ra.Name {
			oomActions++
		}
	}
	for _, c := range e.onlyOpen().Status.Conditions {
		t.Logf("incident condition %s=%s reason=%s: %s", c.Type, c.Status, c.Reason, c.Message)
	}
	assert.NotZero(t, oomActions, "the human declined a readiness plan; the fault is now an OOM kill and is not proposed for")
}

// (c) A fault that flaps AND changes shape each time it comes back. The cap counts
// recurrences per signal (the label resolvedPredecessors selects by), so each shape
// has its own budget and the fault as a whole is not capped at 3.
func TestRereview_C_AFlappingFaultThatChangesShapeIsStillCapped(t *testing.T) {
	e, _ := cappedEnv(t, 3)
	shapes := []func(){e.setReadiness, e.setLiveness, e.setCrashLoop}

	shapes[0]()
	e.cycle()
	for i := 1; i <= 30; i++ {
		e.resolve(e.onlyOpen().Name)
		shapes[i%len(shapes)]()
		e.cycle()
	}

	assert.LessOrEqual(t, len(e.incidents()), 1+3,
		"one fault flapping 30 times opened %d incidents (cap 3): %v", len(e.incidents()), incidentSignals(e.incidents()))
}

// (c) The proposals and Events a capped flapper produces: bounded, no storm.
func TestRereview_C_ACappedFlapperMakesNoEventOrProposalStorm_Characterization(t *testing.T) {
	e, rec := cappedEnv(t, 3)
	e.cycle()
	for range 40 {
		e.resolve(e.onlyOpen().Name)
		e.cycle()
	}
	events := drainEvents(rec)
	assert.LessOrEqual(t, len(events), 8, "events: %v", events)
	assert.LessOrEqual(t, len(e.actions()), 4, "one RemediationAction per incident at most")
}

// (a/F-1 follow-on) An incident that escalated Readiness -> CrashLoopBackOff keeps
// its readiness name. After it resolves, the fault comes back in the MILDER shape
// (readiness). Its lineage, and so every cooldown, must stay the same.
func TestRereview_A_ARecurrenceInAMilderShapeStaysInTheEscalatedIncidentsLineage(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	e.setCrashLoop()
	e.cycle()
	first := e.onlyOpen()
	require.Equal(t, "CrashLoopBackOff", first.Spec.Detection.Signal)
	e.resolve(first.Name)

	e.setReadiness()
	e.cycle()

	all := e.incidents()
	require.Len(t, all, 2, "%v", incidentSignals(all))
	for _, im := range all {
		assert.Equal(t, gateLineage(first.Name), gateLineage(im.Name),
			"incident %s (%s) is a different fault to the gate than %s", im.Name, im.Spec.Detection.Signal, first.Name)
	}
}

func gateLineage(name string) string { return gate.IncidentLineage(name) }

// (c) At the cap the reopened incident is proposed for through the same gate: a
// rejection ten minutes old still holds for the lineage, and no second proposal
// is made while one is outstanding.
func TestRereview_C_AReopenedIncidentStillHonoursTheRejectionCooldown(t *testing.T) {
	e, _ := cappedEnv(t, 1)
	e.cycle()
	e.resolve(e.onlyOpen().Name)
	e.cycle() // gen 1, the one recurrence the cap allows
	latest := e.onlyOpen()
	for _, a := range e.actions() { // forget the proposals so far
		require.NoError(t, e.c.Delete(context.Background(), &a))
	}
	ra := raFor("rejected-earlier", latest.Name, "Rejected")
	ra.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	require.NoError(t, e.c.Create(context.Background(), ra))
	ra.Status.Phase = "Rejected"
	ra.Status.Conditions = []metav1.Condition{{
		Type: "Rejected", Status: metav1.ConditionTrue, Reason: "Rejected",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
	}}
	require.NoError(t, e.c.Status().Update(context.Background(), ra))

	e.resolve(latest.Name)
	e.cycle() // capped: reopens latest

	reopened := e.onlyOpen()
	require.Equal(t, latest.Name, reopened.Name)
	require.NotEmpty(t, reopened.Annotations[AnnotationRecurrencesFolded], "this was the capped path")
	for _, a := range e.actions() {
		assert.Equal(t, ra.Name, a.Name, "a rejected fault was proposed for again inside the cooldown: %s", a.Name)
	}
}
