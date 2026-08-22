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

package events

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ReasonDorguDetected is the K8s Event reason for a signal dorgu detected in the
// cluster. It is the default for any event that does not name its own reason.
const ReasonDorguDetected = "DorguDetected"

// Emitter emits standard K8s Events from the operator so that
// dorgu detections are visible via `kubectl describe`.
type Emitter interface {
	Emit(ctx context.Context, event *InternalEvent) error
}

// K8sEventEmitter emits K8s Events using a controller-runtime EventRecorder.
type K8sEventEmitter struct {
	recorder record.EventRecorder
	logger   logr.Logger
}

// NewEmitter creates a new K8sEventEmitter.
func NewEmitter(recorder record.EventRecorder, logger logr.Logger) *K8sEventEmitter {
	return &K8sEventEmitter{
		recorder: recorder,
		logger:   logger.WithName("event-emitter"),
	}
}

// Emit creates a standard K8s Event for the involved object.
func (e *K8sEventEmitter) Emit(_ context.Context, event *InternalEvent) error {
	if event == nil {
		return nil
	}

	ref, err := involvedObjectRef(event.InvolvedObject)
	if err != nil {
		return fmt.Errorf("emitting %q event: %w", event.Source, err)
	}

	eventType := corev1.EventTypeNormal
	if event.Severity == SeverityWarning || event.Severity == SeverityCritical {
		eventType = corev1.EventTypeWarning
	}

	message := fmt.Sprintf("[dorgu] %s: %s", event.Severity, event.Message)

	reason := event.Reason
	if reason == "" {
		reason = ReasonDorguDetected
	}

	e.recorder.Event(ref, eventType, reason, message)

	e.logger.V(1).Info("emitted K8s event",
		"kind", ref.Kind,
		"name", ref.Name,
		"namespace", ref.Namespace,
		"severity", event.Severity,
	)

	return nil
}

// involvedObjectRef builds the "regarding" object for the event recorder.
//
// It must be a corev1.ObjectReference. The recorder resolves whatever it is
// handed through client-go's reference.GetReference, which requires a real API
// object (one exposing ObjectMeta) and returns an ObjectReference untouched.
// A hand-rolled runtime.Object that only carries a GVK, a name and a namespace
// satisfies neither, so every event was dropped with "object does not implement
// the common interface for accessing the SelfLink" and nothing ever reached
// `kubectl get events` (F-08).
//
// Kind and Name are required: an event whose involved object cannot be named is
// not worth emitting, and the API server would reject or misfile it.
func involvedObjectRef(obj dorguv1.ResourceReference) (*corev1.ObjectReference, error) {
	if obj.Kind == "" || obj.Name == "" {
		return nil, fmt.Errorf("involved object needs both a kind and a name, got kind=%q name=%q",
			obj.Kind, obj.Name)
	}

	return &corev1.ObjectReference{
		Kind:       obj.Kind,
		Name:       obj.Name,
		Namespace:  obj.Namespace,
		APIVersion: apiVersionForKind(obj.Kind),
	}, nil
}

// apiVersionForKind maps the kinds dorgu attaches events to onto their
// apiVersion. An unknown kind yields an empty apiVersion, which still produces a
// usable event (kind, namespace and name are what alerting pipelines select on).
func apiVersionForKind(kind string) string {
	switch kind {
	case "Pod", "Node", "Service", "ComponentStatus", "Event", "Namespace", "PersistentVolumeClaim":
		return "v1"
	case kindDeployment, "ReplicaSet", "StatefulSet", "DaemonSet":
		return "apps/v1"
	case "Lease":
		return "coordination.k8s.io/v1"
	case "ApplicationPersona", "ClusterPersona", "IncidentMemory", "RemediationAction", "DorguEvent":
		return dorguv1.GroupVersion.String()
	default:
		return ""
	}
}
