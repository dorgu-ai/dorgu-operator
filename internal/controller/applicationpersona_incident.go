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
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/websocket"
)

// detectAndRecordOOMIncidents scans pods for OOM signals and creates/updates
// IncidentMemory + RemediationAction CRDs when OOM is detected.
// reasonOOMKilled is the container termination reason Kubernetes reports for an
// out-of-memory kill, and the signal name Dorgu records for it.
const reasonOOMKilled = "OOMKilled"

func (r *ApplicationPersonaReconciler) detectAndRecordOOMIncidents(
	ctx context.Context,
	persona *dorguv1.ApplicationPersona,
	deploy *appsv1.Deployment,
) error {
	selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return fmt.Errorf("invalid deployment selector: %w", err)
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, &client.ListOptions{
		Namespace:     deploy.Namespace,
		LabelSelector: selector,
	}); err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	hasOOM := false
	var oomPodName string
	var oomContainerName string
	var memoryLimit string
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if isOOMKilled(cs) {
				hasOOM = true
				oomPodName = pod.Name
				oomContainerName = cs.Name
				for _, c := range pod.Spec.Containers {
					if c.Name == cs.Name {
						if ml, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
							memoryLimit = ml.String()
						}
						break
					}
				}
				break
			}
		}
		if hasOOM {
			break
		}
	}

	if !hasOOM {
		return nil
	}

	personaRef := dorguv1.PersonaReference{
		Kind:      "ApplicationPersona",
		Name:      persona.Name,
		Namespace: persona.Namespace,
	}

	now := metav1.Now()
	incidentName := generateIncidentName(persona.Namespace, persona.Name, "oomkilled")

	existing := &dorguv1.IncidentMemory{}
	err = r.Get(ctx, client.ObjectKey{Name: incidentName, Namespace: persona.Namespace}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("checking existing incident: %w", err)
	}

	if err == nil {
		// Update existing incident.
		existing.Spec.Detection.LastSeen = now
		if updateErr := r.Update(ctx, existing); updateErr != nil {
			return fmt.Errorf("updating IncidentMemory: %w", updateErr)
		}
		// Update status with retry-on-conflict. Re-fetching inside the loop
		// picks up any concurrent ResourceVersion bump from another controller.
		statusErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if getErr := r.Get(ctx, client.ObjectKeyFromObject(existing), existing); getErr != nil {
				return getErr
			}
			existing.Status.OccurrenceCount++
			existing.Status.LastOccurrence = &now
			return r.Status().Update(ctx, existing)
		})
		if statusErr != nil {
			return fmt.Errorf("updating IncidentMemory status: %w", statusErr)
		}
		return nil
	}

	// Create new IncidentMemory.
	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      incidentName,
			Namespace: persona.Namespace,
			Labels: map[string]string{
				LabelPersonaKind:      "ApplicationPersona",
				LabelPersonaName:      persona.Name,
				LabelPersonaNamespace: persona.Namespace,
				LabelCategory:         "health",
				LabelSeverity:         "critical",
				LabelSignal:           reasonOOMKilled,
				LabelPhase:            PhaseDetected,
			},
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: personaRef,
			Category:   "health",
			Severity:   "critical",
			Detection: dorguv1.DetectionInfo{
				Signal:    reasonOOMKilled,
				Source:    "applicationpersona-reconciler",
				FirstSeen: now,
				LastSeen:  now,
				AffectedResources: []dorguv1.ResourceReference{
					{
						Kind:      "Pod",
						Name:      oomPodName,
						Namespace: persona.Namespace,
						Role:      "affected",
					},
				},
			},
			RootCause: &dorguv1.RootCauseInfo{
				Summary:    fmt.Sprintf("Container %s in pod %s was OOMKilled", oomContainerName, oomPodName),
				Confidence: "0.90",
				Provider:   "applicationpersona-reconciler",
				Contributing: []dorguv1.ContributingSignal{
					{Signal: reasonOOMKilled, Detail: fmt.Sprintf("Container memory limit: %s", memoryLimit)},
				},
			},
		},
	}

	if err := r.Create(ctx, im); err != nil {
		return fmt.Errorf("creating IncidentMemory: %w", err)
	}

	// Set initial status.
	im.Status = dorguv1.IncidentMemoryStatus{
		Phase:           PhaseDetected,
		OccurrenceCount: 1,
		LastOccurrence:  &now,
	}
	if err := r.Status().Update(ctx, im); err != nil {
		return fmt.Errorf("setting IncidentMemory status: %w", err)
	}

	// Propose remediation: increase memory limits.
	if err := r.proposeMemoryRemediation(ctx, persona, im, memoryLimit); err != nil {
		return fmt.Errorf("proposing remediation: %w", err)
	}

	return nil
}

