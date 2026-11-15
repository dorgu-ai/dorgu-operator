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
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

// C2 review F-1: the operator's RBAC preflight demands a list of permissions, and the
// roles the operator ships must grant every one of them. A role that falls short closes
// the intake on a fresh install, so this runs the preflight's own list against the chart's
// rendered ClusterRole and against the generated config/rbac/role.yaml, with no cluster.

const kindClusterRole = "ClusterRole"

// ruleAllows reports whether one RBAC rule allows verb on group/resource[/subresource],
// for the object called name ("" for a request that names none: list, watch, create).
func ruleAllows(rule rbacv1.PolicyRule, group, resource, subresource, name, verb string) bool {
	has := func(set []string, want string) bool { return slices.Contains(set, want) || slices.Contains(set, "*") }
	if !has(rule.APIGroups, group) || !has(rule.Verbs, verb) {
		return false
	}
	matched := false
	for _, r := range rule.Resources {
		switch {
		case r == "*":
			matched = true
		case subresource == "" && r == resource:
			matched = true
		case subresource != "" && (r == resource+"/"+subresource || r == resource+"/*" || r == "*/"+subresource):
			matched = true
		}
	}
	if !matched {
		return false
	}
	// A rule that names objects allows only requests that name one of them.
	return len(rule.ResourceNames) == 0 || (name != "" && slices.Contains(rule.ResourceNames, name))
}

// roleAllows reports whether any rule of the roles allows the request.
func roleAllows(rules []rbacv1.PolicyRule, group, resource, subresource, name, verb string) bool {
	for _, rule := range rules {
		if ruleAllows(rule, group, resource, subresource, name, verb) {
			return true
		}
	}
	return false
}

// clusterRoleRules returns the rules of the ClusterRole called name in manifests.
func clusterRoleRules(t *testing.T, manifests, nameSuffix string) []rbacv1.PolicyRule {
	t.Helper()
	for doc := range strings.SplitSeq(manifests, "\n---") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil || role.Kind != kindClusterRole {
			continue
		}
		if strings.HasSuffix(role.Name, nameSuffix) {
			return role.Rules
		}
	}
	t.Fatalf("no ClusterRole named *%s in the manifests", nameSuffix)
	return nil
}

// gaps lists every request of the preflight that the rules do not allow.
func gaps(rules []rbacv1.PolicyRule) []string {
	var out []string
	for _, need := range intake.OperatorAccess() {
		names := need.Names
		if len(names) == 0 {
			names = []string{""}
		}
		for _, name := range names {
			for _, verb := range need.Verbs {
				if !roleAllows(rules, need.Group, need.Resource, need.Subresource, name, verb) {
					out = append(out, strings.TrimSpace(verb+" "+need.Resource+"/"+need.Subresource+" "+name))
				}
			}
		}
	}
	return out
}

func TestChartClusterRoleGrantsEverythingTheOperatorPreflightRequires(t *testing.T) {
	rules := clusterRoleRules(t, helmTemplate(t), "-manager")
	if g := gaps(rules); len(g) > 0 {
		t.Errorf("the chart's operator ClusterRole does not allow what the RBAC preflight requires, "+
			"so a fresh install closes the intake:\n  %s", strings.Join(g, "\n  "))
	}
}

func TestGeneratedRoleGrantsEverythingTheOperatorPreflightRequires(t *testing.T) {
	raw, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatalf("reading generated role: %v", err)
	}
	rules := clusterRoleRules(t, string(raw), "manager-role")
	if g := gaps(rules); len(g) > 0 {
		t.Errorf("config/rbac/role.yaml (the +kubebuilder:rbac markers) does not allow what the RBAC "+
			"preflight requires, so a kustomize install closes the intake:\n  %s", strings.Join(g, "\n  "))
	}
}

// The matcher itself: a wrong matcher would make the two tests above pass for the wrong
// reason.
func TestRuleMatcher(t *testing.T) {
	rule := func(groups, resources, names, verbs string) rbacv1.PolicyRule {
		split := func(s string) []string {
			if s == "" {
				return nil
			}
			return strings.Split(s, ",")
		}
		return rbacv1.PolicyRule{
			APIGroups: split(groups), Resources: split(resources), ResourceNames: split(names), Verbs: split(verbs),
		}
	}
	const dg, ra = "dorgu.io", "remediationactions"
	type ask struct{ group, resource, subresource, name, verb string }
	cases := []struct {
		name string
		rule rbacv1.PolicyRule
		ask  ask
		want bool
	}{
		{"plain match", rule(dg, ra, "", "get,delete"), ask{dg, ra, "", "", "delete"}, true},
		{"verb missing", rule(dg, ra, "", "get"), ask{dg, ra, "", "", "delete"}, false},
		{"wrong group", rule("apps", ra, "", "get"), ask{dg, ra, "", "", "get"}, false},
		{"a resource rule skips subresources", rule(dg, ra, "", "update"), ask{dg, ra, "status", "", "update"}, false},
		{"a subresource rule skips the resource", rule(dg, ra+"/status", "", "update"), ask{dg, ra, "", "", "update"}, false},
		{"subresource match", rule(dg, ra+"/status", "", "update"), ask{dg, ra, "status", "", "update"}, true},
		{"wildcards", rule("*", "*", "", "*"), ask{dg, ra, "status", "", "delete"}, true},
		{"resource/* covers subresources", rule(dg, ra+"/*", "", "get"), ask{dg, ra, "status", "", "get"}, true},
		{"named rule allows a named request", rule("g", "r", "a,b", "get"), ask{"g", "r", "", "a", "get"}, true},
		{"named rule refuses another name", rule("g", "r", "a,b", "get"), ask{"g", "r", "", "c", "get"}, false},
		{"named rule refuses a request that names none", rule("g", "r", "a,b", "list"), ask{"g", "r", "", "", "list"}, false},
		{"unnamed rule allows a named request", rule("g", "r", "", "get"), ask{"g", "r", "", "a", "get"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.ask
			if got := ruleAllows(tc.rule, a.group, a.resource, a.subresource, a.name, a.verb); got != tc.want {
				t.Errorf("ruleAllows = %v, want %v", got, tc.want)
			}
		})
	}
}
