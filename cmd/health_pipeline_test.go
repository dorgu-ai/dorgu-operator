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
	"context"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

const testOperatorNS = "dorgu-system"

func aiConfig() operatorConfig {
	return operatorConfig{
		llmProvider:         "claude",
		llmAPIKey:           "test-key-never-sent",
		enableAIRemediation: true,
	}
}

func buildForTest(t *testing.T, cfg operatorConfig, operatorNS string) healthPipeline {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := dorguv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return buildHealthPipeline(c, cfg, operatorNS, logr.Discard())
}

func aiProvider(t *testing.T, p healthPipeline) *diagnosis.AIProvider {
	t.Helper()
	for _, dp := range p.DiagnosisProviders {
		if ai, ok := dp.(*diagnosis.AIProvider); ok {
			return ai
		}
	}
	t.Fatalf("no AI diagnosis provider was wired; providers=%d", len(p.DiagnosisProviders))
	return nil
}

// TestBuildHealthPipeline_AIProviderGateProtectsOperatorNamespace is the wiring
// test for the #51 follow-up: it fails if the AI provider is built with a safety
// checker that lacks the operator namespace, which would send dorgu-system
// incidents to the model.
func TestBuildHealthPipeline_AIProviderGateProtectsOperatorNamespace(t *testing.T) {
	p := buildForTest(t, aiConfig(), testOperatorNS)
	ai := aiProvider(t, p)
	ctx := context.Background()

	gate := ai.Gate()
	if gate == nil {
		t.Fatal("AI provider has no gate: it would send nothing, or worse, be mis-wired")
	}
	if !gate.NamespaceProtected(ctx, testOperatorNS) {
		t.Errorf("AI provider's gate does not protect the operator namespace %q", testOperatorNS)
	}
	if !gate.NamespaceProtected(ctx, "kube-system") {
		t.Error("AI provider's gate does not protect kube-system")
	}
	if gate.NamespaceProtected(ctx, "shop") {
		t.Error("AI provider's gate protects an ordinary namespace; the deny-list is over-broad")
	}
}

func TestBuildHealthPipeline_OneDenyListSharedByProviderAndProposer(t *testing.T) {
	cfg := aiConfig()
	cfg.safetyExcludedNamespaces = []string{"payments"}
	p := buildForTest(t, cfg, testOperatorNS)

	if p.SafetyChecker == nil {
		t.Fatal("no safety checker")
	}
	if got := aiProvider(t, p).Gate(); got != diagnosis.NamespaceGate(p.SafetyChecker) {
		t.Error("AI provider is gated by a different checker than the pipeline's safety checker")
	}
	if p.Proposer.Safety() != p.SafetyChecker {
		t.Error("proposer is gated by a different checker than the pipeline's safety checker")
	}

	ctx := context.Background()
	for _, ns := range []string{testOperatorNS, "kube-system", "payments"} {
		if !p.SafetyChecker.NamespaceProtected(ctx, ns) {
			t.Errorf("namespace %q is not protected", ns)
		}
	}
}

func TestBuildHealthPipeline_UnknownOperatorNamespaceDoesNotProtectEmptyName(t *testing.T) {
	p := buildForTest(t, aiConfig(), "")
	ctx := context.Background()
	if !p.SafetyChecker.NamespaceProtected(ctx, "kube-system") {
		t.Error("kube-system must always be protected")
	}
	// An empty namespace is refused by the fail-closed rule, not by being listed.
	if !p.SafetyChecker.NamespaceProtected(ctx, "") {
		t.Error("an undeterminable namespace must be protected (fail closed)")
	}
}

func TestBuildHealthPipeline_AIPlannerWiredIntoProposer(t *testing.T) {
	p := buildForTest(t, aiConfig(), testOperatorNS)
	if p.Planner == nil || p.Proposer.Planner() == nil {
		t.Fatal("AI remediation is enabled with a Claude key: the planner must reach the proposer")
	}
}

func TestBuildHealthPipeline_RulesOnlyWithoutLLM(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")

	tests := map[string]operatorConfig{
		"no provider":           {},
		"provider but no key":   {llmProvider: "claude"},
		"unknown provider":      {llmProvider: "nope", llmAPIKey: "k"},
		"gemini has no planner": {llmProvider: "gemini", llmAPIKey: "k", enableAIRemediation: true},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			p := buildForTest(t, cfg, testOperatorNS)
			if cfg.llmProvider == "gemini" {
				if len(p.DiagnosisProviders) != 2 {
					t.Errorf("gemini with a key should add the AI diagnosis provider, got %d providers",
						len(p.DiagnosisProviders))
				}
			} else if len(p.DiagnosisProviders) != 1 {
				t.Errorf("providers = %d, want only the rule-based one", len(p.DiagnosisProviders))
			}
			if p.Planner != nil || p.Proposer.Planner() != nil {
				t.Error("no Claude key and provider: there must be no AI planner")
			}
			if p.DiagnosisEngine == nil || p.Proposer == nil {
				t.Error("engine and proposer must always be built")
			}
		})
	}
}

func TestBuildHealthPipeline_AIRemediationFlagOffKeepsDiagnosisButNoPlanner(t *testing.T) {
	cfg := aiConfig()
	cfg.enableAIRemediation = false
	p := buildForTest(t, cfg, testOperatorNS)
	_ = aiProvider(t, p)
	if p.Planner != nil {
		t.Error("planner built although --enable-ai-remediation=false")
	}
}

func TestResolveLLMAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "from-env-claude")
	t.Setenv("GEMINI_API_KEY", "from-env-gemini")

	tests := []struct {
		name string
		cfg  operatorConfig
		want string
	}{
		{"flag wins", operatorConfig{llmProvider: "claude", llmAPIKey: "flag"}, "flag"},
		{"claude env", operatorConfig{llmProvider: "claude"}, "from-env-claude"},
		{"gemini env", operatorConfig{llmProvider: "gemini"}, "from-env-gemini"},
		{"unknown provider", operatorConfig{llmProvider: "x"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveLLMAPIKey(tt.cfg); got != tt.want {
				t.Errorf("resolveLLMAPIKey = %q, want %q", got, tt.want)
			}
		})
	}
}
