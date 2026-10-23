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
	"os"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The operator ships two ClusterRoles that must agree: the Helm chart's, and
// config/rbac/role.yaml, which controller-gen generates from the
// +kubebuilder:rbac markers and which `make deploy` (and the e2e suite) installs.
//
// They drifted once. The control-plane collector reads Leases through the
// cached client, which starts a cluster-wide Lease informer. The chart granted
// that; the markers never did. Nothing noticed while the health check was off
// by default, and once it was on, every kustomize install timed out syncing
// caches and exited after two minutes.
//
// Writes are compared one way only: the chart grants Lease writes cluster-wide
// for leader election, where kustomize uses a namespaced Role instead.

var readVerbs = map[string]bool{"get": true, "list": true, "watch": true}

type grant struct{ group, resource, verb string }

func (g grant) String() string { return g.group + "/" + g.resource + ":" + g.verb }

func clusterRoleGrants(t *testing.T, manifests string) map[grant]bool {
	t.Helper()
	grants := map[grant]bool{}
	for doc := range strings.SplitSeq(manifests, "\n---") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil || role.Kind != "ClusterRole" {
			continue
		}
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				for _, resource := range rule.Resources {
					for _, verb := range rule.Verbs {
						grants[grant{group, resource, verb}] = true
					}
				}
			}
		}
	}
	return grants
}

func missing(want, have map[grant]bool, keep func(grant) bool) []string {
	var out []string
	for g := range want {
		if keep(g) && !have[g] {
			out = append(out, g.String())
		}
	}
	sort.Strings(out)
	return out
}

func TestGeneratedRoleCoversChartReads(t *testing.T) {
	generated, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatalf("reading generated role: %v", err)
	}
	chart := clusterRoleGrants(t, helmTemplate(t))
	markers := clusterRoleGrants(t, string(generated))

	if gaps := missing(chart, markers, func(g grant) bool { return readVerbs[g.verb] }); len(gaps) > 0 {
		t.Errorf("chart ClusterRole reads that the +kubebuilder:rbac markers do not grant "+
			"(kustomize installs will fail to sync caches):\n  %s", strings.Join(gaps, "\n  "))
	}
}

// The kustomize install also binds metrics-auth-role so the secure metrics
// endpoint can authenticate scrapers; the chart has to grant the same.
func TestChartCoversGeneratedRole(t *testing.T) {
	var generated []byte
	for _, file := range []string{"../../config/rbac/role.yaml", "../../config/rbac/metrics_auth_role.yaml"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		generated = append(generated, append([]byte("\n---\n"), raw...)...)
	}
	chart := clusterRoleGrants(t, helmTemplate(t))
	markers := clusterRoleGrants(t, string(generated))

	if gaps := missing(markers, chart, func(grant) bool { return true }); len(gaps) > 0 {
		t.Errorf("+kubebuilder:rbac grants missing from the chart ClusterRole "+
			"(Helm installs would lack them):\n  %s", strings.Join(gaps, "\n  "))
	}
}
