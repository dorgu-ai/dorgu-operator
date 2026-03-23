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

	// Node role values
	nodeRoleControlPlane = "control-plane"
	nodeRoleWorker       = "worker"

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
		setCondition(&clusterPersona.Status.Conditions, conditionTypeDiscovered, metav1.ConditionFalse,
			"NodeDiscoveryFailed", err.Error())
		// Preserve existing nodes to avoid phase regression on transient failures
		nodes = clusterPersona.Status.Nodes
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

// discoverAddons detects installed cluster add-ons.
func (r *ClusterPersonaReconciler) discoverAddons(ctx context.Context) []dorguv1.AddonInfo {
	var addons []dorguv1.AddonInfo

	// To add a new addon detector:
	//   r.checkAddon(ctx, podNameContains, namespace, addonType)
	// Valid addonType values: gitops, monitoring, logging, ingress,
	//   service-mesh, secrets, cert-management, other
	// podNameContains: a substring of the main component's pod name

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

	// Check for OpenObserve (installed by 'dorgu cluster setup')
	openObserve := r.checkAddon(ctx, "openobserve", "openobserve", "monitoring")
	addons = append(addons, openObserve)

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
