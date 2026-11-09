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

// `helm upgrade --reuse-values` reuses the previous release's computed values and
// ignores the new chart's values.yaml entirely. Upgrading 0.12.1 to 0.13.0 that way
// therefore renders the 0.13.0 templates against a values set that has none of the
// 0.13.0 keys: a template that reads `.Values.eventPipeline.enabled` panics on the
// nil parent, and one that reads `.Values.healthCheck.readinessThreshold` renders an
// empty flag value that stops the operator at startup.
package chart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renderAgainstValues renders a copy of this chart whose values.yaml is replaced
// by the 0.12.1 values: the --reuse-values situation, where the chart's own defaults are
// not merged in.
func renderAgainstValues(t *testing.T, sets ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed on PATH; skipping chart render test")
	}
	copyDir := filepath.Join(t.TempDir(), "dorgu-operator")
	if out, err := exec.Command("cp", "-r", ".", copyDir).CombinedOutput(); err != nil {
		t.Fatalf("copy chart: %v\n%s", err, out)
	}
	old, err := os.ReadFile(values0121)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "values.yaml"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"template", "dorgu-operator", copyDir, "-n", "dorgu-system"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	return string(out), err
}

const values0121 = "../../test/chart/values-0.12.1.yaml"

func TestUpgradeFrom0121WithReuseValuesRenders(t *testing.T) {
	out, err := renderAgainstValues(t)
	if err != nil {
		t.Fatalf("0.13.0 templates do not render against the 0.12.1 values (helm upgrade --reuse-values): %v\n%s", err, out)
	}
	// Every new flag is present with the 0.13.0 default, never empty.
	for _, want := range []string{
		"--readiness-threshold=2m", "--liveness-restart-count=2", "--liveness-restart-window=10m",
		"--incident-recurrence-cap=3", "--incident-recurrence-window=24h", "--resolved-incident-retention=720h",
		"--enable-event-pipeline=false", "--remediation-pending-ttl=24h",
	} {
		mustContain(t, out, want, "0.12.1 values, new chart")
	}
	for _, empty := range []string{"=\n", "=<nil>", "<no value>"} {
		mustNotContain(t, out, empty, "no flag may render an empty value")
	}
}

func TestUpgradeFrom0121WithReuseValuesAndAIRendersCacheTTL(t *testing.T) {
	// The 0.12.1 AI install: values the user had set, plus the same --set flags again.
	out, err := renderAgainstValues(t, "llm.provider=claude", "aiRemediation.enabled=true",
		"llm.existingSecret=dorgu-llm", "llm.existingSecretKey=ANTHROPIC_API_KEY")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	mustContain(t, out, "--llm-cache-ttl=30m", "llm.cacheTTL missing from the old values")
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "=") && strings.Contains(line, "--") {
			t.Errorf("a flag renders with an empty value: %q", line)
		}
	}
}

func TestReuseValuesKeepsExplicitZeroes(t *testing.T) {
	// A user who set 0 (disable) keeps 0; the fallback is only for an absent key.
	out, err := renderAgainstValues(t, "healthCheck.recurrence.cap=0", "healthCheck.livenessRestarts.count=0",
		"remediation.pendingTTL=0")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	mustContain(t, out, "--incident-recurrence-cap=0", "explicit zero")
	mustContain(t, out, "--liveness-restart-count=0", "explicit zero")
	mustContain(t, out, "--remediation-pending-ttl=0", "explicit zero")
}

// The in-template fallbacks must not drift from values.yaml: a fresh install and
// an upgrade from 0.12.1 with --reuse-values run the operator with the same flags.
func TestReuseValuesFallbacksMatchValuesYaml(t *testing.T) {
	fresh := helmTemplate(t)
	upgraded, err := renderAgainstValues(t)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, upgraded)
	}
	flags := func(rendered string) []string {
		var out []string
		for line := range strings.SplitSeq(rendered, "\n") {
			if l := strings.TrimSpace(line); strings.HasPrefix(l, "- --") {
				out = append(out, l)
			}
		}
		return out
	}
	a, b := flags(fresh), flags(upgraded)
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("flags differ between a fresh install and a --reuse-values upgrade from 0.12.1:\nfresh:\n%s\nupgraded:\n%s",
			strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
}
