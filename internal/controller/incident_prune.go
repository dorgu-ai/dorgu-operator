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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// Pruning Resolved incidents (B9 review F-3).
//
// B9 turned a recurrence from a silent fold into a new record, which without a
// bound is a record per recurrence kept forever. The cap bounds the rate; this
// bounds the total: a Resolved incident whose last sighting is older than
// ResolvedRetention is deleted, with three exceptions.
//
//   - The newest Resolved incident of every fault (its lineage) is kept however
//     old. That is what keeps a lineage reconstructable: a recurrence numbers on
//     from it and links to it, and the cooldowns, which compare lineages, still
//     find it. What pruning does lose is the older generations' own records (their
//     root cause text and timings); the newest record keeps its recurrence count
//     and a recurrence-of annotation that may then name a deleted incident.
//   - An incident a non-terminal RemediationAction names is kept: that proposal is
//     still waiting on a human and its incidentRef must resolve.
//   - An incident is only ever deleted on its status, which only the operator
//     writes. The dorgu.io/phase label is how Resolved incidents are listed, and a
//     label anyone can set must not be what authorises a delete.

// activeRemediationPhases are the RemediationAction phases that are not terminal.
// The empty phase is a just-created action whose status has not landed yet.
var activeRemediationPhases = []string{
	"", RemediationPhasePending, RemediationPhaseApproved, RemediationPhaseTicketed,
	RemediationPhaseApplying, RemediationPhaseVerifying,
}

// pruneResolved deletes the Resolved incidents past the retention. A failure to
// delete one is counted and returned after the rest have been tried; a failure to
// read the RemediationActions deletes nothing, since a reference could be missed.
func (r *HealthCheckReconciler) pruneResolved(ctx context.Context, now time.Time) error {
	if r.ResolvedRetention <= 0 {
		return nil
	}

	referenced, err := r.incidentsNamedByActiveRemediations(ctx)
	if err != nil {
		return err
	}

	var list dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &list, client.MatchingLabels{LabelPhase: PhaseResolved}); err != nil {
		return fmt.Errorf("listing Resolved IncidentMemories: %w", err)
	}

	var resolved []dorguv1.IncidentMemory
	for i := range list.Items {
		if isResolvedIncident(&list.Items[i]) {
			resolved = append(resolved, list.Items[i])
		}
	}
	newest := newestPerLineage(resolved)

	cutoff := now.Add(-r.ResolvedRetention)
	var errs []error
	for i := range resolved {
		im := &resolved[i]
		if newest[incidentKey(im)] || !lastSighting(im).Before(cutoff) {
			continue
		}
		if referenced[incidentKey(im)] {
			metrics.RecordIncidentPrune(metrics.PruneKeptReferenced)
			continue
		}
		uid := im.UID
		err := r.Client.Delete(ctx, im, client.Preconditions{UID: &uid})
		switch {
		case err == nil || apierrors.IsNotFound(err):
			metrics.RecordIncidentPrune(metrics.PruneDeleted)
			r.Logger.V(1).Info("pruned a Resolved incident past the retention",
				"incident", im.Name, "lastSeen", lastSighting(im), "retention", r.ResolvedRetention)
		default:
			metrics.RecordIncidentPrune(metrics.PruneError)
			errs = append(errs, fmt.Errorf("deleting IncidentMemory %s/%s: %w", im.Namespace, im.Name, err))
		}
	}
	return errors.Join(errs...)
}

// incidentsNamedByActiveRemediations returns the incidents that a RemediationAction
// which is not yet terminal names.
func (r *HealthCheckReconciler) incidentsNamedByActiveRemediations(ctx context.Context) (map[string]bool, error) {
	var actions dorguv1.RemediationActionList
	if err := r.Client.List(ctx, &actions); err != nil {
		return nil, fmt.Errorf("listing RemediationActions to see which incidents are still referenced: %w", err)
	}
	referenced := make(map[string]bool)
	for i := range actions.Items {
		ra := &actions.Items[i]
		if !isActiveRemediationPhase(ra.Status.Phase) {
			continue
		}
		namespace := ra.Spec.IncidentRef.Namespace
		if namespace == "" {
			namespace = ra.Namespace
		}
		referenced[namespace+"/"+ra.Spec.IncidentRef.Name] = true
	}
	return referenced, nil
}

func isActiveRemediationPhase(phase string) bool {
	for _, p := range activeRemediationPhases {
		if p == phase {
			return true
		}
	}
	return false
}

// newestPerLineage returns, for every fault (namespace and lineage), the newest of
// its incidents: the highest generation, the latest firstSeen on a tie.
func newestPerLineage(incidents []dorguv1.IncidentMemory) map[string]bool {
	groups := make(map[string][]dorguv1.IncidentMemory)
	for i := range incidents {
		key := incidents[i].Namespace + "/" + gate.IncidentLineage(incidents[i].Name)
		groups[key] = append(groups[key], incidents[i])
	}
	newest := make(map[string]bool, len(groups))
	for _, group := range groups {
		if latest := latestResolved(group); latest != nil {
			newest[incidentKey(latest)] = true
		}
	}
	return newest
}

func incidentKey(im *dorguv1.IncidentMemory) string { return im.Namespace + "/" + im.Name }

// lastSighting is when the incident's signal was last seen, which is when its age
// is measured from.
func lastSighting(im *dorguv1.IncidentMemory) time.Time {
	if t := im.Spec.Detection.LastSeen; !t.IsZero() {
		return t.Time
	}
	return im.CreationTimestamp.Time
}
