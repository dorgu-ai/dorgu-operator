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
	"reflect"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Incident condition types.
const (
	ConditionDetected  = "Detected"
	ConditionResolved  = "Resolved"
	ConditionRecurring = "Recurring"
)

// IncidentController watches IncidentMemory CRDs and manages their lifecycle:
// label maintenance, status conditions, and ApplicationPersona status sync.
type IncidentController struct {
	Client client.Client
	Logger logr.Logger
}

// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas/status,verbs=get;update;patch

// Reconcile ensures IncidentMemory labels, conditions, and persona sync are correct.
func (r *IncidentController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Logger.WithValues("incidentmemory", req.NamespacedName)

	// 1. Fetch IncidentMemory.
	var im dorguv1.IncidentMemory
	if err := r.Client.Get(ctx, req.NamespacedName, &im); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Ensure labels are set for query efficiency.
	labelsUpdated := r.ensureLabels(&im)
	if labelsUpdated {
		if err := r.Client.Update(ctx, &im); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating labels: %w", err)
		}
		logger.V(1).Info("updated incident labels")
	}

	// 3. Update conditions (only if changed to avoid update storm).
	conditionsBefore := make([]metav1.Condition, len(im.Status.Conditions))
	copy(conditionsBefore, im.Status.Conditions)

	r.updateConditions(&im)

	if !reflect.DeepEqual(conditionsBefore, im.Status.Conditions) {
		// Retry-on-conflict with a re-fetch: the health-check reconciler also
		// writes this incident's status, so a bare update races and logs
		// "object has been modified". Re-derive conditions from the freshest
		// object on each attempt.
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Client.Get(ctx, req.NamespacedName, &im); err != nil {
				return err
			}
			r.updateConditions(&im)
			return r.Client.Status().Update(ctx, &im)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("updating conditions: %w", err)
		}
	}

	// 4. Sync ApplicationPersona status. An unattributed incident's personaRef
	// names a workload rather than a persona, so there is nothing to sync and
	// looking would only produce a NotFound on every reconcile.
	if im.Spec.PersonaRef.Kind == "ApplicationPersona" && im.Spec.Attribution != AttributionUnattributed {
		if err := r.syncPersonaStatus(ctx, &im); err != nil {
			logger.Error(err, "failed to sync persona status")
		}
	}

	return ctrl.Result{}, nil
}

// ensureLabels ensures all required labels are set on the IncidentMemory.
// Returns true if any labels were added or changed.
func (r *IncidentController) ensureLabels(im *dorguv1.IncidentMemory) bool {
	if im.Labels == nil {
		im.Labels = make(map[string]string)
	}

	expected := map[string]string{
		LabelPersonaKind:      im.Spec.PersonaRef.Kind,
		LabelPersonaName:      im.Spec.PersonaRef.Name,
		LabelPersonaNamespace: im.Spec.PersonaRef.Namespace,
		LabelCategory:         im.Spec.Category,
		LabelSeverity:         im.Spec.Severity,
		LabelSignal:           im.Spec.Detection.Signal,
	}

	// Only mirror attribution once the spec states it, so an incident written
	// before the field existed is not silently relabelled as something it never
	// claimed to be.
	if im.Spec.Attribution != "" {
		expected[LabelAttribution] = im.Spec.Attribution
	}

	// Only set phase label if phase is non-empty to avoid blank label values.
	if im.Status.Phase != "" {
		expected[LabelPhase] = im.Status.Phase
	}

	changed := false
	for k, v := range expected {
		if im.Labels[k] != v {
			im.Labels[k] = v
			changed = true
		}
	}

	return changed
}

// updateConditions sets standard conditions based on incident state.
func (r *IncidentController) updateConditions(im *dorguv1.IncidentMemory) {
	// Detected condition: True when phase is Detected or Investigating.
	detectedStatus := metav1.ConditionFalse
	detectedReason := "NotDetected"
	detectedMsg := "Incident is not in detected state"
	if im.Status.Phase == PhaseDetected || im.Status.Phase == PhaseInvestigating {
		detectedStatus = metav1.ConditionTrue
		detectedReason = "SignalActive"
		detectedMsg = fmt.Sprintf("Incident detected: %s", im.Spec.Detection.Signal)
	}
	setCondition(&im.Status.Conditions, ConditionDetected, detectedStatus, detectedReason, detectedMsg)

	// Resolved condition: True when phase is Resolved.
	resolvedStatus := metav1.ConditionFalse
	resolvedReason := "NotResolved"
	resolvedMsg := "Incident is not resolved"
	if im.Status.Phase == PhaseResolved {
		resolvedStatus = metav1.ConditionTrue
		resolvedReason = "SignalCleared"
		resolvedMsg = "Incident has been resolved"
	}
	setCondition(&im.Status.Conditions, ConditionResolved, resolvedStatus, resolvedReason, resolvedMsg)

	// Recurring condition: True when occurrenceCount > 1.
	recurringStatus := metav1.ConditionFalse
	recurringReason := "FirstOccurrence"
	recurringMsg := "This is the first occurrence"
	if im.Status.OccurrenceCount > 1 {
		recurringStatus = metav1.ConditionTrue
		recurringReason = "MultipleOccurrences"
		recurringMsg = fmt.Sprintf("Incident has occurred %d times", im.Status.OccurrenceCount)
	}
	setCondition(&im.Status.Conditions, ConditionRecurring, recurringStatus, recurringReason, recurringMsg)
}

// syncPersonaStatus counts active incidents for the referenced persona
// and updates its status.
func (r *IncidentController) syncPersonaStatus(ctx context.Context, im *dorguv1.IncidentMemory) error {
	personaKey := client.ObjectKey{
		Name:      im.Spec.PersonaRef.Name,
		Namespace: im.Spec.PersonaRef.Namespace,
	}

	// Count active incidents for this specific persona.
	var incidents dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &incidents,
		client.InNamespace(im.Namespace),
		client.MatchingLabels{
			LabelPersonaKind: "ApplicationPersona",
			LabelPersonaName: im.Spec.PersonaRef.Name,
		},
	); err != nil {
		return fmt.Errorf("listing incidents: %w", err)
	}

	activeCount := int32(0)
	var latestTime *metav1.Time
	for i := range incidents.Items {
		incident := &incidents.Items[i]
		if incident.Status.Phase != PhaseResolved {
			activeCount++
		}
		if incident.Status.LastOccurrence != nil {
			if latestTime == nil || incident.Status.LastOccurrence.After(latestTime.Time) {
				latestTime = incident.Status.LastOccurrence
			}
		}
	}

	// Update the persona status with retry-on-conflict. The ApplicationPersona
	// reconciler also writes this status, so a bare update races and logs
	// "object has been modified". Re-fetch inside the loop and re-check the
	// change condition against the freshest object (activeCount/latestTime are
	// derived from the incident list and are stable across attempts).
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current dorguv1.ApplicationPersona
		if err := r.Client.Get(ctx, personaKey, &current); err != nil {
			return err
		}
		if current.Status.ActiveIncidents == activeCount && timeEqual(current.Status.LastIncidentTime, latestTime) {
			return nil
		}
		current.Status.ActiveIncidents = activeCount
		current.Status.LastIncidentTime = latestTime
		return r.Client.Status().Update(ctx, &current)
	})
}

// SetupWithManager registers the IncidentController with the manager.
func (r *IncidentController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dorguv1.IncidentMemory{}).
		Complete(r)
}
