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
	"crypto/sha256"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/websocket"
)

const (
	// DefaultReconcileInterval is the default health check reconcile interval.
	DefaultReconcileInterval = 60 * time.Second

	// ResolutionGracePeriod is how long a signal must be absent before auto-resolving.
	ResolutionGracePeriod = 5 * time.Minute

	// MaxIncidentNameLength is the maximum length for IncidentMemory names.
	MaxIncidentNameLength = 253

	// incidentNameOverhead is the fixed overhead in generated names:
	// "im-" (3) + 3 separators ("-") + hash (12) = 18 bytes.
	incidentNameOverhead = 18

	// Label keys for efficient IncidentMemory lookups.
	LabelPersonaKind      = "dorgu.io/persona-kind"
	LabelPersonaName      = "dorgu.io/persona-name"
	LabelPersonaNamespace = "dorgu.io/persona-namespace"
	LabelCategory         = "dorgu.io/category"
	LabelSeverity         = "dorgu.io/severity"
	LabelSignal           = "dorgu.io/signal"
	LabelPhase            = "dorgu.io/phase"

	// IncidentMemory phase constants.
	PhaseDetected      = "Detected"
	PhaseInvestigating = "Investigating"
	PhaseResolved      = "Resolved"
)

// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas,verbs=get;list;watch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationactions,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationactions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=clusterpersonas,verbs=get;list;watch
// +kubebuilder:rbac:groups=dorgu.io,resources=dorguevents,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

// HealthCheckReconciler runs detection and diagnosis on a fixed interval,
// creating and updating IncidentMemory CRDs. It implements manager.Runnable.
type HealthCheckReconciler struct {
	Client            client.Client
	Detection         *detection.Engine
	Diagnosis         *diagnosis.Engine
	EventStore        events.EventStore
	EventEmitter      events.Emitter
	Proposer          remediation.RemediationProposer
	Logger            logr.Logger
	ReconcileInterval time.Duration
	WebSocket         *websocket.Server
}

