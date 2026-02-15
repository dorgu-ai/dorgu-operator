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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	clusterRequeueInterval = 5 * time.Minute

	// ClusterPersona phase values
	clusterPhaseDiscovering = "Discovering"
	clusterPhaseReady       = "Ready"
	clusterPhaseDegraded    = "Degraded"
	clusterPhaseUnknown     = "Unknown"

	// Condition types for ClusterPersona
	conditionTypeDiscovered = "Discovered"
	conditionTypeHealthy    = "Healthy"
)

// ClusterPersonaReconciler reconciles a ClusterPersona object.
type ClusterPersonaReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	DiscoveryClient discovery.DiscoveryInterface
}

// +kubebuilder:rbac:groups=dorgu.io,resources=clusterpersonas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dorgu.io,resources=clusterpersonas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=clusterpersonas/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch

// Reconcile discovers cluster state and updates ClusterPersona status.
func (r *ClusterPersonaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the ClusterPersona
	clusterPersona := &dorguv1.ClusterPersona{}
	if err := r.Get(ctx, req.NamespacedName, clusterPersona); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("Reconciling ClusterPersona", "name", clusterPersona.Spec.Name)

	now := metav1.Now()
	clusterPersona.Status.Phase = clusterPhaseDiscovering
	clusterPersona.Status.LastDiscovery = &now

	// 2. Discover nodes
	nodes, err := r.discoverNodes(ctx)
	if err != nil {
		log.Error(err, "Failed to discover nodes")
		clusterPersona.Status.Phase = clusterPhaseDegraded
		setCondition(&clusterPersona.Status.Conditions, conditionTypeDiscovered, metav1.ConditionFalse,
			"NodeDiscoveryFailed", err.Error())
	} else {
		clusterPersona.Status.Nodes = nodes
	}

	// 3. Calculate resource summary
	resourceSummary := r.calculateResourceSummary(ctx, nodes)
	clusterPersona.Status.ResourceSummary = resourceSummary

	// 4. Discover add-ons
	addons := r.discoverAddons(ctx)
	clusterPersona.Status.Addons = addons

	// 5. Discover namespaces
	namespaceSummary, err := r.discoverNamespaces(ctx)
	if err != nil {
		log.Error(err, "Failed to discover namespaces")
	} else {
		clusterPersona.Status.Namespaces = namespaceSummary
	}

	// 6. Get Kubernetes version
	if r.DiscoveryClient != nil {
		if version, err := r.DiscoveryClient.ServerVersion(); err == nil {
			clusterPersona.Status.KubernetesVersion = version.GitVersion
		}
	}

	// 7. Detect platform
	clusterPersona.Status.Platform = r.detectPlatform(ctx, nodes)

	// 8. Count ApplicationPersonas
	appCount, err := r.countApplicationPersonas(ctx)
	if err != nil {
		log.Error(err, "Failed to count ApplicationPersonas")
	} else {
		clusterPersona.Status.ApplicationCount = appCount
	}

	// 9. Determine overall phase
	clusterPersona.Status.Phase = r.determinePhase(nodes, addons)

	// 10. Set conditions
	if clusterPersona.Status.Phase == clusterPhaseReady {
		setCondition(&clusterPersona.Status.Conditions, conditionTypeDiscovered, metav1.ConditionTrue,
			"DiscoveryComplete", "Cluster state discovered successfully")
		setCondition(&clusterPersona.Status.Conditions, conditionTypeHealthy, metav1.ConditionTrue,
			"AllNodesReady", fmt.Sprintf("%d/%d nodes ready", countReadyNodes(nodes), len(nodes)))
	} else {
		setCondition(&clusterPersona.Status.Conditions, conditionTypeDiscovered, metav1.ConditionTrue,
			"DiscoveryComplete", "Cluster state discovered with issues")
		setCondition(&clusterPersona.Status.Conditions, conditionTypeHealthy, metav1.ConditionFalse,
			"NodesNotReady", fmt.Sprintf("%d/%d nodes ready", countReadyNodes(nodes), len(nodes)))
	}

	// 11. Persist status
	if err := r.Status().Update(ctx, clusterPersona); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("ClusterPersona reconciliation complete",
		"phase", clusterPersona.Status.Phase,
		"nodes", len(nodes),
		"addons", len(addons),
		"apps", clusterPersona.Status.ApplicationCount)

	return ctrl.Result{RequeueAfter: clusterRequeueInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterPersonaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dorguv1.ClusterPersona{}).
		Named("clusterpersona").
		Complete(r)
}

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

	return summary
}

