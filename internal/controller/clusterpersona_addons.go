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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// discoverAddons detects installed cluster add-ons.
func (r *ClusterPersonaReconciler) discoverAddons(ctx context.Context) []dorguv1.AddonInfo {
	var addons []dorguv1.AddonInfo

	// To add a new addon detector:
	//   r.checkAddon(ctx, podNameContains, namespace, addonType)
	// Valid addonType values: gitops, monitoring, logging, ingress,
	//   service-mesh, secrets, cert-management, database, other
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

	// Check for CloudNativePG (CNPG) — PostgreSQL operator, dependency for OpenObserve
	cnpg := r.checkAddon(ctx, "cnpg-cloudnative-pg", "cnpg-system", "database")
	addons = append(addons, cnpg)

	return addons
}

// checkAddon checks if a specific add-on is installed by searching for pods
// with the given name in the expected namespace.
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

			// Prefer app.kubernetes.io/version label (set by Helm charts)
			if v, ok := pod.Labels["app.kubernetes.io/version"]; ok && v != "" {
				addon.Version = v
			} else if v := extractVersionFromHelmChartLabel(pod.Labels); v != "" {
				addon.Version = v
			} else if len(pod.Spec.Containers) > 0 {
				image := pod.Spec.Containers[0].Image
				if parts := strings.Split(image, ":"); len(parts) > 1 {
					tag := parts[len(parts)-1]
					if isHexDigest(tag) || tag == "latest" {
						addon.Version = "unknown"
					} else {
						addon.Version = tag
					}
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

// extractVersionFromHelmChartLabel extracts version from the helm.sh/chart label.
// The label format is "<chart-name>-<version>", e.g., "openobserve-0.60.0".
// Returns the version portion, or empty string if label is missing or unparseable.
func extractVersionFromHelmChartLabel(labels map[string]string) string {
	chartLabel, ok := labels["helm.sh/chart"]
	if !ok || chartLabel == "" {
		return ""
	}
	// Find the last hyphen followed by a digit or 'v' + digit — that's where the version starts.
	for i := len(chartLabel) - 1; i >= 0; i-- {
		if chartLabel[i] != '-' || i+1 >= len(chartLabel) {
			continue
		}
		next := chartLabel[i+1]
		if next >= '0' && next <= '9' {
			return chartLabel[i+1:]
		}
		if next == 'v' && i+2 < len(chartLabel) && chartLabel[i+2] >= '0' && chartLabel[i+2] <= '9' {
			return chartLabel[i+1:]
		}
	}
	return ""
}

// isHexDigest returns true if s looks like a hex digest (40+ hex characters
// with no dots or dashes, indicating it's not a semantic version).
func isHexDigest(s string) bool {
	if len(s) < 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
