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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// B9 (D0.8 finding 2): a recurrence after Resolved is a new incident.
//
// Incident names are a stable hash of namespace, persona and signal, so the
// OOM that came back after the first one resolved found no OPEN incident, tried
// to create the same name, hit AlreadyExists and was folded into the Resolved
// record: occurrenceCount went up, the phase stayed Resolved, no proposal was
// made and `dorgu incidents list` (which hides Resolved) showed nothing.

const (
	recurrenceNS      = "shop"
	recurrencePersona = "api"
)

type recurrenceEnv struct {
	t         *testing.T
	c         client.Client
	r         *HealthCheckReconciler
	pod       dorguv1.ResourceReference
	count     *stubCollector
	container string
}

func newRecurrenceEnv(t *testing.T, c client.Client) *recurrenceEnv {
	t.Helper()
	pod := dorguv1.ResourceReference{Kind: "Pod", Name: "api-abc", Namespace: recurrenceNS}
	ref := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	collector := &stubCollector{name: "stub", signals: []detection.Signal{{
		Type: detection.SignalOOMKilled, Severity: detection.SeverityCritical, Category: detection.CategoryResource,
		Source: "stub", Message: "container api was OOMKilled", Resource: pod, PersonaRef: ref, DetectedAt: time.Now(),
	}}}
	r := newNoiseReconciler(c, collector)
	r.Proposer = remediation.NewProposer(c, gate.New(c, noiseLogger()), noiseLogger())
	return &recurrenceEnv{t: t, c: c, r: r, pod: pod, count: collector}
}

func recurrencePersonaObject() *dorguv1.ApplicationPersona {
	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: recurrencePersona, Namespace: recurrenceNS},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: recurrencePersona,
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{Memory: "256Mi", CPU: "500m"},
			},
		},
	}
}

func (e *recurrenceEnv) cycle() { e.r.reconcile(context.Background()) }

func (e *recurrenceEnv) incidents() []dorguv1.IncidentMemory { return listIncidents(e.t, e.c) }

func (e *recurrenceEnv) open() []dorguv1.IncidentMemory {
	var out []dorguv1.IncidentMemory
	for _, im := range e.incidents() {
		if im.Status.Phase != PhaseResolved {
			out = append(out, im)
		}
	}
	return out
}

// onlyOpen returns the single open incident, failing the test when there is not
// exactly one.
func (e *recurrenceEnv) onlyOpen() dorguv1.IncidentMemory {
	e.t.Helper()
	open := e.open()
	require.Len(e.t, open, 1, "expected exactly one open incident, have %s", incidentSignals(e.incidents()))
	return open[0]
}

func (e *recurrenceEnv) actions() []dorguv1.RemediationAction {
	var list dorguv1.RemediationActionList
	require.NoError(e.t, e.c.List(context.Background(), &list))
	return list.Items
}

// resolve marks an incident Resolved the way markResolved leaves it.
func (e *recurrenceEnv) resolve(name string) {
	e.t.Helper()
	var im dorguv1.IncidentMemory
	key := client.ObjectKey{Namespace: recurrenceNS, Name: name}
	require.NoError(e.t, e.c.Get(context.Background(), key, &im))
	im.Labels[LabelPhase] = PhaseResolved
	im.Spec.Resolution = &dorguv1.ResolutionInfo{Action: "none", Outcome: "resolved"}
	require.NoError(e.t, e.c.Update(context.Background(), &im))
	require.NoError(e.t, e.c.Get(context.Background(), key, &im))
	im.Status.Phase = PhaseResolved
	require.NoError(e.t, e.c.Status().Update(context.Background(), &im))
}

func (e *recurrenceEnv) get(name string) dorguv1.IncidentMemory {
	e.t.Helper()
	var im dorguv1.IncidentMemory
	require.NoError(e.t, e.c.Get(context.Background(), client.ObjectKey{Namespace: recurrenceNS, Name: name}, &im))
	return im
}