// discoverAddons detects installed cluster add-ons.
func (r *ClusterPersonaReconciler) discoverAddons(ctx context.Context) []dorguv1.AddonInfo {
	var addons []dorguv1.AddonInfo

	// Check for ArgoCD
	argoCD := r.checkAddon(ctx, "argocd", "argocd", "gitops")
	addons = append(addons, argoCD)

	// Check for Prometheus
	prometheus := r.checkAddon(ctx, "prometheus", "monitoring", "monitoring")
	if !prometheus.Installed {
		prometheus = r.checkAddon(ctx, "prometheus-server", "monitoring", "monitoring")
	}
	addons = append(addons, prometheus)

	// Check for Grafana
	grafana := r.checkAddon(ctx, "grafana", "monitoring", "monitoring")
	addons = append(addons, grafana)

	// Check for cert-manager
	certManager := r.checkAddon(ctx, "cert-manager", "cert-manager", "cert-management")
	addons = append(addons, certManager)

	// Check for ingress-nginx
	ingressNginx := r.checkAddon(ctx, "ingress-nginx-controller", "ingress-nginx", "ingress")
	addons = append(addons, ingressNginx)

	// Check for external-secrets
	externalSecrets := r.checkAddon(ctx, "external-secrets", "external-secrets", "secrets")
	addons = append(addons, externalSecrets)

	// Check for Istio
	istio := r.checkAddon(ctx, "istiod", "istio-system", "service-mesh")
	addons = append(addons, istio)

	return addons
}

// checkAddon checks if a specific add-on is installed.
func (r *ClusterPersonaReconciler) checkAddon(ctx context.Context, deploymentName, namespace, addonType string) dorguv1.AddonInfo {
	addon := dorguv1.AddonInfo{
		Name:      deploymentName,
		Type:      addonType,
		Namespace: namespace,
		Installed: false,
	}

	// Check if the namespace exists
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		return addon
	}

	// Check for pods with the addon name
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return addon
	}

	for _, pod := range podList.Items {
		if strings.Contains(pod.Name, deploymentName) || strings.Contains(pod.Name, strings.ReplaceAll(deploymentName, "-", "")) {
			addon.Installed = true
			addon.Namespace = namespace

			// Try to get version from container image
			if len(pod.Spec.Containers) > 0 {
				image := pod.Spec.Containers[0].Image
				if parts := strings.Split(image, ":"); len(parts) > 1 {
					addon.Version = parts[len(parts)-1]
				}
			}

			// Check if healthy
			healthy := pod.Status.Phase == corev1.PodRunning
			addon.Healthy = &healthy
			break
		}
	}

	return addon
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

// countApplicationPersonas counts the number of ApplicationPersonas in the cluster.
func (r *ClusterPersonaReconciler) countApplicationPersonas(ctx context.Context) (int32, error) {
	personaList := &dorguv1.ApplicationPersonaList{}
	if err := r.List(ctx, personaList); err != nil {
		return 0, err
	}
	return int32(len(personaList.Items)), nil
}

// determinePhase determines the overall cluster phase.
func (r *ClusterPersonaReconciler) determinePhase(nodes []dorguv1.NodeInfo, _ []dorguv1.AddonInfo) string {
	if len(nodes) == 0 {
		return clusterPhaseUnknown
	}

	readyNodes := countReadyNodes(nodes)
	if readyNodes == len(nodes) {
		return clusterPhaseReady
	}

	if readyNodes > 0 {
		return clusterPhaseDegraded
	}

	return clusterPhaseUnknown
}

// Helper functions

func getNodeRole(node *corev1.Node) string {
	if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return "control-plane"
	}
	if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
		return "control-plane"
	}
	return "worker"
}

func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func filterNodeLabels(labels map[string]string) map[string]string {
	// Return only interesting labels
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

func getTaintStrings(taints []corev1.Taint) []string {
	result := make([]string, 0, len(taints))
	for _, taint := range taints {
		result = append(result, fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect))
	}
	return result
}

func countReadyNodes(nodes []dorguv1.NodeInfo) int {
	count := 0
	for _, node := range nodes {
		if node.Ready {
			count++
		}
	}
	return count
}