// Start begins the health check reconciliation loop. Blocks until ctx is cancelled.
// Implements manager.Runnable.
func (r *HealthCheckReconciler) Start(ctx context.Context) error {
	interval := r.ReconcileInterval
	if interval == 0 {
		interval = DefaultReconcileInterval
	}

	r.Logger.Info("starting health check reconciler", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once immediately on startup.
	r.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			r.Logger.Info("health check reconciler stopped")
			return nil
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

// reconcile runs one detection→diagnosis→incident cycle.
func (r *HealthCheckReconciler) reconcile(ctx context.Context) {
	r.Logger.V(1).Info("starting reconciliation cycle")

	// 1. Collect all signals from detection engine.
	signals, err := r.Detection.CollectAll(ctx)
	if err != nil {
		r.Logger.Error(err, "failed to collect signals")
		return
	}

	// 2. Run diagnosis on signals.
	diagnoses, err := r.Diagnosis.Analyze(ctx, signals)
	if err != nil {
		r.Logger.Error(err, "failed to analyze signals")
		return
	}

	r.Logger.V(1).Info("reconciliation cycle results",
		"signals", len(signals),
		"diagnoses", len(diagnoses),
	)

	// 3. Process each diagnosis: create/update IncidentMemory + store events.
	activeSignalKeys := make(map[string]bool)
	for i := range diagnoses {
		diag := &diagnoses[i]
		// Only track diagnoses with a PersonaRef for resolution detection.
		// Diagnoses without a PersonaRef are skipped by createIncident anyway.
		if diag.PersonaRef == nil {
			continue
		}
		if err := r.processDiagnosis(ctx, diag, activeSignalKeys); err != nil {
			r.Logger.Error(err, "failed to process diagnosis",
				"summary", diag.Summary,
				"category", diag.Category,
			)
		}
	}

	// 4. Check for resolved incidents.
	if err := r.resolveCleared(ctx, activeSignalKeys); err != nil {
		r.Logger.Error(err, "failed to resolve cleared incidents")
	}

	// 5. Broadcast aggregate health update via WebSocket.
	if r.WebSocket != nil {
		r.broadcastHealthSummary(ctx)
	}
}

// processDiagnosis creates or updates an IncidentMemory for a diagnosis,
// stores events, emits K8s events for high-severity signals, and proposes remediation.
func (r *HealthCheckReconciler) processDiagnosis(
	ctx context.Context,
	diag *diagnosis.Diagnosis,
	activeSignalKeys map[string]bool,
) error {
	// Track the signal key for resolution detection.
	signalKey := r.signalKey(diag)
	activeSignalKeys[signalKey] = true

	now := metav1.Now()

	// Check if matching IncidentMemory already exists.
	existing, err := r.findMatchingIncident(ctx, diag)
	if err != nil {
		return fmt.Errorf("finding matching incident: %w", err)
	}

	var incident *dorguv1.IncidentMemory
	if existing != nil {
		// Update existing incident.
		if err := r.updateExistingIncident(ctx, existing, diag, now); err != nil {
			return err
		}
		incident = existing
	} else {
		// Create new IncidentMemory.
		if err := r.createIncident(ctx, diag, now); err != nil {
			return err
		}
		// Re-fetch the incident for proposer.
		incident, _ = r.findMatchingIncident(ctx, diag)
	}

	// Propose remediation if proposer is configured and incident is not resolved.
	if r.Proposer != nil && incident != nil && incident.Status.Phase != PhaseResolved {
		result, proposeErr := r.Proposer.Propose(ctx, *diag, incident)
		if proposeErr != nil {
			r.Logger.Error(proposeErr, "failed to propose remediation", "incident", incident.Name)
		} else if result.Proposed {
			r.Logger.Info("remediation proposed", "action", result.Action.Name, "incident", incident.Name)
			// Broadcast remediation creation via WebSocket.
			if r.WebSocket != nil && result.Action != nil {
				r.WebSocket.BroadcastRemediation(websocket.RemediationEvent{
					EventType:   "created",
					Name:        result.Action.Name,
					Namespace:   result.Action.Namespace,
					ActionType:  result.Action.Spec.Action.Type,
					Phase:       result.Action.Status.Phase,
					Confidence:  result.Action.Spec.Confidence,
					PersonaName: result.Action.Spec.PersonaRef.Name,
					PersonaKind: result.Action.Spec.PersonaRef.Kind,
				})
			}
		} else if result.SkipReason != "" {
			r.Logger.V(1).Info("remediation skipped", "reason", result.SkipReason, "incident", incident.Name)
		}
	}

	return nil
}

// findMatchingIncident searches for an active IncidentMemory matching a diagnosis.
func (r *HealthCheckReconciler) findMatchingIncident(
	ctx context.Context,
	diag *diagnosis.Diagnosis,
) (*dorguv1.IncidentMemory, error) {
	if diag.PersonaRef == nil {
		return nil, nil
	}

	primarySignal := primarySignalType(diag)

	labelSelector := labels.SelectorFromSet(labels.Set{
		LabelPersonaKind: diag.PersonaRef.Kind,
		LabelPersonaName: diag.PersonaRef.Name,
		LabelCategory:    diag.Category,
		LabelSignal:      string(primarySignal),
	})

	var list dorguv1.IncidentMemoryList
	opts := []client.ListOption{
		client.MatchingLabelsSelector{Selector: labelSelector},
	}
	if diag.PersonaRef.Namespace != "" {
		opts = append(opts, client.InNamespace(diag.PersonaRef.Namespace))
	}

	if err := r.Client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing IncidentMemories: %w", err)
	}

	for i := range list.Items {
		im := &list.Items[i]
		if im.Status.Phase != PhaseResolved {
			return im, nil
		}
	}

	return nil, nil
}

// updateExistingIncident updates an active IncidentMemory with new diagnosis data.
func (r *HealthCheckReconciler) updateExistingIncident(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	diag *diagnosis.Diagnosis,
	now metav1.Time,
) error {
	// Update spec fields.
	im.Spec.Detection.LastSeen = now

	// Update root cause if confidence improved.
	if diag.Confidence > 0 {
		existingConfidence := 0.0
		if im.Spec.RootCause != nil {
			parsed, err := strconv.ParseFloat(im.Spec.RootCause.Confidence, 64)
			if err == nil {
				existingConfidence = parsed
			}
		}
		if diag.Confidence > existingConfidence {
			im.Spec.RootCause = buildRootCause(diag)
		}
	}

	// Update affected resources.
	im.Spec.Detection.AffectedResources = toResourceRefs(diag.AffectedResources)

	if err := r.Client.Update(ctx, im); err != nil {
		return fmt.Errorf("updating IncidentMemory %s: %w", im.Name, err)
	}

	// Update status subresource with retry-on-conflict. Re-fetching inside
	// the loop ensures we always carry the latest ResourceVersion even if a
	// concurrent controller wrote to the object between attempts.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
			return err
		}
		im.Status.OccurrenceCount++
		im.Status.LastOccurrence = &now
		return r.Client.Status().Update(ctx, im)
	})
	if err != nil {
		return fmt.Errorf("updating IncidentMemory status %s: %w", im.Name, err)
	}

	r.Logger.V(1).Info("updated existing incident",
		"name", im.Name,
		"occurrenceCount", im.Status.OccurrenceCount,
	)

	// Broadcast incident update via WebSocket.
	if r.WebSocket != nil {
		summary := ""
		if im.Spec.RootCause != nil {
			summary = im.Spec.RootCause.Summary
		}
		r.WebSocket.BroadcastIncident(websocket.IncidentEvent{
			EventType:   "updated",
			Name:        im.Name,
			Namespace:   im.Namespace,
			Severity:    im.Spec.Severity,
			Category:    im.Spec.Category,
			Signal:      im.Spec.Detection.Signal,
			Phase:       im.Status.Phase,
			PersonaName: im.Spec.PersonaRef.Name,
			PersonaKind: im.Spec.PersonaRef.Kind,
			Summary:     summary,
		})
	}

	return nil
}

