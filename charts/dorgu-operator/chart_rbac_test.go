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

// Package chart contains helm chart render tests. It lives inside the chart
// directory so `go test ./...` picks it up alongside the rest of the repo.
package chart

import (
	"os/exec"
	"strings"
	"testing"
)

// TestHelmChartRBACIncludesLifecycleCRDs renders the chart with `helm template`
// and asserts the ClusterRole grants verbs on every dorgu.io CRD the runtime
// reconcilers touch. v0.5.2 shipped a chart that was missing
// incidentmemories / remediationactions / dorguevents rules, which silently
// broke the incident + remediation pipeline on Helm-deployed clusters.
func TestHelmChartRBACIncludesLifecycleCRDs(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed on PATH; skipping chart render test")
	}

	cmd := exec.Command("helm", "template", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}

	rendered := string(out)

	// Every rule below must exist in the rendered ClusterRole. We check both
	// the resource and its /status subresource for CRDs the operator writes.
	required := []string{
		`resources: ["incidentmemories"]`,
		`resources: ["incidentmemories/status"]`,
		`resources: ["remediationactions"]`,
		`resources: ["remediationactions/status"]`,
		`resources: ["dorguevents"]`,
	}

	for _, line := range required {
		if !strings.Contains(rendered, line) {
			t.Errorf("rendered chart missing required RBAC rule: %s", line)
		}
	}
}
