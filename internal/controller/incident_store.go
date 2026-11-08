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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// findMatchingIncident searches for an active IncidentMemory matching a
// diagnosis about a subject. Attribution is part of the match: an unattributed
// incident and the incident raised once a persona claims the same workload are
// two different records, and neither may be mistaken for the other.
func (r *HealthCheckReconciler) findMatchingIncident(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
) (*dorguv1.IncidentMemory, error) {
	primarySignal := primarySignalType(diag)

	labelSelector := labels.SelectorFromSet(labels.Set{
		LabelPersonaKind: subject.personaRef.Kind,
		LabelPersonaName: subject.personaRef.Name,
		LabelCategory:    diag.Category,
		LabelSignal:      string(primarySignal),
	})

	var list dorguv1.IncidentMemoryList
	opts := []client.ListOption{
		client.MatchingLabelsSelector{Selector: labelSelector},
	}
	if subject.namespace != "" {
		opts = append(opts, client.InNamespace(subject.namespace))
	}

	if err := r.Client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing IncidentMemories: %w", err)
	}

	for i := range list.Items {
		im := &list.Items[i]
		if im.Status.Phase == PhaseResolved || !subject.matchesIncident(im) {
			continue
		}
		return im, nil
	}

	return nil, nil
}

// matchesIncident reports whether an existing incident is the same record this
// subject would write.
//
// Attribution has to agree: an unattributed incident and the incident raised
// once a persona claims the same workload are two different records. An
// incident written before the field existed states nothing, and adopting it is
// better than raising a duplicate beside it, so it is treated as the
// persona-attributed record it was; applyDiagnosisToSpec backfills the field on
// the way through.
func (s incidentSubject) matchesIncident(im *dorguv1.IncidentMemory) bool {
	if im.Spec.Attribution == "" {
		return !s.unattributed
	}
	return im.Spec.Attribution == s.attribution()
}

// updateExistingIncident updates an active IncidentMemory with new diagnosis data.
//
// Both writes retry with a re-fetch, on Conflict and on NotFound alike. The
// object handed in comes from a List, so its ResourceVersion is a snapshot: any
// concurrent write (the incident controller stamping conditions, the
// remediation controller resolving) invalidates it and a bare Update fails with
// "the object has been modified". NotFound is the second half, and the half
// CF4-2 missed: reads come from the manager's cache, which can lag a write the
// API server has already accepted, and a Get that lands in that gap used to end
// the diagnosis then and there (F-05).
func (r *HealthCheckReconciler) updateExistingIncident(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	now metav1.Time,
) error {
	specErr := retryIncidentWrite(func(int) error {
		var fresh dorguv1.IncidentMemory
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), &fresh); err != nil {
			return err
		}
		applyDiagnosisToSpec(&fresh, subject, diag, now)
		if err := r.Client.Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.DeepCopyInto(im)
		return nil
	})
	if specErr != nil {
		return r.reportDiscardedDiagnosis(ctx, im, diag, "recording the root cause", specErr)
	}

	// Update the status subresource the same way. Re-fetching inside the loop
	// ensures we always carry the latest ResourceVersion even if a concurrent
	// controller wrote to the object between attempts.
	err := retryIncidentWrite(func(int) error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
			return err
		}
		im.Status.OccurrenceCount++
		im.Status.LastOccurrence = &now
		return r.Client.Status().Update(ctx, im)
	})
	if err != nil {
		return r.reportDiscardedDiagnosis(ctx, im, diag, "recording the occurrence count", err)
	}

	r.Logger.V(1).Info("updated existing incident",
		"name", im.Name,
		"occurrenceCount", im.Status.OccurrenceCount,
	)

	return nil
}

