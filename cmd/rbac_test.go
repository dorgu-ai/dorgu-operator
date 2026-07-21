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
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
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
