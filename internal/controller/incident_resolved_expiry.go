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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// A Pending proposal for an incident that has Resolved is stale (B10 a, D1.1 #1).
//
// The gate defers any new proposal while a proposal for the same incident lineage is
// in flight (DedupCooldown/active-remediation). A Pending one nobody approves stays in
// flight for the whole pending TTL, and it kept answering "wait" for faults that had
// already come and gone: in D1.1, 25 proposals were deferred behind four stale ones.
//
// So a Pending proposal whose incident has Resolved, by the application recovering by
// itself or by the health cycle seeing it healthy, is expired with the reason
// IncidentResolved. Like every Expired it is terminal, applies nothing, is not a
// rejection and starts no cooldown, so if the same fault comes back (a recurrence, the
// same lineage) the normal proposal path runs. The sweep runs at the start of a cycle,
// before anything is proposed, so a recurrence noticed in the same cycle is not
// deferred behind the proposal it replaces.
//
// Only Pending is expired. Approved, Ticketed, Applying and Verifying mean a human or
// the operator already acted on it and it is not the sweep's to withdraw.

// expireResolvedPending expires every Pending proposal whose incident is Resolved.
// It reads no signal: the phase of the incident is a recorded fact, so a partial
// detection does not matter.
func (r *HealthCheckReconciler) expireResolvedPending(ctx context.Context) error {
	var actions dorguv1.RemediationActionList
	if err := r.Client.List(ctx, &actions); err != nil {
		return fmt.Errorf("listing RemediationActions to expire: %w", err)
	}
	var errs []error
	for i := range actions.Items {
		ra := &actions.Items[i]
		if ra.Status.Phase != RemediationPhasePending || ra.Spec.IncidentRef.Name == "" {
			continue
		}
		resolved, err := r.incidentResolved(ctx, ra)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !resolved {
			continue
		}
		if err := r.expireForResolvedIncident(ctx, client.ObjectKeyFromObject(ra)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// incidentResolved reports whether the incident a proposal answers is Resolved. An
// incident that cannot be found (pruned) says nothing, so the proposal is left to the
// pending TTL.
func (r *HealthCheckReconciler) incidentResolved(ctx context.Context, ra *dorguv1.RemediationAction) (bool, error) {
	namespace := ra.Spec.IncidentRef.Namespace
	if namespace == "" {
		namespace = ra.Namespace
	}
	var incident dorguv1.IncidentMemory
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ra.Spec.IncidentRef.Name}, &incident); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return incident.Status.Phase == PhaseResolved, nil
}

// expireForResolvedIncident expires one proposal, re-reading it first: a human may have
// approved it since the list, and an approval that lands first wins.
func (r *HealthCheckReconciler) expireForResolvedIncident(ctx context.Context, key client.ObjectKey) error {
	var expired *dorguv1.RemediationAction
	err := retryIncidentWrite(func(int) error {
		var ra dorguv1.RemediationAction
		if err := r.Client.Get(ctx, key, &ra); err != nil {
			return err
		}
		if ra.Status.Phase != RemediationPhasePending {
			expired = nil
			return nil
		}
		ra.Status.Phase = RemediationPhaseExpired
		setCondition(&ra.Status.Conditions, ConditionExpired, metav1.ConditionTrue, ReasonIncidentResolved, fmt.Sprintf(
			"The incident this proposal answered (%s) resolved before anyone decided, so this advice is stale. "+
				"Nothing was applied. It is not a rejection and starts no cooldown; if the fault comes back it is proposed for again in the normal way.",
			ra.Spec.IncidentRef.Name))
		if err := r.Client.Status().Update(ctx, &ra); err != nil {
			return err
		}
		expired = &ra
		return nil
	})
	if err != nil {
		return fmt.Errorf("expiring %s for its resolved incident: %w", key, err)
	}
	if expired == nil {
		return nil
	}
	metrics.RecordTransition(RemediationPhasePending, RemediationPhaseExpired)
	emit(r.Recorder, expired, corev1.EventTypeNormal, EventReasonExpired, fmt.Sprintf(
		"expired: its incident %s resolved before anyone decided", expired.Spec.IncidentRef.Name))
	return nil
}
