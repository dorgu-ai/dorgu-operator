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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func newTestEvent(reason, message, eventType string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-event",
			Namespace: "default",
			UID:       types.UID("test-uid-123"),
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "test-pod",
			Namespace: "default",
		},
		Reason:        reason,
		Message:       message,
		Type:          eventType,
		LastTimestamp:  metav1.Time{Time: time.Date(2026, 3, 27, 14, 0, 0, 0, time.UTC)},
		Source:        corev1.EventSource{Component: "kubelet"},
	}
}

func TestClassifier_ClassificationRules(t *testing.T) {
	classifier := NewClassifier()

	tests := []struct {
		name             string
		event            *corev1.Event
		expectedSeverity Severity
		expectedCategory Category
		expectedNil      bool
	}{
		{
			name:             "OOMKilling is critical/resource",
			event:            newTestEvent("OOMKilling", "Container killed due to OOM", corev1.EventTypeWarning),
			expectedSeverity: SeverityCritical,
			expectedCategory: CategoryResource,
		},
		{
			name:             "BackOff (CrashLoopBackOff) is critical/health",
			event:            newTestEvent("BackOff", "Back-off restarting failed container", corev1.EventTypeWarning),
			expectedSeverity: SeverityCritical,
			expectedCategory: CategoryHealth,
		},
		{
			name:             "Failed with ImagePullBackOff is warning/deployment",
			event:            newTestEvent("Failed", "Error: ImagePullBackOff", corev1.EventTypeWarning),
			expectedSeverity: SeverityWarning,
			expectedCategory: CategoryDeployment,
		},
		{
			name:             "Failed with ErrImagePull is warning/deployment",
			event:            newTestEvent("Failed", "Failed to pull image: ErrImagePull", corev1.EventTypeWarning),
			expectedSeverity: SeverityWarning,
			expectedCategory: CategoryDeployment,
		},
		{
			name:             "Evicted is critical/resource",
			event:            newTestEvent("Evicted", "The node was low on resource: memory", corev1.EventTypeWarning),
			expectedSeverity: SeverityCritical,
			expectedCategory: CategoryResource,
		},
		{
			name:             "FailedScheduling is warning/resource",
			event:            newTestEvent("FailedScheduling", "0/3 nodes available", corev1.EventTypeWarning),
			expectedSeverity: SeverityWarning,
			expectedCategory: CategoryResource,
		},
		{
			name:             "NodeNotReady is critical/node",
			event:            newTestEvent("NodeNotReady", "Node status is NotReady", corev1.EventTypeWarning),
			expectedSeverity: SeverityCritical,
			expectedCategory: CategoryNode,
		},
		{
			name:             "MemoryPressure is warning/node",
			event:            newTestEvent("MemoryPressure", "Node has memory pressure", corev1.EventTypeWarning),
			expectedSeverity: SeverityWarning,
			expectedCategory: CategoryNode,
		},
		{
			name:             "DiskPressure is warning/node",
			event:            newTestEvent("DiskPressure", "Node has disk pressure", corev1.EventTypeWarning),
			expectedSeverity: SeverityWarning,
			expectedCategory: CategoryNode,
		},
		{
			name:             "PIDPressure is warning/node",
			event:            newTestEvent("PIDPressure", "Node has PID pressure", corev1.EventTypeWarning),
			expectedSeverity: SeverityWarning,
			expectedCategory: CategoryNode,
		},
		{
			name:             "NetworkUnavailable is critical/node",
			event:            newTestEvent("NetworkUnavailable", "Network not available", corev1.EventTypeWarning),
			expectedSeverity: SeverityCritical,
			expectedCategory: CategoryNode,
		},
		{
			name:             "FailedMount is warning/dependency",
			event:            newTestEvent("FailedMount", "Unable to mount volume", corev1.EventTypeWarning),
			expectedSeverity: SeverityWarning,
			expectedCategory: CategoryDependency,
		},
		{
			name:             "Unhealthy (probe failure) is warning/health",
			event:            newTestEvent("Unhealthy", "Liveness probe failed", corev1.EventTypeWarning),
			expectedSeverity: SeverityWarning,
			expectedCategory: CategoryHealth,
		},
		{
			name:             "ScalingReplicaSet is info/scaling",
			event:            newTestEvent("ScalingReplicaSet", "Scaled up replica set to 3", corev1.EventTypeNormal),
			expectedSeverity: SeverityInfo,
			expectedCategory: CategoryScaling,
		},
		{
			name:             "SuccessfulCreate is info/deployment",
			event:            newTestEvent("SuccessfulCreate", "Created pod: test-pod-abc", corev1.EventTypeNormal),
			expectedSeverity: SeverityInfo,
			expectedCategory: CategoryDeployment,
		},
		{
			name:             "Unknown Warning event falls through to info/health",
			event:            newTestEvent("SomeUnknownReason", "Something happened", corev1.EventTypeWarning),
			expectedSeverity: SeverityInfo,
			expectedCategory: CategoryHealth,
		},
		{
			name:        "Normal event with unknown reason is discarded",
			event:       newTestEvent("SomeNormalReason", "Everything is fine", corev1.EventTypeNormal),
			expectedNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifier.Classify(tt.event)

			if tt.expectedNil {
				assert.Nil(t, result)
				return
			}

			require.NotNil(t, result)
			assert.Equal(t, tt.expectedSeverity, result.Severity)
			assert.Equal(t, tt.expectedCategory, result.Category)
		})
	}
}

