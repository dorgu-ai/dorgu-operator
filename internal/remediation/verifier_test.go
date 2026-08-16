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

package remediation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// mockCollector implements detection.SignalCollector for testing.
type mockCollector struct {
	signals []detection.Signal
	err     error
}

func (m *mockCollector) Name() string { return "mock" }
func (m *mockCollector) Collect(_ context.Context) ([]detection.Signal, error) {
	return m.signals, m.err
}

func newTestVerifier(signals []detection.Signal, collectErr error, objs ...any) *Verifier {
	collector := &mockCollector{signals: signals, err: collectErr}
	engine := detection.NewEngine(logr.Discard(), collector)

	var clientObjs []interface {
		GetObjectKind() interface{ GroupVersionKind() any }
	}
	_ = clientObjs // suppress unused

	builder := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithStatusSubresource(&dorguv1.IncidentMemory{})

	for _, obj := range objs {
		if o, ok := obj.(*dorguv1.IncidentMemory); ok {
			builder = builder.WithObjects(o)
		}
	}

	c := builder.Build()
	return NewVerifier(engine, c, logr.Discard())
}

func testIncident() *dorguv1.IncidentMemory {
	return &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-incident",
			Namespace: defaultNamespace,
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind:      kindApplicationPersona,
				Name:      "test-persona",
				Namespace: defaultNamespace,
			},
			Category: "resource",
			Severity: "critical",
			Detection: dorguv1.DetectionInfo{
				Signal:    "OOMKilled",
				Source:    "pod-failure-detector",
				FirstSeen: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				LastSeen:  metav1.NewTime(time.Now()),
				AffectedResources: []dorguv1.ResourceReference{
					{Kind: "Pod", Name: "test-pod", Namespace: defaultNamespace},
				},
			},
		},
		Status: dorguv1.IncidentMemoryStatus{
			Phase:           "Detected",
			OccurrenceCount: 1,
		},
	}
}

func testRemediationAction() *dorguv1.RemediationAction {
	return baseRemediationAction()
}

func TestVerifier_Verify_Healthy(t *testing.T) {
	// No signals present → original signal gone → Healthy.
	incident := testIncident()
	v := newTestVerifier(nil, nil, incident)
	action := testRemediationAction()

	result, err := v.Verify(context.Background(), action)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result != VerificationHealthy {
		t.Errorf("expected Healthy, got %s", result)
	}
}

func TestVerifier_Verify_Degraded_OriginalSignalPresent(t *testing.T) {
	incident := testIncident()

	// Original OOMKilled signal still present for the same persona.
	signals := []detection.Signal{
		{
			Type:     detection.SignalOOMKilled,
			Severity: detection.SeverityCritical,
			Category: detection.CategoryResource,
			Source:   "pod-failure-detector",
			PersonaRef: &dorguv1.PersonaReference{
				Kind:      kindApplicationPersona,
				Name:      "test-persona",
				Namespace: defaultNamespace,
			},
			DetectedAt: time.Now(),
		},
	}

	v := newTestVerifier(signals, nil, incident)
	action := testRemediationAction()

	result, err := v.Verify(context.Background(), action)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result != VerificationDegraded {
		t.Errorf("expected Degraded, got %s", result)
	}
}

func TestVerifier_Verify_Degraded_NewCriticalSignals(t *testing.T) {
	incident := testIncident()

	// Original signal gone, but new critical signal for the same persona.
	signals := []detection.Signal{
		{
			Type:     detection.SignalCPUSaturationCritical,
			Severity: detection.SeverityCritical,
			Category: detection.CategoryResource,
			Source:   "resource-detector",
			PersonaRef: &dorguv1.PersonaReference{
				Kind:      kindApplicationPersona,
				Name:      "test-persona",
				Namespace: defaultNamespace,
			},
			DetectedAt: time.Now(),
		},
	}

	v := newTestVerifier(signals, nil, incident)
	action := testRemediationAction()

	result, err := v.Verify(context.Background(), action)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result != VerificationDegraded {
		t.Errorf("expected Degraded, got %s", result)
	}
}

func TestVerifier_Verify_Healthy_CollectorError(t *testing.T) {
	// When a collector errors, the engine logs it and returns empty signals.
	// Empty signals means the original signal is gone → Healthy.
	incident := testIncident()

	v := newTestVerifier(nil, fmt.Errorf("collector failure"), incident)
	action := testRemediationAction()

	result, err := v.Verify(context.Background(), action)
	if err != nil {
		t.Fatalf("expected no error (engine absorbs collector errors), got: %v", err)
	}
	if result != VerificationHealthy {
		t.Errorf("expected Healthy (no signals found), got %s", result)
	}
}

func TestVerifier_Verify_Unknown_MissingIncident(t *testing.T) {
	// No incident in client → Unknown.
	v := newTestVerifier(nil, nil)
	action := testRemediationAction()

	result, err := v.Verify(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for missing incident")
	}
	if result != VerificationUnknown {
		t.Errorf("expected Unknown, got %s", result)
	}
}

func TestVerifier_Verify_Healthy_SignalForDifferentPersona(t *testing.T) {
	incident := testIncident()

	// Signal exists but for a different persona → should be Healthy for ours.
	signals := []detection.Signal{
		{
			Type:     detection.SignalOOMKilled,
			Severity: detection.SeverityCritical,
			Category: detection.CategoryResource,
			Source:   "pod-failure-detector",
			PersonaRef: &dorguv1.PersonaReference{
				Kind:      kindApplicationPersona,
				Name:      "other-persona",
				Namespace: defaultNamespace,
			},
			DetectedAt: time.Now(),
		},
	}

	v := newTestVerifier(signals, nil, incident)
	action := testRemediationAction()

	result, err := v.Verify(context.Background(), action)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result != VerificationHealthy {
		t.Errorf("expected Healthy, got %s", result)
	}
}
