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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// One bad image is one incident and one proposal (EKS QA 2026-11-09 F-1).
//
// A pod whose image cannot be pulled stays Pending, so it also crossed the
// long-pending threshold and opened a second incident with a second proposal. The
// pod collector no longer raises PodPendingLong for a pod with an image error, and
// this is the rest of it, for the two signals meeting anyway in either order:
//
//   - image error first, then PodPendingLong for the same pod (a cycle in which the
//     image error was momentarily not visible): the PodPendingLong diagnosis is
//     folded into the image-error incident, which stays active, and gets nothing of
//     its own;
//   - PodPendingLong first (the pod was Pending for another reason), then the image
//     error: the incident for the pending pod keeps its record, but the proposal
//     made for it is advice for a fault that is now an image error, so a Pending one
//     is superseded (the same Expired/Superseded the escalation of a worsening fault
//     uses) before the image error is proposed for.
//
// This is not part of the fault progression (see detection.FaultRank): an image
// error is not a rung of the container's own ladder, and merging the incidents would
// make one incident change its signal and category under a person reading it.

// imageErrorCovering returns the open image-error incident that already explains
// this PodPendingLong diagnosis, or nil: one of the same application that names a
// pod in common.
func (r *HealthCheckReconciler) imageErrorCovering(
	ctx context.Context, subject incidentSubject, diag *diagnosis.Diagnosis,
) (*dorguv1.IncidentMemory, error) {
	if primarySignalType(diag) != detection.SignalPodPendingLong {
		return nil, nil
	}
	incidents, err := r.applicationIncidents(ctx, subject)
	if err != nil {
		return nil, err
	}
	for i := range incidents {
		im := &incidents[i]
		if im.Spec.Detection.Signal == string(detection.SignalImagePullBackOff) && !isResolvedIncident(im) &&
			subject.matchesIncident(im) && sharesPod(im.Spec.Detection.AffectedResources, diag.AffectedResources) {
			return im, nil
		}
	}
	return nil, nil
}

// foldIntoImageError records a PodPendingLong sighting against the incident of the
// image error that explains it: counted, lastSeen refreshed, nothing else changes.
func (r *HealthCheckReconciler) foldIntoImageError(
	ctx context.Context, diag *diagnosis.Diagnosis, im *dorguv1.IncidentMemory, activeSignalKeys map[string]bool, now metav1.Time,
) error {
	if err := r.writeIncident(ctx, im, diag, now, incidentEdit{
		spec: func(fresh *dorguv1.IncidentMemory) { fresh.Spec.Detection.LastSeen = now },
	}); err != nil {
		return err
	}
	activeSignalKeys[incidentSignalKey(im)] = true
	return nil
}

// supersedePendingLongProposals expires the Pending proposals of the application's
// open PodPendingLong incidents about a pod this image-error diagnosis names. It
// does nothing for any other diagnosis. A failure only leaves a stale proposal
// beside the new one until it is superseded on a later cycle or expires.
func (r *HealthCheckReconciler) supersedePendingLongProposals(
	ctx context.Context, subject incidentSubject, diag *diagnosis.Diagnosis,
) {
	if primarySignalType(diag) != detection.SignalImagePullBackOff {
		return
	}
	if err := r.expirePendingLong(ctx, subject, diag); err != nil {
		logFailure(r.Logger, err, "could not supersede the proposal for the pending pod", "persona", subject.personaRef.Name)
	}
}

func (r *HealthCheckReconciler) expirePendingLong(ctx context.Context, subject incidentSubject, diag *diagnosis.Diagnosis) error {
	incidents, err := r.applicationIncidents(ctx, subject)
	if err != nil {
		return err
	}
	var actions dorguv1.RemediationActionList
	if err := r.Client.List(ctx, &actions, client.InNamespace(subject.namespace)); err != nil {
		return fmt.Errorf("listing RemediationActions to supersede: %w", err)
	}
	var errs []error
	for i := range incidents {
		im := &incidents[i]
		if im.Spec.Detection.Signal != string(detection.SignalPodPendingLong) || isResolvedIncident(im) ||
			!subject.matchesIncident(im) || !sharesPod(im.Spec.Detection.AffectedResources, diag.AffectedResources) {
			continue
		}
		for j := range actions.Items {
			ra := &actions.Items[j]
			if ra.Status.Phase != RemediationPhasePending || !gate.SameIncidentLineage(ra.Spec.IncidentRef.Name, im.Name) {
				continue
			}
			if err := r.supersede(ctx, client.ObjectKeyFromObject(ra),
				string(detection.SignalPodPendingLong), string(detection.SignalImagePullBackOff)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
