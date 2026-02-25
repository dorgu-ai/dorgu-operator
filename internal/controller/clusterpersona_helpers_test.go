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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ============================================================================
// getNodeRole tests
// ============================================================================

func TestGetNodeRole_ControlPlane(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "control-plane-1",
			Labels: map[string]string{
				"node-role.kubernetes.io/control-plane": "",
			},
		},
	}

	role := getNodeRole(node)
	assert.Equal(t, "control-plane", role)
}

func TestGetNodeRole_Master(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "master-1",
			Labels: map[string]string{
				"node-role.kubernetes.io/master": "",
			},
		},
	}

	role := getNodeRole(node)
	assert.Equal(t, "control-plane", role)
}

func TestGetNodeRole_Worker(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-1",
			Labels: map[string]string{
				"kubernetes.io/os": "linux",
			},
		},
	}

	role := getNodeRole(node)
	assert.Equal(t, "worker", role)
}

func TestGetNodeRole_NoLabels(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
		},
	}

	role := getNodeRole(node)
	assert.Equal(t, "worker", role)
}

// ============================================================================
// isNodeReady tests
// ============================================================================

func TestIsNodeReady_Ready(t *testing.T) {
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	assert.True(t, isNodeReady(node))
}

func TestIsNodeReady_NotReady(t *testing.T) {
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}

	assert.False(t, isNodeReady(node))
}

func TestIsNodeReady_NoConditions(t *testing.T) {
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{},
		},
	}

	assert.False(t, isNodeReady(node))
}

func TestIsNodeReady_OtherConditionsOnly(t *testing.T) {
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeMemoryPressure,
					Status: corev1.ConditionFalse,
				},
				{
					Type:   corev1.NodeDiskPressure,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}

	assert.False(t, isNodeReady(node))
}

func TestIsNodeReady_ReadyUnknown(t *testing.T) {
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionUnknown,
				},
			},
		},
	}

	assert.False(t, isNodeReady(node))
}

// ============================================================================
// filterNodeLabels tests
// ============================================================================

func TestFilterNodeLabels_KeepsInteresting(t *testing.T) {
	labels := map[string]string{
		"node.kubernetes.io/instance-type": "m5.large",
		"topology.kubernetes.io/zone":      "us-east-1a",
		"kubernetes.io/arch":               "amd64",
		"kubernetes.io/os":                 "linux",
		"some-custom-label":                "value",
	}

	filtered := filterNodeLabels(labels)

	assert.Contains(t, filtered, "node.kubernetes.io/instance-type")
	assert.Contains(t, filtered, "topology.kubernetes.io/zone")
	assert.Contains(t, filtered, "kubernetes.io/arch")
	assert.Contains(t, filtered, "kubernetes.io/os")
	assert.NotContains(t, filtered, "some-custom-label")
}

func TestFilterNodeLabels_FiltersOthers(t *testing.T) {
	labels := map[string]string{
		"app":                  "myapp",
		"environment":          "production",
		"custom.label.io/test": "value",
	}

	filtered := filterNodeLabels(labels)

	assert.Empty(t, filtered)
}

func TestFilterNodeLabels_KeepsRoleLabels(t *testing.T) {
	labels := map[string]string{
		"node-role.kubernetes.io/control-plane": "",
		"node-role.kubernetes.io/worker":        "",
		"custom-role":                           "value",
	}

	filtered := filterNodeLabels(labels)

	assert.Contains(t, filtered, "node-role.kubernetes.io/control-plane")
	assert.Contains(t, filtered, "node-role.kubernetes.io/worker")
	assert.Contains(t, filtered, "custom-role")
}

func TestFilterNodeLabels_Empty(t *testing.T) {
	labels := map[string]string{}

	filtered := filterNodeLabels(labels)

	assert.Empty(t, filtered)
}

func TestFilterNodeLabels_Nil(t *testing.T) {
	filtered := filterNodeLabels(nil)

	assert.Empty(t, filtered)
}

// ============================================================================
// getTaintStrings tests
// ============================================================================

func TestGetTaintStrings_Empty(t *testing.T) {
	taints := []corev1.Taint{}

	result := getTaintStrings(taints)

	assert.Empty(t, result)
}

func TestGetTaintStrings_Multiple(t *testing.T) {
	taints := []corev1.Taint{
		{
			Key:    "node-role.kubernetes.io/control-plane",
			Value:  "",
			Effect: corev1.TaintEffectNoSchedule,
		},
		{
			Key:    "dedicated",
			Value:  "gpu",
			Effect: corev1.TaintEffectPreferNoSchedule,
		},
		{
			Key:    "maintenance",
			Value:  "true",
			Effect: corev1.TaintEffectNoExecute,
		},
	}

	result := getTaintStrings(taints)

	assert.Len(t, result, 3)
	assert.Contains(t, result, "node-role.kubernetes.io/control-plane=:NoSchedule")
	assert.Contains(t, result, "dedicated=gpu:PreferNoSchedule")
	assert.Contains(t, result, "maintenance=true:NoExecute")
}

