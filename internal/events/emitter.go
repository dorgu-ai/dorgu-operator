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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
)

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

	eventType := corev1.EventTypeNormal
	if event.Severity == SeverityWarning || event.Severity == SeverityCritical {
		eventType = corev1.EventTypeWarning
	}

	message := fmt.Sprintf("[dorgu] %s: %s", event.Severity, event.Message)

	ref := &objectRef{
		gvk:       schema.GroupVersionKind{Kind: event.InvolvedObject.Kind},
		name:      event.InvolvedObject.Name,
		namespace: event.InvolvedObject.Namespace,
	}

	e.recorder.Event(ref, eventType, "DorguDetected", message)

	e.logger.V(1).Info("emitted K8s event",
		"kind", event.InvolvedObject.Kind,
		"name", event.InvolvedObject.Name,
		"severity", event.Severity,
	)

	return nil
}

// objectRef is a minimal runtime.Object used as the "regarding" object
// for the event recorder. It allows emitting events for any resource kind
// without needing the actual object instance.
type objectRef struct {
	gvk       schema.GroupVersionKind
	name      string
	namespace string
}

func (r *objectRef) GetObjectKind() schema.ObjectKind { return r }
func (r *objectRef) DeepCopyObject() runtime.Object {
	return &objectRef{gvk: r.gvk, name: r.name, namespace: r.namespace}
}
func (r *objectRef) SetGroupVersionKind(gvk schema.GroupVersionKind) { r.gvk = gvk }
func (r *objectRef) GroupVersionKind() schema.GroupVersionKind       { return r.gvk }