// recurrenceCount reads dorgu_incident_recurrences_total for the resource class
// and persona scope the fixtures raise, or 0 if the series does not exist.
func recurrenceCount(t *testing.T) float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	require.NoError(t, err)
	want := map[string]string{"class": "resource", "scope": "persona"}
	for _, f := range families {
		if f.GetName() != "dorgu_incident_recurrences_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatch(m.GetLabel(), want) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestRecurrence_AfterResolvedOpensANewIncident(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))

	e.cycle()
	require.Len(t, e.incidents(), 1)
	first := e.incidents()[0]
	e.resolve(first.Name)
	resolvedBefore := e.get(first.Name)

	e.cycle()

	all := e.incidents()
	require.Len(t, all, 2, "the recurrence must be its own incident, got %s", incidentSignals(all))
	open := e.open()
	require.Len(t, open, 1, "exactly one open incident: the recurrence")
	recurrence := open[0]
	assert.NotEqual(t, first.Name, recurrence.Name)
	assert.Equal(t, PhaseDetected, recurrence.Status.Phase)
	assert.EqualValues(t, 1, recurrence.Status.OccurrenceCount, "its own count, not the old one's")
	assert.False(t, recurrence.Spec.Detection.FirstSeen.Before(&resolvedBefore.Spec.Detection.LastSeen),
		"its own firstSeen, which is what time-to-detect and detect-to-proposal are measured from")

	// The Resolved record is history and stays exactly as it was.
	after := e.get(first.Name)
	assert.Equal(t, PhaseResolved, after.Status.Phase)
	assert.Equal(t, resolvedBefore.Status.OccurrenceCount, after.Status.OccurrenceCount,
		"a recurrence must never be folded into the Resolved incident")
	assert.Equal(t, PhaseResolved, after.Labels[LabelPhase])
}

func TestRecurrence_IsVisible(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.cycle()
	first := e.onlyOpen()
	e.resolve(first.Name)

	before := recurrenceCount(t)
	e.cycle()

	recurrence := e.onlyOpen()
	assert.Equal(t, first.Name, recurrence.Annotations[AnnotationRecurrenceOf], "links to the incident it recurs")
	assert.Equal(t, "1", recurrence.Annotations[AnnotationRecurrenceCount])
	assert.Equal(t, 1.0, recurrenceCount(t)-before)
	// An incident that is not a recurrence carries neither.
	assert.Empty(t, first.Annotations[AnnotationRecurrenceOf])
}

func TestRecurrence_ChainsAcrossGenerations(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))

	e.cycle()
	g0 := e.onlyOpen()
	e.resolve(g0.Name)
	e.cycle()
	g1 := e.onlyOpen()
	e.resolve(g1.Name)
	e.cycle()
	g2 := e.onlyOpen()

	require.Len(t, e.incidents(), 3)
	assert.NotEqual(t, g1.Name, g2.Name)
	assert.NotEqual(t, g0.Name, g2.Name)
	assert.Equal(t, g1.Name, g2.Annotations[AnnotationRecurrenceOf], "links to the latest earlier incident")
	assert.Equal(t, "2", g2.Annotations[AnnotationRecurrenceCount])
	assert.Equal(t, gate.IncidentLineage(g0.Name), gate.IncidentLineage(g2.Name),
		"every generation of one fault shares a lineage, which is what the rejection cooldown keys on")
}

func TestRecurrence_OpenIncidentStillFoldsRepeatSightings(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	before := recurrenceCount(t)

	e.cycle()
	e.cycle()
	e.cycle()

	require.Len(t, e.incidents(), 1, "the same fault seen again while open is the same incident")
	assert.EqualValues(t, 3, e.incidents()[0].Status.OccurrenceCount)
	assert.Equal(t, before, recurrenceCount(t))
}

func TestRecurrence_StaleCacheDoesNotFoldIntoAResolvedIncident(t *testing.T) {
	// The manager's cache can lag: the List that looks for a Resolved predecessor
	// misses it, the create of the base name hits AlreadyExists, and the adopt path
	// used to fold into the Resolved object. It must open a new generation instead.
	base := fake.NewClientBuilder().WithScheme(noiseScheme(t)).WithObjects(recurrencePersonaObject()).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}).Build()
	hideResolved := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := inner.List(ctx, list, opts...); err != nil {
				return err
			}
			if im, ok := list.(*dorguv1.IncidentMemoryList); ok && hideResolved {
				kept := im.Items[:0]
				for _, item := range im.Items {
					if item.Status.Phase != PhaseResolved {
						kept = append(kept, item)
					}
				}
				im.Items = kept
			}
			return nil
		},
	})
	e := newRecurrenceEnv(t, c)
	e.cycle()
	first := e.onlyOpen()
	e.resolve(first.Name)

	hideResolved = true
	e.cycle()
	hideResolved = false

	require.Len(t, e.incidents(), 2, "got %s", incidentSignals(e.incidents()))
	assert.Equal(t, PhaseResolved, e.get(first.Name).Status.Phase)
	assert.EqualValues(t, 1, e.get(first.Name).Status.OccurrenceCount)
	require.Len(t, e.open(), 1)
}