// createIncident creates a new IncidentMemory CRD from a diagnosis.
func (r *HealthCheckReconciler) createIncident(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	now metav1.Time,
) error {
	primarySignal := primarySignalType(diag)
	namespace := subject.namespace
	if namespace == "" {
		namespace = "default"
	}

	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateIncidentName(namespace, subject.incidentNameKey(), string(primarySignal)),
			Namespace: namespace,
			Labels: map[string]string{
				LabelPersonaKind:      subject.personaRef.Kind,
				LabelPersonaName:      subject.personaRef.Name,
				LabelPersonaNamespace: namespace,
				LabelCategory:         diag.Category,
				LabelSeverity:         string(diag.Severity),
				LabelSignal:           string(primarySignal),
				LabelPhase:            PhaseDetected,
				LabelAttribution:      subject.attribution(),
			},
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef:  subject.personaRef,
			Attribution: subject.attribution(),
			Category:    diag.Category,
			Severity:    string(diag.Severity),
			Detection: dorguv1.DetectionInfo{
				Signal:            string(primarySignal),
				Source:            diag.Provider,
				FirstSeen:         now,
				LastSeen:          now,
				AffectedResources: toResourceRefs(diag.AffectedResources),
			},
			RootCause: annotateUnattributed(buildRootCause(diag), subject),
		},
	}

	switch createErr := r.Client.Create(ctx, im); {
	case createErr == nil:
		metrics.RecordIncident(diag.Category, subject.attribution())
		if first, ok := earliestSignal(diag); ok {
			metrics.RecordTimeToDetect(now.Sub(first))
		}
	case apierrors.IsAlreadyExists(createErr):
		// The incident is already there and our cached List did not see it yet.
		// Fold this diagnosis into the object that really exists rather than
		// treating a stale read as a reason to throw away a paid-for analysis
		// (F-05).
		var existing dorguv1.IncidentMemory
		getErr := retryIncidentWrite(func(int) error {
			return r.Client.Get(ctx, client.ObjectKeyFromObject(im), &existing)
		})
		if getErr != nil {
			return r.reportDiscardedDiagnosis(ctx, im, diag,
				"adopting the incident that already existed", getErr)
		}
		r.Logger.V(1).Info("adopted an incident the cache had not caught up with", "name", im.Name)
		return r.updateExistingIncident(ctx, &existing, subject, diag, now)
	default:
		return fmt.Errorf("creating IncidentMemory: %w", createErr)
	}

	// Set the initial status, retrying on Conflict and on NotFound. The first
	// attempt writes through the object Create just returned, which already
	// carries a valid ResourceVersion, so the common path never reads at all.
	// Re-reading unconditionally is what exposed the write to a cache that had
	// not caught up: the Get returned "not found" for an object the API server
	// had just accepted, retry.RetryOnConflict did not consider that worth
	// another try, and the diagnosis died there. Five of the first six
	// diagnoses in a fresh install were lost this way (F-05).
	statusErr := retryIncidentWrite(func(attempt int) error {
		if attempt > 0 {
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
				return err
			}
		}
		im.Status.Phase = PhaseDetected
		im.Status.OccurrenceCount = 1
		im.Status.LastOccurrence = &now
		return r.Client.Status().Update(ctx, im)
	})
	if statusErr != nil {
		// The object exists but carries no status. Surface the loss rather than
		// leaving an orphan nobody knows about; updateExistingIncident repairs
		// the status on the next cycle.
		return r.reportDiscardedDiagnosis(ctx, im, diag, "setting the initial incident status", statusErr)
	}

	r.Logger.Info("created incident",
		"name", im.Name,
		"category", diag.Category,
		"severity", diag.Severity,
		"signal", primarySignal,
		"attribution", subject.attribution(),
	)

	// Store contributing signals as DorguEvents.
	r.storeSignalEvents(ctx, diag, im)

	// Emit K8s Events for high-severity signals.
	if diag.Severity == detection.SeverityWarning || diag.Severity == detection.SeverityCritical {
		r.emitK8sEvents(ctx, diag)
	}

	return nil
}

// signalKey creates a unique key for a diagnosis to track active signals. It is
// keyed on the subject rather than the diagnosis, so it matches whatever was
// written to the incident, including for an unattributed subject where the
// diagnosis itself carries no persona.
func (r *HealthCheckReconciler) signalKey(subject incidentSubject, diag *diagnosis.Diagnosis) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		subject.personaRef.Kind,
		subject.personaRef.Namespace,
		subject.personaRef.Name,
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

// applyDiagnosisToSpec folds a diagnosis into an IncidentMemory spec. It is
// separate from the write so the retry loop can re-apply it to a freshly fetched
// object instead of replaying a stale one.
func applyDiagnosisToSpec(
	im *dorguv1.IncidentMemory,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	now metav1.Time,
) {
	im.Spec.Detection.LastSeen = now

	// Backfill attribution on incidents raised before it existed, and on any
	// incident adopted after an AlreadyExists. Without it findMatchingIncident
	// would stop recognising the record and raise a duplicate every cycle.
	if im.Spec.Attribution == "" {
		im.Spec.Attribution = subject.attribution()
	}
	if im.Labels == nil {
		im.Labels = map[string]string{}
	}
	if im.Labels[LabelAttribution] == "" {
		im.Labels[LabelAttribution] = im.Spec.Attribution
	}

	// Refresh the root cause when the new diagnosis is at least as confident as
	// the recorded one. The tie matters: an AI-enhanced diagnosis carries the
	// same numeric confidence as the rule-based diagnosis it enhances, so
	// requiring a strict improvement is how an incident first recorded before AI
	// was configured stayed stamped "rule-based" for its whole life (F-05).
	if diag.Confidence > 0 {
		existingConfidence := 0.0
		if im.Spec.RootCause != nil {
			parsed, err := strconv.ParseFloat(im.Spec.RootCause.Confidence, 64)
			if err == nil {
				existingConfidence = parsed
			}
		}
		if diag.Confidence >= existingConfidence {
			im.Spec.RootCause = annotateUnattributed(buildRootCause(diag), subject)
		}
	}

	im.Spec.Detection.AffectedResources = toResourceRefs(diag.AffectedResources)
}

// annotateUnattributed adds, to the field a user actually reads, the reason an
// incident names a workload instead of an application. Dorgu can see the
// workload is broken and can say nothing more useful than that, so it says
// exactly that rather than presenting a persona-shaped record with no persona
// behind it.
func annotateUnattributed(rc *dorguv1.RootCauseInfo, subject incidentSubject) *dorguv1.RootCauseInfo {
	if rc == nil || !subject.unattributed {
		return rc
	}

	rc.Summary = strings.TrimSpace(rc.Summary) + fmt.Sprintf(
		" No ApplicationPersona in namespace %s claims workload %s, so this incident is recorded against the workload"+
			" and Dorgu will not propose a remediation for it. Run \"dorgu persona import -n %s\" to onboard it.",
		subject.namespace, subject.personaRef.Name, subject.namespace)

	return rc
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
