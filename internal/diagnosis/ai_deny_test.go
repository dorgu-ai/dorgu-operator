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

package diagnosis

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/llm/llmtest"
)

// fakeGate protects a fixed set of namespaces, and treats the empty namespace
// as protected the way the real deny-list does.
type fakeGate struct {
	protected map[string]bool
	asked     []string
}

func newFakeGate(namespaces ...string) *fakeGate {
	g := &fakeGate{protected: map[string]bool{}}
	for _, ns := range namespaces {
		g.protected[ns] = true
	}
	return g
}

func (g *fakeGate) NamespaceProtected(_ context.Context, namespace string) bool {
	g.asked = append(g.asked, namespace)
	return namespace == "" || g.protected[namespace]
}

// allowAll is a gate that protects nothing, for tests that are about something else.
type allowAll struct{}

func (allowAll) NamespaceProtected(context.Context, string) bool { return false }

func enhancingLLM() *mockLLMClient {
	return &mockLLMClient{response: &llm.DiagnosisResponse{
		Summary:           "model narrative",
		RecommendedAction: actionResourceAdjustment,
	}}
}

func oomSignalIn(namespace string) detection.Signal {
	return detection.Signal{
		Type:     detection.SignalOOMKilled,
		Severity: detection.SeverityCritical,
		Category: detection.CategoryResource,
		Source:   "pod-collector",
		Message:  "Container killed due to OOM",
		Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "worker-abc", Namespace: namespace},
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: "worker", Namespace: namespace,
		},
		DetectedAt: time.Now(),
	}
}

// F-1 (EKS QA 2026-11-06): the deny-list gated the remediation planner but not
// this call, so an incident in the operator's own namespace was still sent to the
// model every cycle and recorded ai-enhanced.
func TestAIProvider_DenyList_NoModelCallForProtectedNamespace(t *testing.T) {
	tests := []struct {
		name      string
		signals   []detection.Signal
		gate      NamespaceGate
		wantCalls int
		wantAI    int
	}{
		{
			name:      "a protected namespace is never sent",
			signals:   []detection.Signal{oomSignalIn("dorgu-system")},
			gate:      newFakeGate("dorgu-system"),
			wantCalls: 0,
			wantAI:    0,
		},
		{
			name:      "an allowed namespace is enhanced as before",
			signals:   []detection.Signal{oomSignalIn("apps")},
			gate:      newFakeGate("dorgu-system"),
			wantCalls: 1,
			wantAI:    1,
		},
		{
			name:      "a pod with no namespace fails closed",
			signals:   []detection.Signal{oomSignalIn("")},
			gate:      newFakeGate("dorgu-system"),
			wantCalls: 0,
			wantAI:    0,
		},
		{
			name:      "no gate configured fails closed",
			signals:   []detection.Signal{oomSignalIn("apps")},
			gate:      nil,
			wantCalls: 0,
			wantAI:    0,
		},
		{
			name: "one protected resource among allowed ones protects the whole diagnosis",
			signals: []detection.Signal{
				oomSignalIn("apps"),
				func() detection.Signal {
					s := oomSignalIn("dorgu-system")
					s.Resource.Name = "other"
					return s
				}(),
			},
			gate:      newFakeGate("dorgu-system"),
			wantCalls: 0,
			wantAI:    0,
		},
		{
			name: "a protected persona namespace protects the diagnosis",
			signals: []detection.Signal{func() detection.Signal {
				s := oomSignalIn("apps")
				s.PersonaRef.Namespace = "dorgu-system"
				return s
			}()},
			gate:      newFakeGate("dorgu-system"),
			wantCalls: 0,
			wantAI:    0,
		},
		{
			name: "a cluster-scoped node is not a namespace and is enhanced",
			signals: []detection.Signal{{
				Type:       detection.SignalNodeNotReady,
				Severity:   detection.SeverityCritical,
				Category:   detection.CategoryNode,
				Source:     "node-collector",
				Message:    "Node ip-10-0-0-1 is not ready",
				Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "ip-10-0-0-1"},
				DetectedAt: time.Now(),
			}},
			gate:      newFakeGate("dorgu-system"),
			wantCalls: 1,
			wantAI:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := enhancingLLM()
			provider := NewAIProvider(mock, tt.gate, logr.Discard())

			diagnoses, err := provider.Diagnose(context.Background(), tt.signals)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mock.calls != tt.wantCalls {
				t.Errorf("LLM calls = %d, want %d", mock.calls, tt.wantCalls)
			}
			if len(diagnoses) != tt.wantAI {
				t.Errorf("AI diagnoses = %d, want %d", len(diagnoses), tt.wantAI)
			}
		})
	}
}

// The incident must still be detected and shown: through the real engine the
// protected namespace's diagnosis survives, rule-based, and never reaches the model.
func TestEngine_DenyList_ProtectedIncidentStaysRuleBased(t *testing.T) {
	mock := enhancingLLM()
	engine := NewEngine(logr.Discard(),
		NewRuleBasedProvider(logr.Discard()),
		NewAIProvider(mock, newFakeGate("dorgu-system"), logr.Discard()),
	)

	diagnoses, err := engine.Analyze(context.Background(), []detection.Signal{oomSignalIn("dorgu-system")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) == 0 {
		t.Fatal("the protected namespace's incident was not diagnosed at all")
	}
	for _, d := range diagnoses {
		if d.Provider != providerNameRuleBased {
			t.Errorf("provider = %q, want %q", d.Provider, providerNameRuleBased)
		}
	}
	if mock.calls != 0 {
		t.Errorf("LLM calls = %d, want 0", mock.calls)
	}
}

// A protected diagnosis must not stop an allowed one in the same cycle from being
// enhanced, and the model must see only the allowed one.
func TestAIProvider_DenyList_AllowedDiagnosisStillEnhancedBesideProtectedOne(t *testing.T) {
	pull := detection.Signal{
		Type:       detection.SignalImagePullBackOff,
		Severity:   detection.SeverityWarning,
		Category:   detection.CategoryDeployment,
		Source:     "pod-collector",
		Message:    "Image pull failed",
		Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "api-1", Namespace: "kube-system"},
		DetectedAt: time.Now(),
	}
	rec := &recordingLLM{}
	provider := NewAIProvider(rec, newFakeGate("kube-system"), logr.Discard())

	diagnoses, err := provider.Diagnose(context.Background(), []detection.Signal{oomSignalIn("apps"), pull})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) != 1 {
		t.Fatalf("AI diagnoses = %d, want 1", len(diagnoses))
	}
	for _, req := range rec.requests {
		if strings.Contains(req.User, "Pod/kube-system/api-1") {
			t.Errorf("the protected namespace's resource reached the model: %q", req.User)
		}
	}
	if len(rec.requests) != 1 {
		t.Errorf("LLM calls = %d, want 1", len(rec.requests))
	}
}

type recordingLLM struct{ requests []llm.Request }

func (r *recordingLLM) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	r.requests = append(r.requests, req)
	return llmtest.DiagnosisReply("narrative", "cause", actionResourceAdjustment, 0.5), nil
}

func (r *recordingLLM) Provider() string { return "recording" }
