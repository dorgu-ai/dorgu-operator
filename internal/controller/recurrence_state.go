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
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// A recurrence after Resolved is a new IncidentMemory (B9, D0.8 finding 2).
//
// The choice was between a new object and an "occurrence" record inside the old
// one. A new object, because everything downstream keys on the incident object:
//
//   - time-to-detect and detect-to-proposal (KPI Q1) are measured from the
//     incident's own firstSeen, and "first proposal per incident" is one per
//     record. A recurrence folded into history has neither, and its detection
//     latency would be silently lost.
//   - the proposal is answered per incident (gate dedup, RemediationAction
//     incidentRef), `dorgu incidents list` hides Resolved ones, and the report's
//     precision counts decisions per incident.
//   - the CRD has no list of occurrences, and adding one is a shape change.
//
// What a new object costs is two things, both handled. History: the records of
// one fault are tied together by the annotations below and by a shared name
// lineage (gate.IncidentLineage). Cooldowns: the gate's rejection cooldown and
// "active remediation" dedup compare lineages, not names, so a human's no, or a
// proposal already outstanding, still covers the fault when it comes back. The
// failed-cooldown is per persona and needs nothing.

// isResolvedIncident reports whether an incident is closed. It reads status.phase,
// which only the operator writes, and not the dorgu.io/phase label, which anyone
// who can edit an incident can set: findMatchingIncident reads the status, and two
// readings of "Resolved" would disagree exactly where they are written one after
// the other (markResolved sets the label first, the status second). The label is
// only an index for listing.
func isResolvedIncident(im *dorguv1.IncidentMemory) bool {
	return im.Status.Phase == PhaseResolved
}

// resolvedPredecessors lists the Resolved incidents of the same fault as a diagnosis
// about a subject.
//
// A fault is the application, the container and the family, so a signal in a family
// (see incident_family.go and detection.FamilyOf) looks at every Resolved incident of
// the application in that family that is about the same container, whatever shape it
// was in: the recurrence cap, the generation number and the lineage are per fault, not
// per signal, or a fault that flapped and changed shape each time would have a budget
// per shape. CPU throttling is a family of one signal, so it gets the container
// identity and nothing else. Any signal in no family keeps looking for its own: same
// persona, category, signal and attribution.
func (r *HealthCheckReconciler) resolvedPredecessors(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
) ([]dorguv1.IncidentMemory, error) {
	signal := primarySignalType(diag)
	family, inFamily := detection.FamilyOf(signal)

	var candidates []dorguv1.IncidentMemory
	if inFamily {
		all, err := r.applicationIncidents(ctx, subject)
		if err != nil {
			return nil, err
		}
		containers := containersOf(diag)
		for i := range all {
			if f, in := detection.FamilyOf(detection.SignalType(all[i].Spec.Detection.Signal)); in && f == family &&
				containersOverlap(incidentContainers(&all[i]), containers) {
				candidates = append(candidates, all[i])
			}
		}
	} else {
		opts := []client.ListOption{client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{
			LabelPersonaKind: subject.personaRef.Kind,
			LabelPersonaName: subject.personaRef.Name,
			LabelCategory:    diag.Category,
			LabelSignal:      string(signal),
		})}}
		if subject.namespace != "" {
			opts = append(opts, client.InNamespace(subject.namespace))
		}
		var list dorguv1.IncidentMemoryList
		if err := r.Client.List(ctx, &list, opts...); err != nil {
			return nil, fmt.Errorf("listing earlier IncidentMemories: %w", err)
		}
		candidates = list.Items
	}

	var resolved []dorguv1.IncidentMemory
	for i := range candidates {
		if im := &candidates[i]; isResolvedIncident(im) && subject.matchesIncident(im) {
			resolved = append(resolved, *im)
		}
	}
	return resolved, nil
}

// latestResolved returns the most recent earlier incident of the fault, the one
// a recurrence links to: the highest generation, the latest firstSeen on a tie.
func latestResolved(resolved []dorguv1.IncidentMemory) *dorguv1.IncidentMemory {
	var latest *dorguv1.IncidentMemory
	for i := range resolved {
		im := &resolved[i]
		if latest == nil {
			latest = im
			continue
		}
		gi, gl := gate.IncidentGeneration(im.Name), gate.IncidentGeneration(latest.Name)
		if gi > gl || (gi == gl && im.Spec.Detection.FirstSeen.After(latest.Spec.Detection.FirstSeen.Time)) {
			latest = im
		}
	}
	return latest
}

