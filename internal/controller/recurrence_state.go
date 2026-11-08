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

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
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

// isResolvedIncident reports whether an incident is closed.
func isResolvedIncident(im *dorguv1.IncidentMemory) bool {
	return im.Status.Phase == PhaseResolved || im.Labels[LabelPhase] == PhaseResolved
}

// resolvedPredecessors lists the Resolved incidents of the same fault as a
// diagnosis about a subject: same persona, category, signal and attribution.
func (r *HealthCheckReconciler) resolvedPredecessors(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
) ([]dorguv1.IncidentMemory, error) {
	opts := []client.ListOption{client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{
		LabelPersonaKind: subject.personaRef.Kind,
		LabelPersonaName: subject.personaRef.Name,
		LabelCategory:    diag.Category,
		LabelSignal:      string(primarySignalType(diag)),
	})}}
	if subject.namespace != "" {
		opts = append(opts, client.InNamespace(subject.namespace))
	}

	var list dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing earlier IncidentMemories: %w", err)
	}

	var resolved []dorguv1.IncidentMemory
	for i := range list.Items {
		if im := &list.Items[i]; isResolvedIncident(im) && subject.matchesIncident(im) {
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
// it recurs.
func (r *HealthCheckReconciler) nextGeneration(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
) (int, *dorguv1.IncidentMemory, error) {
	resolved, err := r.resolvedPredecessors(ctx, subject, diag)
	if err != nil {
		return 0, nil, err
	}
	previous := latestResolved(resolved)
	if previous == nil {
		return 0, nil, nil
	}
	return gate.IncidentGeneration(previous.Name) + 1, previous.DeepCopy(), nil
}
