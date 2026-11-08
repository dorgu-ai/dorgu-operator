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
	"slices"
	"testing"
	"time"

	"github.com/dorgu-ai/dorgu-operator/internal/llm"
)

func TestSplitNamespaces(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"payments", []string{"payments"}},
		{"payments, billing ,,ops", []string{"payments", "billing", "ops"}},
	}
	for _, tt := range tests {
		if got := splitNamespaces(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("splitNamespaces(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidate_RejectsNegativePendingTTL(t *testing.T) {
	if err := (operatorConfig{remediationPendingTTL: -time.Second}).validate(); err == nil {
		t.Error("a negative --remediation-pending-ttl must be rejected")
	}
	for _, ttl := range []time.Duration{0, time.Hour, DefaultRemediationPendingTTL} {
		if err := (operatorConfig{remediationPendingTTL: ttl}).validate(); err != nil {
			t.Errorf("ttl %s: unexpected error %v", ttl, err)
		}
	}
}

func TestDefaultRemediationPendingTTLIsTwentyFourHours(t *testing.T) {
	if DefaultRemediationPendingTTL != 24*time.Hour {
		t.Errorf("default = %s, want 24h (D17)", DefaultRemediationPendingTTL)
	}
}

func TestAIDisabledReason(t *testing.T) {
	if got := aiDisabledReason("claude", ""); got == "" {
		t.Error("an LLM provider with an unknown operator namespace must be disabled")
	}
	for _, tt := range []struct{ provider, ns string }{
		{"claude", "dorgu-system"}, // known namespace: AI may run
		{"", ""},                   // no provider: nothing to disable
		{"", "dorgu-system"},
	} {
		if got := aiDisabledReason(tt.provider, tt.ns); got != "" {
			t.Errorf("aiDisabledReason(%q, %q) = %q, want it enabled", tt.provider, tt.ns, got)
		}
	}
}

func TestValidate_RejectsNegativeLLMCacheTTL(t *testing.T) {
	if err := (operatorConfig{llmCacheTTL: -time.Second}).validate(); err == nil {
		t.Error("a negative --llm-cache-ttl must be rejected")
	}
	for _, ttl := range []time.Duration{0, time.Minute, llm.DefaultCacheTTL} {
		if err := (operatorConfig{llmCacheTTL: ttl}).validate(); err != nil {
			t.Errorf("--llm-cache-ttl=%s must be accepted, got %v", ttl, err)
		}
	}
}

func TestDefaultLLMCacheTTLIsThirtyMinutes(t *testing.T) {
	if llm.DefaultCacheTTL != 30*time.Minute {
		t.Errorf("default = %s, want 30m", llm.DefaultCacheTTL)
	}
}