// nextGeneration says which generation of the fault a new incident is: 0 when no
// earlier incident of it exists, otherwise one past the latest, with the incident
// it recurs and the earlier incidents it was worked out from.
func (r *HealthCheckReconciler) nextGeneration(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
) (int, *dorguv1.IncidentMemory, []dorguv1.IncidentMemory, error) {
	resolved, err := r.resolvedPredecessors(ctx, subject, diag)
	if err != nil {
		return 0, nil, nil, err
	}
	previous := latestResolved(resolved)
	if previous == nil {
		return 0, nil, nil, nil
	}
	return gate.IncidentGeneration(previous.Name) + 1, previous.DeepCopy(), resolved, nil
}

// recurrenceCapReached reports whether the fault has already opened as many
// recurrences inside the window as the cap allows. A recurrence is dated by its
// own firstSeen. Zero cap disables the cap; a cap with no window uses the default.
func (r *HealthCheckReconciler) recurrenceCapReached(resolved []dorguv1.IncidentMemory, now time.Time) bool {
	if r.RecurrenceCap <= 0 {
		return false
	}
	window := r.RecurrenceWindow
	if window <= 0 {
		window = DefaultRecurrenceWindow
	}
	cutoff := now.Add(-window)
	recent := 0
	for i := range resolved {
		im := &resolved[i]
		if gate.IncidentGeneration(im.Name) >= 1 && !im.Spec.Detection.FirstSeen.Time.Before(cutoff) {
			recent++
		}
	}
	return recent >= r.RecurrenceCap
}

// reopenLatest is what a recurrence does once the cap is reached: it reopens the
// latest incident of the fault instead of creating another. It is open again
// (never a silent fold into a Resolved record), its sighting is counted and its
// lastSeen refreshed, it carries the number of recurrences folded into it and the
// Flapping condition, and the first fold raises one Warning Event.
func (r *HealthCheckReconciler) reopenLatest(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	latest *dorguv1.IncidentMemory,
	now metav1.Time,
) error {
	folded := 0
	err := r.writeIncident(ctx, latest, diag, now, incidentEdit{
		spec: func(fresh *dorguv1.IncidentMemory) {
			// It takes the shape the fault has now, whatever it was when it resolved.
			reshapeSpec(fresh, subject, diag, now, true)
			if fresh.Annotations == nil {
				fresh.Annotations = map[string]string{}
			}
			if res := fresh.Spec.Resolution; res != nil {
				// spec.resolution is the record of an incident that closed; an open
				// one carries none. Keep what it said, so the fix that did not hold is
				// not lost.
				fresh.Annotations[AnnotationLastResolution] = fmt.Sprintf("%s: %s", res.Outcome, res.Action)
			}
			fresh.Spec.Resolution = nil
			fresh.Labels[LabelPhase] = PhaseDetected
			folded = foldedCount(fresh) + 1
			fresh.Annotations[AnnotationRecurrencesFolded] = strconv.Itoa(folded)
		},
		status: func(fresh *dorguv1.IncidentMemory) { fresh.Status.Phase = PhaseDetected },
	})
	if err != nil {
		return err
	}

	metrics.RecordIncidentRecurrenceCapped(diag.Category, subject.attribution())
	r.Logger.Info("recurrence cap reached: reopened the latest incident instead of opening another",
		"incident", latest.Name, "folded", folded, "cap", r.RecurrenceCap)
	if folded == 1 {
		emit(r.Recorder, latest, corev1.EventTypeWarning, EventReasonIncidentFlapping, fmt.Sprintf(
			"this fault has recurred %d times within %s; further recurrences reopen this incident instead of opening a new one",
			r.RecurrenceCap, r.recurrenceWindow()))
	}
	return nil
}

func (r *HealthCheckReconciler) recurrenceWindow() time.Duration {
	if r.RecurrenceWindow <= 0 {
		return DefaultRecurrenceWindow
	}
	return r.RecurrenceWindow
}

// foldedCount reads how many recurrences were folded into an incident.
func foldedCount(im *dorguv1.IncidentMemory) int {
	n, err := strconv.Atoi(im.Annotations[AnnotationRecurrencesFolded])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
