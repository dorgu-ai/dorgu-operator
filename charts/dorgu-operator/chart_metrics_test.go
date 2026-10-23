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

package chart

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The metrics endpoint is served over HTTPS behind controller-runtime's
// authentication and authorization filter, which checks every scrape with a
// TokenReview and a SubjectAccessReview. The chart granted neither, shipped no
// Service and no role a scraper could be bound to, so every scrape on a Helm
// install failed with a 500 (EKS QA 2026-10-24, H-1).

var (
	tokenReviewCreate  = grant{"authentication.k8s.io", "tokenreviews", "create"}
	accessReviewCreate = grant{"authorization.k8s.io", "subjectaccessreviews", "create"}
)

func metricsReaderRoles(t *testing.T, manifests string) []string {
	t.Helper()
	var names []string
	for doc := range strings.SplitSeq(manifests, "\n---") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil || role.Kind != "ClusterRole" {
			continue
		}
		for _, rule := range role.Rules {
			for _, url := range rule.NonResourceURLs {
				if url == "/metrics" && len(rule.Verbs) == 1 && rule.Verbs[0] == "get" {
					names = append(names, role.Name)
				}
			}
		}
	}
	return names
}

func metricsServices(t *testing.T, manifests string) []corev1.Service {
	t.Helper()
	var services []corev1.Service
	for doc := range strings.SplitSeq(manifests, "\n---") {
		var svc corev1.Service
		if err := yaml.Unmarshal([]byte(doc), &svc); err != nil || svc.Kind != "Service" {
			continue
		}
		for _, port := range svc.Spec.Ports {
			if port.TargetPort.String() == "metrics" {
				services = append(services, svc)
			}
		}
	}
	return services
}

func TestSecureMetricsCanAuthenticateScrapers(t *testing.T) {
	grants := clusterRoleGrants(t, helmTemplate(t))
	for _, g := range []grant{tokenReviewCreate, accessReviewCreate} {
		if !grants[g] {
			t.Errorf("secure metrics are on by default but the chart does not grant %s", g)
		}
	}
}

func TestMetricsAreScrapableByDefault(t *testing.T) {
	out := helmTemplate(t)
	if roles := metricsReaderRoles(t, out); len(roles) != 1 {
		t.Errorf("want exactly one metrics-reader ClusterRole to bind scrapers to, got %v", roles)
	}
	services := metricsServices(t, out)
	if len(services) != 1 {
		t.Fatalf("want one Service targeting the metrics port, got %d", len(services))
	}
	if port := services[0].Spec.Ports[0].Port; port != 8443 {
		t.Errorf("metrics Service port = %d, want 8443", port)
	}
	if !strings.Contains(out, "name: metrics\n              containerPort: 8443") {
		t.Errorf("manager container does not expose a port named metrics on 8443")
	}
}

func TestInsecureMetricsNeedNoReviewRights(t *testing.T) {
	grants := clusterRoleGrants(t, helmTemplate(t, "metrics.secure=false"))
	for _, g := range []grant{tokenReviewCreate, accessReviewCreate} {
		if grants[g] {
			t.Errorf("metrics.secure=false still grants %s; it is only needed to authenticate scrapers", g)
		}
	}
}

func TestDisabledMetricsShipNothing(t *testing.T) {
	out := helmTemplate(t, "metrics.enabled=false")
	if roles := metricsReaderRoles(t, out); len(roles) != 0 {
		t.Errorf("metrics.enabled=false still renders a metrics-reader role: %v", roles)
	}
	if services := metricsServices(t, out); len(services) != 0 {
		t.Errorf("metrics.enabled=false still renders a metrics Service")
	}
	grants := clusterRoleGrants(t, out)
	if grants[tokenReviewCreate] || grants[accessReviewCreate] {
		t.Errorf("metrics.enabled=false still grants review rights")
	}
}