// proposeMemoryRemediation creates a RemediationAction proposing a memory limit increase.
func (r *ApplicationPersonaReconciler) proposeMemoryRemediation(
	ctx context.Context,
	persona *dorguv1.ApplicationPersona,
	incident *dorguv1.IncidentMemory,
	currentLimit string,
) error {
	patch := map[string]any{
		"resources": map[string]any{
			"limits": map[string]any{
				"memory": doubleMemory(currentLimit),
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshaling patch: %w", err)
	}

	prePatch := map[string]any{
		"resources": map[string]any{
			"limits": map[string]any{
				"memory": currentLimit,
			},
		},
	}
	prePatchBytes, _ := json.Marshal(prePatch)

	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("ra-%s-memory", incident.Name),
			Namespace: persona.Namespace,
			Labels: map[string]string{
				LabelPersonaName: persona.Name,
				LabelSignal:      reasonOOMKilled,
			},
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{
				Name:      incident.Name,
				Namespace: incident.Namespace,
			},
			PersonaRef: dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      persona.Name,
				Namespace: persona.Namespace,
			},
			TrustLevel:  1,
			Explanation: fmt.Sprintf("Container was OOMKilled with memory limit %s; proposing increase", currentLimit),
			Confidence:  "0.90",
			Action: dorguv1.RemediationActionDetail{
				Type:          "persona-update",
				Patch:         &apiextensionsv1.JSON{Raw: patchBytes},
				PrePatchState: &apiextensionsv1.JSON{Raw: prePatchBytes},
			},
		},
	}

	if err := r.Create(ctx, ra); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating RemediationAction: %w", err)
	}

	// Set initial status.
	ra.Status.Phase = "Pending"
	if err := r.Status().Update(ctx, ra); err != nil {
		return fmt.Errorf("setting RemediationAction status: %w", err)
	}

	// Broadcast remediation creation via WebSocket.
	if r.WebSocket != nil {
		r.WebSocket.BroadcastRemediation(websocket.RemediationEvent{
			EventType:   "created",
			Name:        ra.Name,
			Namespace:   ra.Namespace,
			ActionType:  ra.Spec.Action.Type,
			Phase:       ra.Status.Phase,
			Confidence:  ra.Spec.Confidence,
			PersonaName: ra.Spec.PersonaRef.Name,
			PersonaKind: ra.Spec.PersonaRef.Kind,
		})
	}

	return nil
}

// isOOMKilled checks if a container status indicates OOM.
func isOOMKilled(cs corev1.ContainerStatus) bool {
	if cs.State.Terminated != nil && cs.State.Terminated.Reason == reasonOOMKilled {
		return true
	}
	if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == reasonOOMKilled {
		return true
	}
	return false
}

// doubleMemory parses a memory quantity string and returns double the value.
func doubleMemory(limit string) string {
	if limit == "" {
		return "128Mi"
	}
	// Simple heuristic: parse common formats
	q, err := parseMemoryQuantity(limit)
	if err != nil {
		return "128Mi"
	}
	return fmt.Sprintf("%dMi", q*2)
}

// parseMemoryQuantity extracts the numeric value in MiB from a K8s memory string.
func parseMemoryQuantity(s string) (int64, error) {
	// Use resource.Quantity for correct parsing
	// We import it indirectly via the corev1 resource types
	if len(s) == 0 {
		return 0, fmt.Errorf("empty quantity")
	}

	// Handle common suffixes
	var multiplier int64
	var numStr string
	switch {
	case len(s) > 2 && s[len(s)-2:] == "Mi":
		multiplier = 1
		numStr = s[:len(s)-2]
	case len(s) > 2 && s[len(s)-2:] == "Gi":
		multiplier = 1024
		numStr = s[:len(s)-2]
	case len(s) > 2 && s[len(s)-2:] == "Ki":
		// No multiplier: this branch returns directly rather than falling through
		// to the shared "value * multiplier" tail below.
		numStr = s[:len(s)-2]
		// Ki -> MiB: divide by 1024, but keep at least 1
		val := parseInt64(numStr)
		result := max(val/1024, 1)
		return result, nil
	default:
		// Assume bytes, convert to MiB
		val := parseInt64(s)
		result := max(val/(1024*1024), 1)
		return result, nil
	}

	val := parseInt64(numStr)
	return val * multiplier, nil
}

func parseInt64(s string) int64 {
	var n int64
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int64(c-'0')
		}
	}
	return n
}
