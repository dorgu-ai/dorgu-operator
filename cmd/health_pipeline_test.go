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
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
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
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return buildHealthPipeline(c, c, cfg, operatorNS, logr.Discard())
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

	g := ai.Gate()
	if g == nil {
		t.Fatal("AI provider has no gate: it would send nothing, or worse, be mis-wired")
	}
	if !g.NamespaceProtected(ctx, testOperatorNS) {
		t.Errorf("AI provider's gate does not protect the operator namespace %q", testOperatorNS)
	}
	if !g.NamespaceProtected(ctx, "kube-system") {
		t.Error("AI provider's gate does not protect kube-system")
	}
	if g.NamespaceProtected(ctx, "shop") {
		t.Error("AI provider's gate protects an ordinary namespace; the deny-list is over-broad")
	}
}

func TestBuildHealthPipeline_OneDenyListSharedByProviderAndProposer(t *testing.T) {
	cfg := aiConfig()
	cfg.safetyExcludedNamespaces = []string{"payments"}
	p := buildForTest(t, cfg, testOperatorNS)

	if p.Gate == nil {
		t.Fatal("no gate")
	}
	if got := aiProvider(t, p).Gate(); got != diagnosis.NamespaceGate(p.Gate) {
		t.Error("AI provider is gated by a different gate than the pipeline's gate")
	}
	if p.Proposer.Gate() != p.Gate {
		t.Error("proposer is gated by a different gate than the pipeline's gate")
	}

	ctx := context.Background()
	for _, ns := range []string{testOperatorNS, "kube-system", "payments"} {
		if !p.Gate.NamespaceProtected(ctx, ns) {
			t.Errorf("namespace %q is not protected", ns)
		}
	}
}

// TestBuildHealthPipeline_ProviderAndProposerShareOneGateInstance is design answer
// 2 of the B1 review: the AI diagnosis provider and the proposer are handed the
// SAME *gate.Gate, not two gates that happen to be configured alike. A second gate
// built without the operator namespace, or one that missed an exclusion, would
// let one path through that the other refuses.
func TestBuildHealthPipeline_ProviderAndProposerShareOneGateInstance(t *testing.T) {
	p := buildForTest(t, aiConfig(), testOperatorNS)

	providerGate, ok := aiProvider(t, p).Gate().(*gate.Gate)
	if !ok {
		t.Fatalf("the AI provider's gate is a %T, want the pipeline's *gate.Gate", aiProvider(t, p).Gate())
	}
	if providerGate != p.Proposer.Gate() {
		t.Error("the AI provider and the proposer hold different gate instances")
	}
	if providerGate != p.Gate {
		t.Error("the providers' gate is not the pipeline's gate")
	}
}

// R12: with the operator namespace unknown the pipeline's gate refuses every
// proposal, rule-based included, and a known namespace leaves proposals alone.
func TestBuildHealthPipeline_UnknownOperatorNamespaceBlocksProposals(t *testing.T) {
	ctx := context.Background()
	cand := gate.Candidate{
		Target:    dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "web", Namespace: "shop"},
		Namespace: "shop",
		Action:    dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification},
	}

	unknown := buildForTest(t, aiConfig(), "")
	dec, err := unknown.Gate.Run(ctx, cand, gate.PhaseProposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, guardrail := dec.RefusedBy(); dec.Allowed() || guardrail != gate.GuardrailOperatorNamespace {
		t.Errorf("unknown operator namespace: allowed=%v guardrail=%q, want a refusal by %q",
			dec.Allowed(), guardrail, gate.GuardrailOperatorNamespace)
	}
	if !unknown.Gate.NamespaceProtected(ctx, "shop") {
		t.Error("an ordinary namespace must read as protected while the operator namespace is unknown")
	}

	known := buildForTest(t, aiConfig(), testOperatorNS)
	if known.Gate.NamespaceProtected(ctx, "shop") {
		t.Error("a known operator namespace must not block ordinary namespaces")
	}
}

func TestBuildHealthPipeline_UnknownOperatorNamespaceDoesNotProtectEmptyName(t *testing.T) {
	p := buildForTest(t, aiConfig(), "")
	ctx := context.Background()
	if !p.Gate.NamespaceProtected(ctx, "kube-system") {
		t.Error("kube-system must always be protected")
	}
	// An empty namespace is refused by the fail-closed rule, not by being listed.
	if !p.Gate.NamespaceProtected(ctx, "") {
		t.Error("an undeterminable namespace must be protected (fail closed)")
	}
}

func TestBuildHealthPipeline_AIPlannerWiredIntoProposer(t *testing.T) {
	p := buildForTest(t, aiConfig(), testOperatorNS)
	if p.Planner == nil || p.Proposer.Planner() == nil {
		t.Fatal("AI remediation is enabled with a Claude key: the planner must reach the proposer")
	}
}

// The planner is wired behind the fingerprint cache, so an unchanged situation
// costs one planning call however many cycles it lasts (KPI Q3).
func TestBuildHealthPipeline_PlannerSitsBehindTheFingerprintCache(t *testing.T) {
	cfg := aiConfig()
	cfg.llmCacheTTL = 30 * time.Minute
	p := buildForTest(t, cfg, testOperatorNS)

	if _, ok := p.Planner.(*planner.CachedPlanner); !ok {
		t.Fatalf("planner is %T, want *planner.CachedPlanner", p.Planner)
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

// F-13 wiring: the pod collector main builds skips the namespaces the pipeline's
// gate refuses, every one of them. It fails if main builds the collector without
// the gate, which would send platform-namespace incidents back to the user.
func TestNewPodCollector_SharesTheGatesDenyList(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dorguv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	oomed := func(ns string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "main",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
			}}},
		}
	}
	cp := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "dorgu-cluster"},
		Spec: dorguv1.ClusterPersonaSpec{Policies: &dorguv1.ClusterPolicies{
			SelfHealing: &dorguv1.SelfHealingPolicy{ExcludeNamespaces: []string{"cert-manager"}},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp, oomed("cert-manager"), oomed(testOperatorNS), oomed("kube-system"), oomed("shop")).Build()
	cfg := operatorConfig{safetyExcludedNamespaces: []string{"gateway"}}
	p := buildHealthPipeline(c, cfg, testOperatorNS, logr.Discard())

	signals, err := newPodCollector(c, p.Gate, logr.Discard()).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0].Resource.Namespace != "shop" {
		t.Fatalf("only the OOM in shop may be detected, got %d signals: %+v", len(signals), signals)
	}
}