func TestClassifier_NilEvent(t *testing.T) {
	classifier := NewClassifier()
	assert.Nil(t, classifier.Classify(nil))
}

func TestClassifier_InternalEventFields(t *testing.T) {
	classifier := NewClassifier()
	event := newTestEvent("OOMKilling", "Container killed", corev1.EventTypeWarning)

	result := classifier.Classify(event)
	require.NotNil(t, result)

	assert.Equal(t, "kubelet", result.Source)
	assert.Equal(t, "Container killed", result.Message)
	assert.Equal(t, "Pod", result.InvolvedObject.Kind)
	assert.Equal(t, "test-pod", result.InvolvedObject.Name)
	assert.Equal(t, "default", result.InvolvedObject.Namespace)
	assert.Equal(t, string(event.UID), result.K8sEventUID)
	assert.NotEmpty(t, result.ID)
	assert.Equal(t, event, result.Raw)
}

func TestClassifier_EventTimeHandling(t *testing.T) {
	classifier := NewClassifier()

	t.Run("uses LastTimestamp when available", func(t *testing.T) {
		event := newTestEvent("OOMKilling", "OOM", corev1.EventTypeWarning)
		result := classifier.Classify(event)
		require.NotNil(t, result)
		assert.Equal(t, event.LastTimestamp.Time, result.EventTime)
	})

	t.Run("falls back to CreationTimestamp", func(t *testing.T) {
		event := newTestEvent("OOMKilling", "OOM", corev1.EventTypeWarning)
		event.LastTimestamp = metav1.Time{}
		event.CreationTimestamp = metav1.Time{Time: time.Date(2026, 3, 27, 15, 0, 0, 0, time.UTC)}

		result := classifier.Classify(event)
		require.NotNil(t, result)
		assert.Equal(t, event.CreationTimestamp.Time, result.EventTime)
	})

	t.Run("falls back to now when both timestamps are zero", func(t *testing.T) {
		event := newTestEvent("OOMKilling", "OOM", corev1.EventTypeWarning)
		event.LastTimestamp = metav1.Time{}
		event.CreationTimestamp = metav1.Time{}

		before := time.Now()
		result := classifier.Classify(event)
		require.NotNil(t, result)
		assert.True(t, result.EventTime.After(before) || result.EventTime.Equal(before))
	})
}

func TestClassifier_FailedEventWithoutImagePull(t *testing.T) {
	classifier := NewClassifier()
	// A "Failed" event that doesn't match ImagePullBackOff or ErrImagePull
	// should fall through to the generic Warning handler.
	event := newTestEvent("Failed", "Some other failure", corev1.EventTypeWarning)

	result := classifier.Classify(event)
	require.NotNil(t, result)
	assert.Equal(t, SeverityInfo, result.Severity)
	assert.Equal(t, CategoryHealth, result.Category)
}

func TestClassifier_GenerateEventID_Deterministic(t *testing.T) {
	event := newTestEvent("OOMKilling", "OOM", corev1.EventTypeWarning)
	id1 := generateEventID(event)
	id2 := generateEventID(event)
	assert.Equal(t, id1, id2, "same event should produce same ID")
}

func TestClassifier_GenerateEventID_Unique(t *testing.T) {
	event1 := newTestEvent("OOMKilling", "OOM", corev1.EventTypeWarning)
	event2 := newTestEvent("BackOff", "BackOff", corev1.EventTypeWarning)
	id1 := generateEventID(event1)
	id2 := generateEventID(event2)
	assert.NotEqual(t, id1, id2, "different events should produce different IDs")
}