// createIncident creates a new IncidentMemory CRD from a diagnosis.
func (r *HealthCheckReconciler) createIncident(
	ctx context.Context,
	diag *diagnosis.Diagnosis,
	now metav1.Time,
) error {
	if diag.PersonaRef == nil {
		r.Logger.V(1).Info("skipping diagnosis without persona ref", "summary", diag.Summary)
		return nil
	}

	primarySignal := primarySignalType(diag)
	namespace := diag.PersonaRef.Namespace
	if namespace == "" {
		namespace = "default"
	}

	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateIncidentName(namespace, diag.PersonaRef.Name, string(primarySignal)),
			Namespace: namespace,
			Labels: map[string]string{
				LabelPersonaKind:      diag.PersonaRef.Kind,
				LabelPersonaName:      diag.PersonaRef.Name,
				LabelPersonaNamespace: namespace,
				LabelCategory:         diag.Category,
				LabelSeverity:         string(diag.Severity),
				LabelSignal:           string(primarySignal),
				LabelPhase:            PhaseDetected,
			},
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: *diag.PersonaRef,
			Category:   diag.Category,
			Severity:   string(diag.Severity),
			Detection: dorguv1.DetectionInfo{
				Signal:            string(primarySignal),
				Source:            diag.Provider,
				FirstSeen:         now,
				LastSeen:          now,
				AffectedResources: toResourceRefs(diag.AffectedResources),
			},
			RootCause: buildRootCause(diag),
		},
	}

	if err := r.Client.Create(ctx, im); err != nil {
		return fmt.Errorf("creating IncidentMemory: %w", err)
	}

	// Set initial status.
	im.Status = dorguv1.IncidentMemoryStatus{
		Phase:           PhaseDetected,
		OccurrenceCount: 1,
		LastOccurrence:  &now,
	}
	if err := r.Client.Status().Update(ctx, im); err != nil {
		// Object was created but status was not set. Log prominently so the
		// orphan is observable; updateExistingIncident will repair on next cycle.
		r.Logger.Error(err, "IncidentMemory created but initial status update failed",
			"name", im.Name)
		return fmt.Errorf("setting initial IncidentMemory status: %w", err)
	}

	r.Logger.Info("created incident",
		"name", im.Name,
		"category", diag.Category,
		"severity", diag.Severity,
		"signal", primarySignal,
	)

	// Broadcast incident creation via WebSocket.
	if r.WebSocket != nil {
		summary := ""
		if diag.Summary != "" {
			summary = diag.Summary
		}
		r.WebSocket.BroadcastIncident(websocket.IncidentEvent{
			EventType:   "created",
			Name:        im.Name,
			Namespace:   im.Namespace,
			Severity:    string(diag.Severity),
			Category:    diag.Category,
			Signal:      string(primarySignal),
			Phase:       PhaseDetected,
			PersonaName: diag.PersonaRef.Name,
			PersonaKind: diag.PersonaRef.Kind,
			Summary:     summary,
		})
	}

	// Store contributing signals as DorguEvents.
	r.storeSignalEvents(ctx, diag, im)

	// Emit K8s Events for high-severity signals.
	if diag.Severity == detection.SeverityWarning || diag.Severity == detection.SeverityCritical {
		r.emitK8sEvents(ctx, diag)
	}

	return nil
}

