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

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// hasRule reports whether the ClusterRole grants every verb in wantVerbs on the
// given apiGroup/resource.
func hasRule(role rbacv1.ClusterRole, apiGroup, resource string, wantVerbs ...string) bool {
	for _, rule := range role.Rules {
		if !contains(rule.APIGroups, apiGroup) || !contains(rule.Resources, resource) {
			continue
		}
		all := true
		for _, v := range wantVerbs {
			if !contains(rule.Verbs, v) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

// TestGeneratedRole_CoversEventsAndPodMetrics locks the WS8 F1 RBAC additions:
// the generated manager ClusterRole must be able to list/watch core events (the
// event watcher) and list pods in metrics.k8s.io (the metrics-usage checker).
func TestGeneratedRole_CoversEventsAndPodMetrics(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "config", "rbac", "role.yaml"))
	require.NoError(t, err, "generated role.yaml must exist (run make manifests)")

	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &role))

	assert.True(t, hasRule(role, "", "events", "get", "list", "watch"),
		"core events need get/list/watch for the event watcher")
	assert.True(t, hasRule(role, "metrics.k8s.io", "pods", "get", "list"),
		"pods.metrics.k8s.io need get/list for the metrics-usage checker")
}

// TestGeneratedRole_CoversReplicaSets locks the WS9 R2 RBAC addition: the
// generated manager ClusterRole must be able to read ReplicaSets, which the
// event correlator and pod->ReplicaSet->Deployment ownership walk both list.
func TestGeneratedRole_CoversReplicaSets(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "config", "rbac", "role.yaml"))
	require.NoError(t, err, "generated role.yaml must exist (run make manifests)")

	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &role))

	assert.True(t, hasRule(role, "apps", "replicasets", "get", "list", "watch"),
		"apps/replicasets need get/list/watch for the ReplicaSet correlator")
}

// TestGeneratedRole_CanPruneItsOwnEvents locks the CF5-2 F-10 RBAC addition. The
// cleaner had no delete permission, so 187 DorguEvent records piled up in etcd
// with nothing able to remove them. The grant covers dorgu.io/dorguevents only.
func TestGeneratedRole_CanPruneItsOwnEvents(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "config", "rbac", "role.yaml"))
	require.NoError(t, err, "generated role.yaml must exist (run make manifests)")

	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &role))

	assert.True(t, hasRule(role, "dorgu.io", "dorguevents", "create", "get", "list", "watch", "delete"),
		"dorguevents need delete so the retention and cap cleaner can prune them")
}

// TestGeneratedRole_CannotDeleteWorkloads is the other half of the trust claim
// the ClusterRole carries: gaining delete on dorgu's own CRD must not have
// widened anything a user's workloads live in. This test is the reason the F-10
// grant can be named in the release notes without qualification.
func TestGeneratedRole_CannotDeleteWorkloads(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "config", "rbac", "role.yaml"))
	require.NoError(t, err, "generated role.yaml must exist (run make manifests)")

	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &role))

	workloads := []struct{ apiGroup, resource string }{
		{"apps", "deployments"},
		{"apps", "replicasets"},
		{"", "pods"},
		{"", "nodes"},
		{"", "namespaces"},
	}

	for _, w := range workloads {
		for _, verb := range []string{"create", "update", "patch", "delete"} {
			assert.False(t, hasRule(role, w.apiGroup, w.resource, verb),
				"the operator must not be able to %s %s/%s", verb, w.apiGroup, w.resource)
		}
	}
}

// TestChartRBAC_MatchesDorguEventDelete ensures the hand-maintained Helm
// ClusterRole mirrors the generated role's delete grant, so a Helm-installed
// release can prune its own records rather than logging a permission error every
// cleanup cycle.
func TestChartRBAC_MatchesDorguEventDelete(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "charts", "dorgu-operator", "templates", "rbac.yaml"))
	require.NoError(t, err, "chart rbac.yaml must exist")

	assert.Contains(t, string(data), `resources: ["dorguevents"]
    verbs: ["create", "get", "list", "watch", "delete"]`,
		"chart dorguevents rule must grant delete for the cleaner")
}