func TestGetTaintStrings_Single(t *testing.T) {
	taints := []corev1.Taint{
		{
			Key:    "key",
			Value:  "value",
			Effect: corev1.TaintEffectNoSchedule,
		},
	}

	result := getTaintStrings(taints)

	assert.Len(t, result, 1)
	assert.Equal(t, "key=value:NoSchedule", result[0])
}

// ============================================================================
// countReadyNodes tests
// ============================================================================

func TestCountReadyNodes_AllReady(t *testing.T) {
	nodes := []dorguv1.NodeInfo{
		{Name: "node-1", Ready: true},
		{Name: "node-2", Ready: true},
		{Name: "node-3", Ready: true},
	}

	count := countReadyNodes(nodes)
	assert.Equal(t, 3, count)
}

func TestCountReadyNodes_SomeReady(t *testing.T) {
	nodes := []dorguv1.NodeInfo{
		{Name: "node-1", Ready: true},
		{Name: "node-2", Ready: false},
		{Name: "node-3", Ready: true},
	}

	count := countReadyNodes(nodes)
	assert.Equal(t, 2, count)
}

func TestCountReadyNodes_NoneReady(t *testing.T) {
	nodes := []dorguv1.NodeInfo{
		{Name: "node-1", Ready: false},
		{Name: "node-2", Ready: false},
		{Name: "node-3", Ready: false},
	}

	count := countReadyNodes(nodes)
	assert.Equal(t, 0, count)
}

func TestCountReadyNodes_Empty(t *testing.T) {
	nodes := []dorguv1.NodeInfo{}

	count := countReadyNodes(nodes)
	assert.Equal(t, 0, count)
}

// ============================================================================
// determinePhase tests
// ============================================================================

func TestDeterminePhase_NoNodes(t *testing.T) {
	r := &ClusterPersonaReconciler{}
	nodes := []dorguv1.NodeInfo{}
	addons := []dorguv1.AddonInfo{}

	phase := r.determinePhase(nodes, addons)
	assert.Equal(t, clusterPhaseUnknown, phase)
}

func TestDeterminePhase_AllReady(t *testing.T) {
	r := &ClusterPersonaReconciler{}
	nodes := []dorguv1.NodeInfo{
		{Name: "node-1", Ready: true},
		{Name: "node-2", Ready: true},
		{Name: "node-3", Ready: true},
	}
	addons := []dorguv1.AddonInfo{}

	phase := r.determinePhase(nodes, addons)
	assert.Equal(t, clusterPhaseReady, phase)
}

func TestDeterminePhase_SomeReady(t *testing.T) {
	r := &ClusterPersonaReconciler{}
	nodes := []dorguv1.NodeInfo{
		{Name: "node-1", Ready: true},
		{Name: "node-2", Ready: false},
		{Name: "node-3", Ready: true},
	}
	addons := []dorguv1.AddonInfo{}

	phase := r.determinePhase(nodes, addons)
	assert.Equal(t, clusterPhaseDegraded, phase)
}

func TestDeterminePhase_NoneReady(t *testing.T) {
	r := &ClusterPersonaReconciler{}
	nodes := []dorguv1.NodeInfo{
		{Name: "node-1", Ready: false},
		{Name: "node-2", Ready: false},
		{Name: "node-3", Ready: false},
	}
	addons := []dorguv1.AddonInfo{}

	phase := r.determinePhase(nodes, addons)
	assert.Equal(t, clusterPhaseUnknown, phase)
}

func TestDeterminePhase_SingleNodeReady(t *testing.T) {
	r := &ClusterPersonaReconciler{}
	nodes := []dorguv1.NodeInfo{
		{Name: "node-1", Ready: true},
	}
	addons := []dorguv1.AddonInfo{}

	phase := r.determinePhase(nodes, addons)
	assert.Equal(t, clusterPhaseReady, phase)
}

func TestDeterminePhase_SingleNodeNotReady(t *testing.T) {
	r := &ClusterPersonaReconciler{}
	nodes := []dorguv1.NodeInfo{
		{Name: "node-1", Ready: false},
	}
	addons := []dorguv1.AddonInfo{}

	phase := r.determinePhase(nodes, addons)
	assert.Equal(t, clusterPhaseUnknown, phase)
}
