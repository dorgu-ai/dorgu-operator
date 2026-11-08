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

	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/websocket"
)

// Notifier receives the lifecycle events the health-check reconciler publishes
// as it opens, updates and resolves incidents and proposes remediations. It is
// the one seam between the reconcile cycle and whatever carries those events to
// a client, so the cycle never names a transport.
//
// *websocket.Server satisfies it. A nil Notifier means nobody is listening;
// callers use HealthCheckReconciler.notifier, which also covers the legacy
// WebSocket field.
type Notifier interface {
	BroadcastIncident(event websocket.IncidentEvent)
	BroadcastRemediation(event websocket.RemediationEvent)
	BroadcastHealthUpdate(event websocket.HealthUpdateEvent)
}

var _ Notifier = (*websocket.Server)(nil)

// notifier returns the configured Notifier, or nil when nothing is listening.
// An explicit Notifier wins over the WebSocket field. The WebSocket field is
// checked against nil before it is converted, so a nil *websocket.Server never
// becomes a non-nil interface.
func (r *HealthCheckReconciler) notifier() Notifier {
	if r.Notifier != nil {
		return r.Notifier
	}
	if r.WebSocket != nil {
		return r.WebSocket
	}
	return nil
}

// broadcastHealthSummary collects aggregate health data and broadcasts it.
func (r *HealthCheckReconciler) broadcastHealthSummary(ctx context.Context, n Notifier) {
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
			if pendingRemedies.Items[i].Status.Phase == RemediationPhasePending {
				pendingCount++
			}
		}
	}

	n.BroadcastHealthUpdate(websocket.HealthUpdateEvent{
		EventType:       "health-update",
		ActiveIncidents: activeCount,
		PendingRemedies: pendingCount,
	})
}