// resolveCleared checks active incidents and resolves those whose signals have cleared.
// Only queries non-resolved incidents using label selectors for efficiency.
func (r *HealthCheckReconciler) resolveCleared(ctx context.Context, activeSignalKeys map[string]bool) error {
	// Use label selector to only fetch non-resolved incidents.
	var list dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &list,
		client.MatchingLabels{LabelPhase: PhaseDetected},
	); err != nil {
		return fmt.Errorf("listing active IncidentMemories: %w", err)
	}

	// Also check Investigating phase incidents.
	var investigatingList dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &investigatingList,
		client.MatchingLabels{LabelPhase: PhaseInvestigating},
	); err != nil {
		return fmt.Errorf("listing investigating IncidentMemories: %w", err)
	}
	list.Items = append(list.Items, investigatingList.Items...)

	for i := range list.Items {
		im := &list.Items[i]

		// Build signal key from incident fields.
		key := incidentSignalKey(im)
		if activeSignalKeys[key] {
			continue // Still active.
		}

		// Check grace period: signal must be absent for ResolutionGracePeriod.
		if time.Since(im.Spec.Detection.LastSeen.Time) < ResolutionGracePeriod {
			continue
		}

		// Resolve the incident: update spec (resolution + labels) first.
		im.Labels[LabelPhase] = PhaseResolved
		im.Spec.Resolution = &dorguv1.ResolutionInfo{
			Action:  "auto-resolved",
			Outcome: "resolved",
		}

		if err := r.Client.Update(ctx, im); err != nil {
			r.Logger.Error(err, "failed to resolve incident", "name", im.Name)
			continue
		}

		// Update status with retry-on-conflict. Re-fetching inside the loop picks
		// up any concurrent ResourceVersion bump from another controller racing
		// on this incident's status (quiets "object has been modified" noise).
		statusErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
				return err
			}
			im.Status.Phase = PhaseResolved
			return r.Client.Status().Update(ctx, im)
		})
		if statusErr != nil {
			r.Logger.Error(statusErr, "failed to update resolved incident status", "name", im.Name)
			continue
		}

		r.Logger.Info("auto-resolved incident",
			"name", im.Name,
			"category", im.Spec.Category,
			"signal", im.Spec.Detection.Signal,
			"lastSeen", im.Spec.Detection.LastSeen.Time,
		)

		// Broadcast incident resolution via WebSocket.
		if r.WebSocket != nil {
			r.WebSocket.BroadcastIncident(websocket.IncidentEvent{
				EventType:   "resolved",
				Name:        im.Name,
				Namespace:   im.Namespace,
				Severity:    im.Spec.Severity,
				Category:    im.Spec.Category,
				Signal:      im.Spec.Detection.Signal,
				Phase:       PhaseResolved,
				PersonaName: im.Spec.PersonaRef.Name,
				PersonaKind: im.Spec.PersonaRef.Kind,
			})
		}
	}

	return nil
}

// broadcastHealthSummary collects aggregate health data and broadcasts it.
func (r *HealthCheckReconciler) broadcastHealthSummary(ctx context.Context) {
	// Count active incidents.
	var activeIncidents dorguv1.IncidentMemoryList
	activeCount := 0
	if err := r.Client.List(ctx, &activeIncidents,
		client.MatchingLabels{LabelPhase: PhaseDetected},
	); err == nil {
		activeCount += len(activeIncidents.Items)
	}
	var investigatingIncidents dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &investigatingIncidents,
		client.MatchingLabels{LabelPhase: PhaseInvestigating},
	); err == nil {
		activeCount += len(investigatingIncidents.Items)
	}

	// Count pending remediations.
	var pendingRemedies dorguv1.RemediationActionList
	pendingCount := 0
	if err := r.Client.List(ctx, &pendingRemedies); err == nil {
		for i := range pendingRemedies.Items {
			if pendingRemedies.Items[i].Status.Phase == "Pending" {
				pendingCount++
			}
		}
	}

	r.WebSocket.BroadcastHealthUpdate(websocket.HealthUpdateEvent{
		EventType:       "health-update",
		ActiveIncidents: activeCount,
		PendingRemedies: pendingCount,
	})
}

