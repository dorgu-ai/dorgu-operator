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

// Package chart contains helm chart render tests. This file covers the secure
// Anthropic key injection refactor (WS3): the API key must reach the operator
// via an ANTHROPIC_API_KEY env var sourced from a Secret, never as an inline
// pod-spec arg, and the AI-remediation toggle must map to --enable-ai-remediation.
package chart

import (
	"os/exec"
	"strings"
	"testing"
)

// helmTemplate renders the chart with the given --set overrides and fails the
// test (or skips when helm is absent) rather than returning an error.
func helmTemplate(t *testing.T, sets ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed on PATH; skipping chart render test")
	}
	args := []string{"template", "."}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %v failed: %v\n%s", sets, err, out)
	}
	return string(out)
}

func mustContain(t *testing.T, rendered, want, desc string) {
	t.Helper()
	if !strings.Contains(rendered, want) {
		t.Errorf("%s: expected rendered output to contain %q", desc, want)
	}
}

func mustNotContain(t *testing.T, rendered, unwanted, desc string) {
	t.Helper()
	if strings.Contains(rendered, unwanted) {
		t.Errorf("%s: expected rendered output to NOT contain %q", desc, unwanted)
	}
}

// (a) The raw key is never rendered as an inline arg, even when llm.apiKey is
// set alongside an existingSecret.
func TestLLMKeyNeverInArgs(t *testing.T) {
	out := helmTemplate(t,
		"llm.provider=claude",
		"llm.existingSecret=dorgu-llm",
		"llm.apiKey=sk-should-not-leak",
	)
	mustNotContain(t, out, "--llm-api-key", "inline key arg")
	mustNotContain(t, out, "sk-should-not-leak", "raw key value")
}

// (b) existingSecret renders an ANTHROPIC_API_KEY env via secretKeyRef, with a
// configurable key name.
func TestLLMExistingSecretEnv(t *testing.T) {
	out := helmTemplate(t, "llm.provider=claude", "llm.existingSecret=dorgu-llm")
	mustContain(t, out, "name: ANTHROPIC_API_KEY", "env var name")
	mustContain(t, out, "name: dorgu-llm", "secretKeyRef name")
	mustContain(t, out, "key: ANTHROPIC_API_KEY", "default secret key")

	custom := helmTemplate(t,
		"llm.provider=claude",
		"llm.existingSecret=dorgu-llm",
		"llm.existingSecretKey=MY_KEY",
	)
	mustContain(t, custom, "key: MY_KEY", "custom secret key")
}

// (c) createSecret+apiKey renders a chart-managed Secret referenced by the env;
// existingSecret takes precedence and suppresses the chart-managed Secret.
func TestLLMChartManagedSecret(t *testing.T) {
	out := helmTemplate(t,
		"llm.provider=claude",
		"llm.apiKey=sk-test",
		"llm.createSecret=true",
	)
	mustContain(t, out, "kind: Secret", "chart-managed Secret rendered")
	mustContain(t, out, "name: release-name-dorgu-operator-llm", "chart Secret name")
	mustContain(t, out, `ANTHROPIC_API_KEY: "sk-test"`, "stringData key")
	mustContain(t, out, "name: release-name-dorgu-operator-llm", "env references chart Secret")

	wins := helmTemplate(t,
		"llm.provider=claude",
		"llm.apiKey=sk-test",
		"llm.createSecret=true",
		"llm.existingSecret=dorgu-llm",
	)
	mustNotContain(t, wins, "kind: Secret", "existingSecret suppresses chart Secret")
	mustContain(t, wins, "name: dorgu-llm", "env references existingSecret")
}

// (d) Non-secret provider/model still render as args.
func TestLLMNonSecretArgs(t *testing.T) {
	out := helmTemplate(t, "llm.provider=claude", "llm.model=claude-opus-4-1")
	mustContain(t, out, "--llm-provider=claude", "provider arg")
	mustContain(t, out, "--llm-model=claude-opus-4-1", "model arg")
}

// (e) The AI-remediation toggle maps to --enable-ai-remediation with an explicit
// boolean, gated on a provider being configured (the operator's flag default is
// true, so omitting it when no provider is set keeps the default render clean
// while AI stays off without a provider/key).
func TestAIRemediationToggle(t *testing.T) {
	on := helmTemplate(t, "llm.provider=claude", "aiRemediation.enabled=true")
	mustContain(t, on, "--enable-ai-remediation=true", "toggle on")

	off := helmTemplate(t, "llm.provider=claude")
	mustContain(t, off, "--enable-ai-remediation=false", "toggle off (default) is explicit")

	noProvider := helmTemplate(t)
	mustNotContain(t, noProvider, "--enable-ai-remediation", "no flag without a provider")
}

// (f) The image tag resolves to the chart appVersion when image.tag is unset.
// Read from Chart.yaml rather than hardcoded, so a version bump cannot leave this
// assertion pinned to a stale image — see TestChartVersionMatchesLatestTag.
func TestImageTagMatchesAppVersion(t *testing.T) {
	appVersion := readChartMeta(t).AppVersion
	out := helmTemplate(t)
	mustContain(t, out, "ghcr.io/dorgu-ai/dorgu-operator:"+appVersion, "image tag from appVersion")
}

// (g) With no provider, the default render is clean: no env, no LLM args, no Secret.
func TestDisabledPathIsClean(t *testing.T) {
	out := helmTemplate(t)
	mustNotContain(t, out, "name: ANTHROPIC_API_KEY", "no env block")
	mustNotContain(t, out, "--llm-", "no LLM args")
	mustNotContain(t, out, "kind: Secret", "no Secret")
}
