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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// discoverNodes retrieves information about all cluster nodes.
func (r *ClusterPersonaReconciler) discoverNodes(ctx context.Context) ([]dorguv1.NodeInfo, error) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return nil, err
	}

	nodes := make([]dorguv1.NodeInfo, 0, len(nodeList.Items))
	for _, node := range nodeList.Items {
		nodeInfo := dorguv1.NodeInfo{
			Name:             node.Name,
			Role:             getNodeRole(&node),
			Ready:            isNodeReady(&node),
			KubeletVersion:   node.Status.NodeInfo.KubeletVersion,
			ContainerRuntime: node.Status.NodeInfo.ContainerRuntimeVersion,
			Labels:           filterNodeLabels(node.Labels),
			Taints:           getTaintStrings(node.Spec.Taints),
		}

		// Capacity
		nodeInfo.Capacity = &dorguv1.NodeResources{
			CPU:              node.Status.Capacity.Cpu().String(),
			Memory:           node.Status.Capacity.Memory().String(),
			Pods:             node.Status.Capacity.Pods().String(),
			EphemeralStorage: node.Status.Capacity.StorageEphemeral().String(),
		}

		// Allocatable
		nodeInfo.Allocatable = &dorguv1.NodeResources{
			CPU:              node.Status.Allocatable.Cpu().String(),
			Memory:           node.Status.Allocatable.Memory().String(),
			Pods:             node.Status.Allocatable.Pods().String(),
			EphemeralStorage: node.Status.Allocatable.StorageEphemeral().String(),
		}

		nodes = append(nodes, nodeInfo)
	}

	return nodes, nil
}

// calculateResourceSummary aggregates resource information across all nodes.
func (r *ClusterPersonaReconciler) calculateResourceSummary(ctx context.Context, nodes []dorguv1.NodeInfo) *dorguv1.ClusterResourceSummary {
	summary := &dorguv1.ClusterResourceSummary{}

	var totalCPU, totalMemory, allocatableCPU, allocatableMemory resource.Quantity
	var totalPods int64

	for _, node := range nodes {
		if node.Capacity != nil {
			if cpu, err := resource.ParseQuantity(node.Capacity.CPU); err == nil {
				totalCPU.Add(cpu)
			}
			if mem, err := resource.ParseQuantity(node.Capacity.Memory); err == nil {
				totalMemory.Add(mem)
			}
			if pods, err := resource.ParseQuantity(node.Capacity.Pods); err == nil {
				totalPods += pods.Value()
			}
		}
		if node.Allocatable != nil {
			if cpu, err := resource.ParseQuantity(node.Allocatable.CPU); err == nil {
				allocatableCPU.Add(cpu)
			}
			if mem, err := resource.ParseQuantity(node.Allocatable.Memory); err == nil {
				allocatableMemory.Add(mem)
			}
		}
	}

	summary.TotalCPU = totalCPU.String()
	summary.TotalMemory = totalMemory.String()
	summary.AllocatableCPU = allocatableCPU.String()
	summary.AllocatableMemory = allocatableMemory.String()
	summary.TotalPods = int32(totalPods)

	// Count running pods
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err == nil {
		runningCount := int32(0)
		for _, pod := range podList.Items {
			if pod.Status.Phase == corev1.PodRunning {
				runningCount++
			}
		}
		summary.RunningPods = runningCount
	}

	// Set node count
	summary.NodeCount = int32(len(nodes))

	return summary
}

// discoverNamespaces retrieves namespace information.
func (r *ClusterPersonaReconciler) discoverNamespaces(ctx context.Context) (*dorguv1.NamespaceSummary, error) {
	nsList := &corev1.NamespaceList{}
	if err := r.List(ctx, nsList); err != nil {
		return nil, err
	}

	summary := &dorguv1.NamespaceSummary{
		Total: int32(len(nsList.Items)),
	}

	activeCount := int32(0)
	for _, ns := range nsList.Items {
		if ns.Status.Phase == corev1.NamespaceActive {
			activeCount++
		}
	}
	summary.Active = activeCount

	// Count namespaces with ApplicationPersonas
	personaList := &dorguv1.ApplicationPersonaList{}
	if err := r.List(ctx, personaList); err == nil {
		nsWithPersonas := make(map[string]bool)
		for _, p := range personaList.Items {
			nsWithPersonas[p.Namespace] = true
		}
		summary.WithPersonas = int32(len(nsWithPersonas))
	}

	return summary, nil
}