func TestRecurrence_UnattributedKeepsItsOwnLineage(t *testing.T) {
	// No persona: the incident is filed against the workload. It recurs the same way.
	c := noiseClient(t)
	collector := &stubCollector{name: "stub", signals: []detection.Signal{{
		Type: detection.SignalOOMKilled, Severity: detection.SeverityCritical, Category: detection.CategoryResource,
		Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "worker-abc", Namespace: recurrenceNS},
		Metadata: map[string]string{detection.MetadataKeyDeployment: "worker"}, DetectedAt: time.Now(),
	}}}
	r := newNoiseReconciler(c, collector)
	r.reconcile(context.Background())
	first := listIncidents(t, c)
	require.Len(t, first, 1)
	e := &recurrenceEnv{t: t, c: c, r: r}
	e.resolve(first[0].Name)

	r.reconcile(context.Background())

	require.Len(t, e.incidents(), 2)
	open := e.open()
	require.Len(t, open, 1)
	assert.Equal(t, AttributionUnattributed, open[0].Spec.Attribution)
	assert.Equal(t, first[0].Name, open[0].Annotations[AnnotationRecurrenceOf])
}

func TestRecurrence_PreviousOccurrencesTellsThePromptTheHistory(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.cycle()
	e.cycle()
	first := e.onlyOpen()
	require.EqualValues(t, 2, first.Status.OccurrenceCount)
	e.resolve(first.Name)

	diag := aiDiagnosis()
	diag.PersonaRef = &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: recurrencePersona, Namespace: recurrenceNS}
	diag.Category = "resource"
	subject := personaSubject(*diag.PersonaRef)

	got := e.r.previousOccurrences(subject)(context.Background(), *diag)
	assert.EqualValues(t, 2, got, "a recurrence is not a first sighting: the model is told the earlier count")
}

func TestRecurrence_RecurringConditionNamesThePredecessor(t *testing.T) {
	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name: "im-x-r1", Annotations: map[string]string{AnnotationRecurrenceOf: "im-x", AnnotationRecurrenceCount: "1"},
		},
		Status: dorguv1.IncidentMemoryStatus{Phase: PhaseDetected, OccurrenceCount: 1},
	}
	(&IncidentController{}).updateConditions(im)

	var cond *metav1.Condition
	for i := range im.Status.Conditions {
		if im.Status.Conditions[i].Type == ConditionRecurring {
			cond = &im.Status.Conditions[i]
		}
	}
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a recurrence is Recurring even on its first occurrence")
	assert.Equal(t, "RecurrenceOfResolvedIncident", cond.Reason)
	assert.Contains(t, cond.Message, "im-x")
}

// ---- The recurrence gets a proposal through the normal gate ----------------