// storeSignalEvents stores contributing signals as DorguEvents via the event store.
func (r *HealthCheckReconciler) storeSignalEvents(ctx context.Context, diag *diagnosis.Diagnosis, im *dorguv1.IncidentMemory) {
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
			r.Logger.Error(err, "failed to store signal event",
				"signal", cs.Signal.Type,
				"resource", cs.Signal.Resource.Name,
			)
		}
	}
}

// emitK8sEvents emits K8s Events for diagnosis contributing signals.
func (r *HealthCheckReconciler) emitK8sEvents(ctx context.Context, diag *diagnosis.Diagnosis) {
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
			r.Logger.Error(err, "failed to emit K8s event",
				"signal", cs.Signal.Type,
				"resource", cs.Signal.Resource.Name,
			)
		}
	}
}

// signalKey creates a unique key for a diagnosis to track active signals.
// Only called for diagnoses with non-nil PersonaRef.
func (r *HealthCheckReconciler) signalKey(diag *diagnosis.Diagnosis) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		diag.PersonaRef.Kind,
		diag.PersonaRef.Namespace,
		diag.PersonaRef.Name,
		diag.Category,
		primarySignalType(diag),
	)
}

// incidentSignalKey reconstructs the signal key from an IncidentMemory.
func incidentSignalKey(im *dorguv1.IncidentMemory) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		im.Spec.PersonaRef.Kind,
		im.Spec.PersonaRef.Namespace,
		im.Spec.PersonaRef.Name,
		im.Spec.Category,
		im.Spec.Detection.Signal,
	)
}

// primarySignalType returns the primary signal type from a diagnosis.
func primarySignalType(diag *diagnosis.Diagnosis) detection.SignalType {
	if len(diag.Contributing) > 0 {
		return diag.Contributing[0].Signal.Type
	}
	return detection.SignalType("Unknown")
}

// generateIncidentName creates a deterministic name for an IncidentMemory.
// Format: im-{namespace}-{persona}-{signal}-{hash}
// Truncates variable segments to ensure the hash suffix is always preserved.
func generateIncidentName(namespace, persona, signal string) string {
	hashInput := fmt.Sprintf("%s/%s/%s", namespace, persona, signal)
	hash := sha256.Sum256([]byte(hashInput))
	hashStr := fmt.Sprintf("%x", hash[:6])

	ns := sanitizeName(namespace)
	p := sanitizeName(persona)
	sig := sanitizeName(signal)

	// Reserve space for overhead, distribute remaining among segments.
	maxSegmentLen := (MaxIncidentNameLength - incidentNameOverhead) / 3
	ns = truncate(ns, maxSegmentLen)
	p = truncate(p, maxSegmentLen)
	sig = truncate(sig, maxSegmentLen)

	return fmt.Sprintf("im-%s-%s-%s-%s", ns, p, sig, hashStr)
}

// truncate returns s truncated to maxLen characters.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// sanitizeName converts a string to a valid K8s name component.
func sanitizeName(s string) string {
	result := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32)
		} else {
			result = append(result, '-')
		}
	}
	return string(result)
}

// buildRootCause converts a diagnosis into an IncidentMemory RootCauseInfo.
func buildRootCause(diag *diagnosis.Diagnosis) *dorguv1.RootCauseInfo {
	if diag.Confidence == 0 && diag.Summary == "" {
		return nil
	}

	contributing := make([]dorguv1.ContributingSignal, 0, len(diag.Contributing))
	for _, cs := range diag.Contributing {
		contributing = append(contributing, dorguv1.ContributingSignal{
			Signal: string(cs.Signal.Type),
			Detail: cs.Detail,
		})
	}

	return &dorguv1.RootCauseInfo{
		Summary:      diag.Summary,
		Confidence:   fmt.Sprintf("%.2f", diag.Confidence),
		Provider:     diag.Provider,
		Contributing: contributing,
	}
}

// toResourceRefs converts affected resources, preserving the "affected" role.
func toResourceRefs(refs []dorguv1.ResourceReference) []dorguv1.ResourceReference {
	result := make([]dorguv1.ResourceReference, len(refs))
	for i, ref := range refs {
		result[i] = dorguv1.ResourceReference{
			Kind:      ref.Kind,
			Name:      ref.Name,
			Namespace: ref.Namespace,
			Role:      "affected",
		}
	}
	return result
}

// timeEqual compares two *metav1.Time values for equality.
func timeEqual(a, b *metav1.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(b)
}
