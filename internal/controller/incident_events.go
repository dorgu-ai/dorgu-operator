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
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
)

// storeSignalEvents stores contributing signals as DorguEvents via the event store.
// With no store configured (the event pipeline is off) it records nothing.
func (r *HealthCheckReconciler) storeSignalEvents(ctx context.Context, diag *diagnosis.Diagnosis, im *dorguv1.IncidentMemory) {
	if r.EventStore == nil {
		return
	}
	for _, cs := range diag.Contributing {
		internalEvent := &events.InternalEvent{
			ID:       fmt.Sprintf("%s-%s-%d", im.Name, cs.Signal.Type, cs.Signal.DetectedAt.UnixNano()),
			Severity: events.Severity(cs.Signal.Severity),
			Category: events.Category(cs.Signal.Category),
			Source:   cs.Signal.Source,
			Message:  cs.Signal.Message,
			InvolvedObject: dorguv1.ResourceReference{
				Kind:      cs.Signal.Resource.Kind,
				Name:      cs.Signal.Resource.Name,
				Namespace: cs.Signal.Resource.Namespace,
			},
			PersonaRef: cs.Signal.PersonaRef,
			EventTime:  cs.Signal.DetectedAt,
		}

		if err := r.EventStore.Store(ctx, internalEvent); err != nil {
			logFailure(r.Logger, err, "failed to store signal event",
				"signal", cs.Signal.Type,
				"resource", cs.Signal.Resource.Name,
			)
		}
	}
}

// emitK8sEvents emits K8s Events for diagnosis contributing signals.
func (r *HealthCheckReconciler) emitK8sEvents(ctx context.Context, diag *diagnosis.Diagnosis) {
	if r.EventEmitter == nil {
		return
	}
	for _, cs := range diag.Contributing {
		internalEvent := &events.InternalEvent{
			Severity: events.Severity(cs.Signal.Severity),
			Category: events.Category(cs.Signal.Category),
			Source:   cs.Signal.Source,
			Message:  fmt.Sprintf("%s: %s", diag.Summary, cs.Signal.Message),
			InvolvedObject: dorguv1.ResourceReference{
				Kind:      cs.Signal.Resource.Kind,
				Name:      cs.Signal.Resource.Name,
				Namespace: cs.Signal.Resource.Namespace,
			},
			PersonaRef: cs.Signal.PersonaRef,
			EventTime:  cs.Signal.DetectedAt,
		}

		if err := r.EventEmitter.Emit(ctx, internalEvent); err != nil {
			logFailure(r.Logger, err, "failed to emit K8s event",
				"signal", cs.Signal.Type,
				"resource", cs.Signal.Resource.Name,
			)
		}
	}
}

// reportDiscardedDiagnosis makes a lost diagnosis loud, then returns the error
// so the cycle counts it.
//
// A diagnosis is not free: the AI path bills a model call for every one. Losing
// one to a write that could not be retried is a real cost to the user, so it is
// logged at ERROR, recorded as a DorguEvent, and emitted as a Kubernetes Warning
// on the incident. Whatever the operator's failure mode, the user finds out.
func (r *HealthCheckReconciler) reportDiscardedDiagnosis(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	diag *diagnosis.Diagnosis,
	stage string,
	cause error,
) error {
	err := fmt.Errorf("%s for IncidentMemory %s: %w", stage, im.Name, cause)

	persona := ""
	if diag.PersonaRef != nil {
		persona = diag.PersonaRef.Name
	}

	r.Logger.Error(err, "diagnosis discarded: could not persist it after retrying on conflict",
		"incident", im.Name,
		"namespace", im.Namespace,
		"persona", persona,
		"provider", diag.Provider,
		"confidence", diag.Confidence,
		"summary", diag.Summary,
	)

	message := fmt.Sprintf(
		"discarded the%s diagnosis for %s after %s failed: %v. The analysis was produced and paid for but is not recorded.",
		providerSuffix(diag.Provider), im.Name, stage, cause)

	surfaced := &events.InternalEvent{
		ID:       fmt.Sprintf("diagnosis-discarded-%s-%d", im.Name, time.Now().UnixNano()),
		Severity: events.SeverityCritical,
		Category: events.Category(im.Spec.Category),
		Source:   "healthcheck-reconciler",
		Reason:   ReasonDiagnosisDiscarded,
		Message:  message,
		InvolvedObject: dorguv1.ResourceReference{
			Kind:      "IncidentMemory",
			Name:      im.Name,
			Namespace: im.Namespace,
		},
		PersonaRef: diag.PersonaRef,
		EventTime:  time.Now(),
	}

	if r.EventStore != nil {
		if storeErr := r.EventStore.Store(ctx, surfaced); storeErr != nil {
			r.Logger.Error(storeErr, "could not record the discarded diagnosis as a DorguEvent",
				"incident", im.Name)
		}
	}
	if r.EventEmitter != nil {
		if emitErr := r.EventEmitter.Emit(ctx, surfaced); emitErr != nil {
			r.Logger.Error(emitErr, "could not emit a Kubernetes event for the discarded diagnosis",
				"incident", im.Name)
		}
	}

	return err
}

// providerSuffix names the diagnosis source in a user-facing message, and says
// nothing when the source is unknown rather than inventing one.
func providerSuffix(provider string) string {
	if provider == "" {
		return ""
	}
	return " " + provider
}
