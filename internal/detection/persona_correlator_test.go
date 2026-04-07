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

package detection

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func init() {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))
}

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = dorguv1.AddToScheme(s)
	return s
}

func TestPersonaCorrelator_MatchesOOMSignalToPersona(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oom-test",
			Namespace: "incident-test",
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "oom-test",
			Type: "api",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(persona).
		Build()
	correlator := NewPersonaCorrelator(c, logf.Log)

	signals := []Signal{
		{
			Type:     SignalOOMKilled,
			Severity: SeverityCritical,
			Resource: dorguv1.ResourceReference{
				Kind:      "Pod",
				Name:      "oom-test-abc123-xyz",
				Namespace: "incident-test",
			},
			DetectedAt: time.Now(),
		},
	}

	correlator.Correlate(context.Background(), signals)

	if signals[0].PersonaRef == nil {
		t.Fatal("expected PersonaRef to be set, got nil")
	}
	if signals[0].PersonaRef.Name != "oom-test" {
		t.Errorf("expected persona name 'oom-test', got %q", signals[0].PersonaRef.Name)
	}
	if signals[0].PersonaRef.Kind != "ApplicationPersona" {
		t.Errorf("expected kind 'ApplicationPersona', got %q", signals[0].PersonaRef.Kind)
	}
	if signals[0].PersonaRef.Namespace != "incident-test" {
		t.Errorf("expected namespace 'incident-test', got %q", signals[0].PersonaRef.Namespace)
	}
}

func TestPersonaCorrelator_NoPersonaInNamespace(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		Build()
	correlator := NewPersonaCorrelator(c, logf.Log)

	signals := []Signal{
		{
			Type:     SignalOOMKilled,
			Severity: SeverityCritical,
			Resource: dorguv1.ResourceReference{
				Kind:      "Pod",
				Name:      "orphan-pod-abc123",
				Namespace: "no-personas",
			},
			DetectedAt: time.Now(),
		},
	}

	correlator.Correlate(context.Background(), signals)

	if signals[0].PersonaRef != nil {
		t.Errorf("expected PersonaRef to remain nil, got %+v", signals[0].PersonaRef)
	}
}

func TestPersonaCorrelator_MultiplePersonasSameNamespace(t *testing.T) {
	personaA := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-alpha",
			Namespace: "multi-ns",
		},
		Spec: dorguv1.ApplicationPersonaSpec{Name: "app-alpha", Type: "api"},
	}
	personaB := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-beta",
			Namespace: "multi-ns",
		},
		Spec: dorguv1.ApplicationPersonaSpec{Name: "app-beta", Type: "worker"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(personaA, personaB).
		Build()
	correlator := NewPersonaCorrelator(c, logf.Log)

	signals := []Signal{
		{
			Type:     SignalCrashLoopBackOff,
			Severity: SeverityCritical,
			Resource: dorguv1.ResourceReference{
				Kind:      "Pod",
				Name:      "app-beta-7f8b9c-xyz",
				Namespace: "multi-ns",
			},
			DetectedAt: time.Now(),
		},
	}

	correlator.Correlate(context.Background(), signals)

	if signals[0].PersonaRef == nil {
		t.Fatal("expected PersonaRef to be set")
	}
	if signals[0].PersonaRef.Name != "app-beta" {
		t.Errorf("expected persona 'app-beta', got %q", signals[0].PersonaRef.Name)
	}
}

func TestPersonaCorrelator_AlreadyHasPersonaRef(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "default",
		},
		Spec: dorguv1.ApplicationPersonaSpec{Name: "my-app", Type: "api"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(persona).
		Build()
	correlator := NewPersonaCorrelator(c, logf.Log)

	existingRef := &dorguv1.PersonaReference{
		Kind:      "ApplicationPersona",
		Name:      "pre-existing",
		Namespace: "default",
	}

	signals := []Signal{
		{
			Type:       SignalOOMKilled,
			Severity:   SeverityCritical,
			PersonaRef: existingRef,
			Resource: dorguv1.ResourceReference{
				Kind:      "Pod",
				Name:      "my-app-pod-1",
				Namespace: "default",
			},
			DetectedAt: time.Now(),
		},
	}

	correlator.Correlate(context.Background(), signals)

	if signals[0].PersonaRef.Name != "pre-existing" {
		t.Errorf("expected pre-existing PersonaRef to be preserved, got %q", signals[0].PersonaRef.Name)
	}
}

func TestPersonaCorrelator_ClusterScopedSignal_NoNamespace(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "default",
		},
		Spec: dorguv1.ApplicationPersonaSpec{Name: "my-app", Type: "api"},
	}

	c := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(persona).
		Build()
	correlator := NewPersonaCorrelator(c, logf.Log)

	signals := []Signal{
		{
			Type:     SignalNodeNotReady,
			Severity: SeverityCritical,
			Resource: dorguv1.ResourceReference{
				Kind: "Node",
				Name: "worker-1",
				// No namespace — cluster-scoped
			},
			DetectedAt: time.Now(),
		},
	}

	correlator.Correlate(context.Background(), signals)

	if signals[0].PersonaRef != nil {
		t.Errorf("expected PersonaRef to remain nil for cluster-scoped signal, got %+v", signals[0].PersonaRef)
	}
}

func TestMatchesPersona_SpecNameDiffersFromMetadataName(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "persona-v2",
			Namespace: "default",
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "my-service",
			Type: "api",
		},
	}

	sig := &Signal{
		Resource: dorguv1.ResourceReference{
			Kind:      "Pod",
			Name:      "my-service-deploy-abc123",
			Namespace: "default",
		},
	}

	if !matchesPersona(sig, persona) {
		t.Error("expected signal to match persona via spec.Name")
	}

	sigNoMatch := &Signal{
		Resource: dorguv1.ResourceReference{
			Kind:      "Pod",
			Name:      "unrelated-pod-xyz",
			Namespace: "default",
		},
	}

	if matchesPersona(sigNoMatch, persona) {
		t.Error("expected signal NOT to match persona")
	}
}