func TestRecurrence_GetsANewProposalThroughTheGate(t *testing.T) {
	const hour = time.Hour
	tests := []struct {
		name string
		// prior is what the earlier incident's RemediationAction looks like when the
		// fault comes back.
		prior       func(ra *dorguv1.RemediationAction)
		wantActions int
		wantNew     bool
	}{
		{"prior fix completed, did not hold", func(ra *dorguv1.RemediationAction) {
			ra.Status.Phase = RemediationPhaseCompleted
		}, 2, true},
		{"prior proposal expired unanswered", func(ra *dorguv1.RemediationAction) {
			ra.Status.Phase = "Expired"
		}, 2, true},
		{"prior proposal still pending: outstanding, not duplicated", func(ra *dorguv1.RemediationAction) {
			ra.Status.Phase = RemediationPhasePending
		}, 1, false},
		{"prior proposal rejected 10 minutes ago: rejection cooldown holds", func(ra *dorguv1.RemediationAction) {
			ra.Status.Phase = RemediationPhaseRejected
			ra.Status.Conditions = []metav1.Condition{{
				Type: ConditionRejected, Status: metav1.ConditionTrue, Reason: ReasonUserRejected,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
			}}
		}, 1, false},
		{"prior proposal rejected 2 hours ago: cooldown over", func(ra *dorguv1.RemediationAction) {
			ra.Status.Phase = RemediationPhaseRejected
			ra.Status.Conditions = []metav1.Condition{{
				Type: ConditionRejected, Status: metav1.ConditionTrue, Reason: ReasonUserRejected,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * hour)),
			}}
		}, 2, true},
		{"prior fix failed 10 minutes ago: failed-cooldown holds", func(ra *dorguv1.RemediationAction) {
			ra.Status.Phase = RemediationPhaseFailed
			applied := metav1.NewTime(time.Now().Add(-10 * time.Minute))
			ra.Status.AppliedAt = &applied
		}, 1, false},
		{"prior fix failed 2 hours ago: cooldown over", func(ra *dorguv1.RemediationAction) {
			ra.Status.Phase = RemediationPhaseFailed
			applied := metav1.NewTime(time.Now().Add(-2 * hour))
			ra.Status.AppliedAt = &applied
		}, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
			e.cycle()
			require.Len(t, e.actions(), 1, "the first incident gets its proposal")
			first := e.onlyOpen()

			ra := e.actions()[0]
			tt.prior(&ra)
			require.NoError(t, e.c.Status().Update(context.Background(), &ra))
			e.resolve(first.Name)

			e.cycle()

			require.Len(t, e.open(), 1, "the recurrence is always a visible incident")
			actions := e.actions()
			assert.Len(t, actions, tt.wantActions, "RemediationActions after the recurrence")
			if tt.wantNew {
				recurrence := e.onlyOpen()
				found := false
				for _, a := range actions {
					found = found || a.Spec.IncidentRef.Name == recurrence.Name
				}
				assert.True(t, found, "a RemediationAction must name the recurrence incident %s", recurrence.Name)
			}
		})
	}
}

// B9 review F-2: the rate limit, with the creation timestamps a real API server
// sets (the fake client leaves them zero, so an hourly limit counted nothing in
// the table above).
func TestRecurrence_RateLimitWithRealCreationTimestamps(t *testing.T) {
	proposalsDeferred := func() float64 {
		return metricValue(t, "dorgu_proposals_total", map[string]string{
			"source": "dorgu", "action_type": "resource-adjustment", "result": "deferred",
		})
	}
	refused := func(im dorguv1.IncidentMemory) *metav1.Condition {
		for i := range im.Status.Conditions {
			if im.Status.Conditions[i].Type == dorguv1.ConditionProposalRefused && im.Status.Conditions[i].Status == metav1.ConditionTrue {
				return &im.Status.Conditions[i]
			}
		}
		return nil
	}

	t.Run("an outstanding proposal of the same fault defers, it is not another remediation", func(t *testing.T) {
		e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject(), limitOneClusterPersona()))
		e.cycle()
		e.resolve(e.onlyOpen().Name)
		stampCreationNow(t, e)
		before := proposalsDeferred()

		e.cycle()

		assert.Len(t, e.actions(), 1)
		assert.Nil(t, refused(e.onlyOpen()), "a deferral records no refusal")
		assert.Greater(t, proposalsDeferred(), before, "it is counted as a deferral")
	})

	t.Run("a finished proposal inside the hour still counts: propose, fail, propose is stopped", func(t *testing.T) {
		e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject(), limitOneClusterPersona()))
		e.cycle()
		e.resolve(e.onlyOpen().Name)
		stampCreationNow(t, e)
		for _, ra := range e.actions() {
			ra.Status.Phase = RemediationPhaseCompleted
			require.NoError(t, e.c.Status().Update(context.Background(), &ra))
		}

		e.cycle()

		assert.Len(t, e.actions(), 1, "the hourly limit of 1 is spent")
		cond := refused(e.onlyOpen())
		require.NotNil(t, cond, "this one IS a refusal")
		assert.Equal(t, "Guardrails_RateLimit", cond.Reason)
	})
}