// countApplicationPersonas counts the number of ApplicationPersonas in the cluster.
func (r *ClusterPersonaReconciler) countApplicationPersonas(ctx context.Context) (int32, error) {
	personaList := &dorguv1.ApplicationPersonaList{}
	if err := r.List(ctx, personaList); err != nil {
		return 0, err
	}
	return int32(len(personaList.Items)), nil
}

// determinePhase determines the overall cluster phase.
// When no nodes are discovered, returns Discovering instead of Unknown
// to prevent transient API failures from regressing a Ready cluster.
func (r *ClusterPersonaReconciler) determinePhase(nodes []dorguv1.NodeInfo, _ []dorguv1.AddonInfo) string {
	if len(nodes) == 0 {
		return clusterPhaseDiscovering
	}

	readyNodes := countReadyNodes(nodes)
	if readyNodes == len(nodes) {
		return clusterPhaseReady
	}

	// Some or all nodes not ready — this is Degraded, not Unknown
	return clusterPhaseDegraded
}

// detectPlatform attempts to identify the Kubernetes platform.
func (r *ClusterPersonaReconciler) detectPlatform(ctx context.Context, nodes []dorguv1.NodeInfo) string {
	if len(nodes) == 0 {
		return "Unknown"
	}

	// Check node labels for platform hints
	for _, node := range nodes {
		for key := range node.Labels {
			switch {
			case strings.Contains(key, "eks.amazonaws.com"):
				return "EKS"
			case strings.Contains(key, "cloud.google.com/gke"):
				return "GKE"
			case strings.Contains(key, "kubernetes.azure.com"):
				return "AKS"
			case strings.Contains(key, "node.openshift.io"):
				return "OpenShift"
			case strings.Contains(key, "minikube.k8s.io"):
				return "Minikube"
			case strings.Contains(key, "kind.x-k8s.io"):
				return "Kind"
			case strings.Contains(key, "k3s.io"):
				return "K3s"
			}
		}
	}

	// Check provider ID
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err == nil && len(nodeList.Items) > 0 {
		providerID := nodeList.Items[0].Spec.ProviderID
		switch {
		case strings.HasPrefix(providerID, "aws://"):
			return "EKS"
		case strings.HasPrefix(providerID, "gce://"):
			return "GKE"
		case strings.HasPrefix(providerID, "azure://"):
			return "AKS"
		case strings.HasPrefix(providerID, "kind://"):
			return "Kind"
		}
	}

	return "Generic"
}

// getNodeRole determines the role of a node based on its labels.
func getNodeRole(node *corev1.Node) string {
	if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return nodeRoleControlPlane
	}
	if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
		return nodeRoleControlPlane
	}
	return nodeRoleWorker
}

// isNodeReady checks if a node is in Ready condition.
func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// filterNodeLabels returns only interesting labels from a node.
func filterNodeLabels(labels map[string]string) map[string]string {
	filtered := make(map[string]string)
	interestingPrefixes := []string{
		"node.kubernetes.io/",
		"topology.kubernetes.io/",
		"kubernetes.io/arch",
		"kubernetes.io/os",
		"node-role.kubernetes.io/",
	}

	for key, value := range labels {
		for _, prefix := range interestingPrefixes {
			if strings.HasPrefix(key, prefix) || strings.Contains(key, "role") {
				filtered[key] = value
				break
			}
		}
	}
	return filtered
}

// getTaintStrings converts taints to string representations.
func getTaintStrings(taints []corev1.Taint) []string {
	result := make([]string, 0, len(taints))
	for _, taint := range taints {
		result = append(result, fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect))
	}
	return result
}

// countReadyNodes counts the number of ready nodes.
func countReadyNodes(nodes []dorguv1.NodeInfo) int {
	count := 0
	for _, node := range nodes {
		if node.Ready {
			count++
		}
	}
	return count
}
