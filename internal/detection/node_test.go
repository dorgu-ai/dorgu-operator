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

package detection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestNodeCollector_Name(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	c := NewNodeCollector(fake.NewClientBuilder().Build(), logger)
	assert.Equal(t, "node-health-checker", c.Name())
}

func TestNodeCollector_AllHealthy(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse},
				{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	assert.Empty(t, signals)
}

func TestNodeCollector_NotReady(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalNodeNotReady, signals[0].Type)
	assert.Equal(t, SeverityCritical, signals[0].Severity)
	assert.Equal(t, CategoryNode, signals[0].Category)
	assert.Equal(t, "Node", signals[0].Resource.Kind)
	assert.Equal(t, "node-1", signals[0].Resource.Name)
	assert.Contains(t, signals[0].Message, "node-1")
}

func TestNodeCollector_ReadyUnknown(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionUnknown},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalNodeNotReady, signals[0].Type)
	assert.Equal(t, SeverityWarning, signals[0].Severity)
}

func TestNodeCollector_MemoryPressure(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalNodeMemoryPressure, signals[0].Type)
	assert.Equal(t, SeverityWarning, signals[0].Severity)
}

func TestNodeCollector_DiskPressure(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalNodeDiskPressure, signals[0].Type)
	assert.Equal(t, SeverityWarning, signals[0].Severity)
}

func TestNodeCollector_PIDPressure(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodePIDPressure, Status: corev1.ConditionTrue},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalNodePIDPressure, signals[0].Type)
	assert.Equal(t, SeverityWarning, signals[0].Severity)
}

func TestNodeCollector_NetworkUnavailable(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionTrue},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalNodeNetworkDown, signals[0].Type)
	assert.Equal(t, SeverityCritical, signals[0].Severity)
}

func TestNodeCollector_MultipleIssues(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-sick"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	assert.Len(t, signals, 3)
}

func TestNodeCollector_MultipleNodes(t *testing.T) {
	healthy := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-healthy"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	unhealthy := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-unhealthy"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}

	signals := collectNodeSignals(t, healthy, unhealthy)
	require.Len(t, signals, 1)
	assert.Equal(t, "node-unhealthy", signals[0].Resource.Name)
}

func TestNodeCollector_EmptyCluster(t *testing.T) {
	signals := collectNodeSignals(t)
	assert.Empty(t, signals)
}

func TestNodeCollector_Metadata_Zone(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				"topology.kubernetes.io/zone":              "us-east-1a",
				"topology.kubernetes.io/region":            "us-east-1",
				"node-role.kubernetes.io/control-plane":    "",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	require.Len(t, signals, 1)
	assert.Equal(t, "us-east-1a", signals[0].Metadata["zone"])
	assert.Equal(t, "us-east-1", signals[0].Metadata["region"])
	assert.Equal(t, "control-plane", signals[0].Metadata["role"])
}

func TestNodeCollector_DetectedAt_UsesTransitionTime(t *testing.T) {
	// Use a fixed time truncated to seconds (metav1.Time serializes at second precision)
	transitionTime := metav1.NewTime(time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC))
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionFalse,
					LastTransitionTime: transitionTime,
				},
			},
		},
	}

	signals := collectNodeSignals(t, node)
	require.Len(t, signals, 1)
	assert.True(t, transitionTime.Time.Equal(signals[0].DetectedAt),
		"expected %v, got %v", transitionTime.Time, signals[0].DetectedAt)
}

// collectNodeSignals is a test helper that creates a fake client with the given nodes
// and runs the NodeCollector.
func collectNodeSignals(t *testing.T, nodes ...*corev1.Node) []Signal {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	objs := make([]runtime.Object, len(nodes))
	for i, n := range nodes {
		objs[i] = n
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	collector := NewNodeCollector(fakeClient, logger)

	signals, err := collector.Collect(context.Background())
	require.NoError(t, err)
	return signals
}