// TestChartRBAC_MatchesReplicaSets ensures the hand-maintained Helm ClusterRole
// mirrors the generated role's WS9 replicasets grant, so an installed release
// carries the same permission as `make deploy`.
func TestChartRBAC_MatchesReplicaSets(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "charts", "dorgu-operator", "templates", "rbac.yaml"))
	require.NoError(t, err, "chart rbac.yaml must exist")

	body := string(data)
	assert.Contains(t, body, `apiGroups: ["apps"]`,
		"chart ClusterRole must grant the apps API group")
	assert.Contains(t, body, `resources: ["deployments", "replicasets"]`,
		"chart apps rule must include replicasets alongside deployments")
	assert.Contains(t, body, `verbs: ["get", "list", "watch"]`,
		"replicasets grant must be read-only get/list/watch")
}

// C4 (P2-D11): CPU throttling reads metrics.k8s.io. It must never be given a kubelet
// path to read cAdvisor with: nodes/proxy is an exec path into every pod on the node.
func TestRBAC_NeverGrantsKubeletAccess(t *testing.T) {
	forbidden := func(resource string) bool {
		return strings.HasPrefix(resource, "nodes/") || resource == "*" || strings.HasSuffix(resource, "/proxy")
	}

	data, err := os.ReadFile(filepath.Join("..", "config", "rbac", "role.yaml"))
	require.NoError(t, err)
	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &role))
	for _, rule := range role.Rules {
		for _, resource := range rule.Resources {
			assert.False(t, forbidden(resource), "generated role grants %v on %q", rule.Verbs, resource)
		}
	}

	roles := chartClusterRoles(t)
	require.NotEmpty(t, roles, "no chart ClusterRole could be parsed, so this test inspected nothing (it used to pass vacuously on {{ }} directives)")
	for _, cr := range roles {
		require.NotEmpty(t, cr.Rules, "chart ClusterRole %q parsed with no rules", cr.Name)
		for _, rule := range cr.Rules {
			for _, resource := range rule.Resources {
				assert.False(t, forbidden(resource), "chart role %q grants %v on %q", cr.Name, rule.Verbs, resource)
			}
		}
	}
}

var clusterRoleKind = regexp.MustCompile(`(?m)^kind: ClusterRole$`)

// chartClusterRoles parses the ClusterRoles in the chart's rbac template. The template
// carries {{ }} directives, which are not YAML, so every line holding one is dropped
// first: the rules themselves are literal. A document that still does not parse fails
// the test instead of being skipped.
func chartClusterRoles(t *testing.T) []rbacv1.ClusterRole {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "charts", "dorgu-operator", "templates", "rbac.yaml"))
	require.NoError(t, err)
	var kept []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.Contains(line, "{{") {
			kept = append(kept, line)
		}
	}
	var roles []rbacv1.ClusterRole
	for doc := range strings.SplitSeq(strings.Join(kept, "\n"), "\n---") {
		if !clusterRoleKind.MatchString(doc) {
			continue
		}
		var cr rbacv1.ClusterRole
		require.NoError(t, yaml.Unmarshal([]byte(doc), &cr), "chart ClusterRole document does not parse:\n%s", doc)
		if cr.Kind == "ClusterRole" {
			roles = append(roles, cr)
		}
	}
	return roles
}

// The pod metrics grant is read-only and exactly get/list, in both install paths.
func TestRBAC_PodMetricsAreReadOnly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "config", "rbac", "role.yaml"))
	require.NoError(t, err)
	var role rbacv1.ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &role))
	assert.True(t, hasRule(role, "metrics.k8s.io", "pods", "get", "list"))
	for _, verb := range []string{"create", "update", "patch", "delete", "watch"} {
		assert.False(t, hasRule(role, "metrics.k8s.io", "pods", verb), "metrics.k8s.io pods must not grant %s", verb)
	}

	chart, err := os.ReadFile(filepath.Join("..", "charts", "dorgu-operator", "templates", "rbac.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(chart), `apiGroups: ["metrics.k8s.io"]
    resources: ["pods"]
    verbs: ["get", "list"]`)
	assert.NotContains(t, string(chart), "nodes/proxy")
	assert.NotContains(t, string(chart), "nodes/metrics")
}
