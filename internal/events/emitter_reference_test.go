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

// F-08: every event the operator emitted was dropped by client-go with
// "Could not construct reference, will not report event: object does not
// implement the common interface for accessing the SelfLink" (77 of them in one
// clean-room run). record.FakeRecorder ignores the object it is handed, so the
// existing emitter tests passed while `kubectl get events` returned nothing.
// These tests exercise the reference resolution the real recorder performs.
package events

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// capturingRecorder records the object it is asked to attach an event to, which
// is what the production recorder resolves into an ObjectReference.
type capturingRecorder struct {
	objects []runtime.Object
}

func (c *capturingRecorder) Event(object runtime.Object, _, _, _ string) {
	c.objects = append(c.objects, object)
}

func (c *capturingRecorder) Eventf(object runtime.Object, _, _, _ string, _ ...any) {
	c.objects = append(c.objects, object)
}

func (c *capturingRecorder) AnnotatedEventf(object runtime.Object, _ map[string]string, _, _, _ string, _ ...any) {
	c.objects = append(c.objects, object)
}

// referenceCase is one involved-object shape the emitter must be able to
// reference.
type referenceCase struct {
	name          string
	involved      dorguv1.ResourceReference
	wantKind      string
	wantNamespace string
	wantAPIGroup  string
}

// The object handed to the recorder must resolve into a valid ObjectReference.
// This is the exact call (client-go's reference.GetReference) that was failing.
func TestEmitter_ReferenceResolves(t *testing.T) {
	tests := []referenceCase{
		{
			name:          "pod",
			involved:      dorguv1.ResourceReference{Kind: "Pod", Name: "report-worker-abc", Namespace: "apps"},
			wantKind:      "Pod",
			wantNamespace: "apps",
			wantAPIGroup:  "v1",
		},
		{
			name:         "cluster-scoped node",
			involved:     dorguv1.ResourceReference{Kind: "Node", Name: "ip-10-0-1-20"},
			wantKind:     "Node",
			wantAPIGroup: "v1",
		},
		{
			name:          "deployment",
			involved:      dorguv1.ResourceReference{Kind: kindDeployment, Name: "web", Namespace: "apps"},
			wantKind:      kindDeployment,
			wantNamespace: "apps",
			wantAPIGroup:  "apps/v1",
		},
		{
			name:          "dorgu persona",
			involved:      dorguv1.ResourceReference{Kind: "ApplicationPersona", Name: "web", Namespace: "apps"},
			wantKind:      "ApplicationPersona",
			wantNamespace: "apps",
			wantAPIGroup:  dorguv1.GroupVersion.String(),
		},
	}

	for _, tt := range tests {
		assertReferenceResolves(t, tt)
	}
}

// The involved object must be identified, or the event is meaningless. An event
// with no kind or no name is refused loudly rather than emitted.
func TestEmitter_RejectsUnidentifiedObject(t *testing.T) {
	tests := []struct {
		name     string
		involved dorguv1.ResourceReference
	}{
		{name: "no kind", involved: dorguv1.ResourceReference{Name: "orphan", Namespace: "apps"}},
		{name: "no name", involved: dorguv1.ResourceReference{Kind: "Pod", Namespace: "apps"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := &capturingRecorder{}
			emitter := NewEmitter(capture, zap.New(zap.UseDevMode(true)))

			err := emitter.Emit(context.Background(), &InternalEvent{
				Severity:       SeverityCritical,
				Category:       CategoryResource,
				Source:         "pod-collector",
				Message:        "Container killed due to OOM",
				InvolvedObject: tt.involved,
				EventTime:      time.Now(),
			})

			require.Error(t, err, "an event with an unidentifiable involved object must not be emitted silently")
			assert.Empty(t, capture.objects, "nothing should be handed to the recorder")
		})
	}
}

// End to end through a real broadcaster and sink: the Event object must actually
// be created, which is what `kubectl get events --field-selector
// reason=DorguDetected` reads.
func TestEmitter_EventReachesTheAPI(t *testing.T) {
	clientset := fake.NewClientset()

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	defer broadcaster.Shutdown()

	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "dorgu-operator"})
	emitter := NewEmitter(recorder, zap.New(zap.UseDevMode(true)))

	require.NoError(t, emitter.Emit(context.Background(), &InternalEvent{
		Severity: SeverityCritical,
		Category: CategoryResource,
		Source:   "pod-collector",
		Message:  "Container killed due to OOM",
		InvolvedObject: dorguv1.ResourceReference{
			Kind:      "Pod",
			Name:      "report-worker-abc",
			Namespace: "apps",
		},
		EventTime: time.Now(),
	}))

	var recorded *corev1.Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, err := clientset.CoreV1().Events("apps").List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		if len(list.Items) > 0 {
			recorded = &list.Items[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	require.NotNil(t, recorded, "no Event was created; kubectl get events would show nothing")
	assert.Equal(t, "DorguDetected", recorded.Reason)
	assert.Equal(t, corev1.EventTypeWarning, recorded.Type)
	assert.Equal(t, "Pod", recorded.InvolvedObject.Kind)
	assert.Equal(t, "report-worker-abc", recorded.InvolvedObject.Name)
	assert.Equal(t, "apps", recorded.InvolvedObject.Namespace)
	assert.Contains(t, recorded.Message, "Container killed due to OOM")
}

// assertReferenceResolves runs one reference-resolution case.
func assertReferenceResolves(t *testing.T, tt referenceCase) {
	t.Helper()

	t.Run(tt.name, func(t *testing.T) {
		capture := &capturingRecorder{}
		emitter := NewEmitter(capture, zap.New(zap.UseDevMode(true)))

		require.NoError(t, emitter.Emit(context.Background(), &InternalEvent{
			Severity:       SeverityCritical,
			Category:       CategoryResource,
			Source:         "pod-collector",
			Message:        "Container killed due to OOM",
			InvolvedObject: tt.involved,
			EventTime:      time.Now(),
		}))
		require.Len(t, capture.objects, 1)

		reference, err := ref.GetReference(scheme.Scheme, capture.objects[0])
		require.NoError(t, err, "client-go could not build a reference; the event would be dropped")
		assert.Equal(t, tt.wantKind, reference.Kind)
		assert.Equal(t, tt.involved.Name, reference.Name)
		assert.Equal(t, tt.wantNamespace, reference.Namespace)
		assert.Equal(t, tt.wantAPIGroup, reference.APIVersion)
	})
}
