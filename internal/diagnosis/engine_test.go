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
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// mockProvider is a test DiagnosisProvider.
type mockProvider struct {
	name      string
	diagnoses []Diagnosis
	err       error
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Diagnose(_ context.Context, _ []detection.Signal) ([]Diagnosis, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.diagnoses, nil
}

func TestEngine_EmptySignals(t *testing.T) {
	engine := NewEngine(logr.Discard(), &mockProvider{name: "test"})
	diagnoses, err := engine.Analyze(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) != 0 {
		t.Errorf("expected 0 diagnoses for empty signals, got %d", len(diagnoses))
	}
}

func TestEngine_SingleProvider(t *testing.T) {
	provider := &mockProvider{
		name: "test-provider",
		diagnoses: []Diagnosis{
			{Summary: "test diagnosis", Confidence: 0.85, Provider: "test-provider"},
		},
	}
	engine := NewEngine(logr.Discard(), provider)

	signals := []detection.Signal{
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "test-pod", Namespace: "default"},
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := engine.Analyze(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) != 1 {
		t.Fatalf("expected 1 diagnosis, got %d", len(diagnoses))
	}
	if diagnoses[0].Summary != "test diagnosis" {
		t.Errorf("summary = %q, want %q", diagnoses[0].Summary, "test diagnosis")
	}
}

func TestEngine_MultipleProviders(t *testing.T) {
	p1 := &mockProvider{
		name: "rule-based",
		diagnoses: []Diagnosis{
			{Summary: "rule-based diagnosis", Confidence: 0.70, Provider: "rule-based"},
		},
	}
	p2 := &mockProvider{
		name: "ai-enhanced",
		diagnoses: []Diagnosis{
			{Summary: "ai-enhanced diagnosis", Confidence: 0.90, Provider: "ai-enhanced"},
		},
	}
	engine := NewEngine(logr.Discard(), p1, p2)

	signals := []detection.Signal{
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "test-pod", Namespace: "default"},
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := engine.Analyze(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) != 2 {
		t.Fatalf("expected 2 diagnoses, got %d", len(diagnoses))
	}
	// Should be sorted by confidence descending.
	if diagnoses[0].Confidence < diagnoses[1].Confidence {
		t.Errorf("diagnoses not sorted: [0]=%v < [1]=%v", diagnoses[0].Confidence, diagnoses[1].Confidence)
	}
	if diagnoses[0].Provider != "ai-enhanced" {
		t.Errorf("highest confidence diagnosis provider = %q, want %q", diagnoses[0].Provider, "ai-enhanced")
	}
}

func TestEngine_ProviderError(t *testing.T) {
	provider := &mockProvider{
		name: "failing-provider",
		err:  errors.New("provider failed"),
	}
	engine := NewEngine(logr.Discard(), provider)

	signals := []detection.Signal{
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "test-pod", Namespace: "default"},
			DetectedAt: time.Now(),
		},
	}

	_, err := engine.Analyze(context.Background(), signals)
	if err == nil {
		t.Fatal("expected error from failing provider")
	}
	if !errors.Is(err, provider.err) {
		t.Errorf("error should wrap provider error, got: %v", err)
	}
}

func TestEngine_Providers(t *testing.T) {
	p1 := &mockProvider{name: "a"}
	p2 := &mockProvider{name: "b"}
	engine := NewEngine(logr.Discard(), p1, p2)

	providers := engine.Providers()
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}
	if providers[0].Name() != "a" || providers[1].Name() != "b" {
		t.Error("providers not in expected order")
	}
}

func TestEngine_IntegrationWithRuleBasedProvider(t *testing.T) {
	provider := NewRuleBasedProvider(logr.Discard())
	engine := NewEngine(logr.Discard(), provider)

	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Category:   detection.CategoryResource,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "api-pod", Namespace: "prod"},
			DetectedAt: now,
		},
		{
			Type:       detection.SignalMemoryUsageHigh,
			Severity:   detection.SeverityWarning,
			Category:   detection.CategoryResource,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "api-pod", Namespace: "prod"},
			Value:      floatPtr(95),
			DetectedAt: now.Add(-10 * time.Second),
		},
	}

	diagnoses, err := engine.Analyze(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) == 0 {
		t.Fatal("expected at least 1 diagnosis")
	}

	// Should produce an OOM diagnosis with high confidence.
	found := false
	for _, d := range diagnoses {
		if d.Provider == "rule-based" && d.SuggestedAction == "resource-adjustment" {
			found = true
			if d.Confidence < 0.70 {
				t.Errorf("OOM+memory diagnosis confidence = %v, expected >= 0.70", d.Confidence)
			}
		}
	}
	if !found {
		t.Error("expected rule-based resource-adjustment diagnosis")
	}
}

func TestEngine_NoProviders(t *testing.T) {
	engine := NewEngine(logr.Discard())

	signals := []detection.Signal{
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "test-pod", Namespace: "default"},
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := engine.Analyze(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) != 0 {
		t.Errorf("expected 0 diagnoses with no providers, got %d", len(diagnoses))
	}
}
