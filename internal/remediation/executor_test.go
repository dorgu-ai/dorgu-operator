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
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func newTestExecutor(objs ...client.Object) *Executor {
	c := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&dorguv1.RemediationAction{}, &dorguv1.ApplicationPersona{}).
		Build()
	return NewExecutor(c, logr.Discard())
}

func baseRemediationAction() *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-action",
			Namespace: defaultNamespace,
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{
				Name:      "test-incident",
				Namespace: defaultNamespace,
			},
			PersonaRef: dorguv1.PersonaReference{
				Kind:      kindApplicationPersona,
				Name:      "test-persona",
				Namespace: defaultNamespace,
			},
			TrustLevel: 2,
			Confidence: "0.85",
			Action: dorguv1.RemediationActionDetail{
				Type:          "persona-update",
				Patch:         &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
				PrePatchState: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)},
			},
			Explanation: "Increase memory limit from 256Mi to 512Mi",
		},
		Status: dorguv1.RemediationActionStatus{
			Phase: phaseApproved,
		},
	}
}

func baseApplicationPersona() *dorguv1.ApplicationPersona {
	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-persona",
			Namespace: defaultNamespace,
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Type: "api",
			Tier: "standard",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{
					Memory: "256Mi",
					CPU:    "250m",
				},
			},
		},
	}
}

func TestExecutor_Apply_ValidPatch(t *testing.T) {
	persona := baseApplicationPersona()
	action := baseRemediationAction()

	executor := newTestExecutor(persona)

	if err := executor.Apply(context.Background(), action); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify persona was updated.
	var updated dorguv1.ApplicationPersona
	if err := executor.client.Get(context.Background(), client.ObjectKeyFromObject(persona), &updated); err != nil {
		t.Fatalf("failed to get updated persona: %v", err)
	}

	if updated.Spec.Resources.Limits.Memory != "512Mi" {
		t.Errorf("expected memory=512Mi, got %s", updated.Spec.Resources.Limits.Memory)
	}

	// CPU should remain unchanged.
	if updated.Spec.Resources.Limits.CPU != "250m" {
		t.Errorf("expected cpu=250m, got %s", updated.Spec.Resources.Limits.CPU)
	}
}

func TestExecutor_Apply_RejectNonPersonaUpdateType(t *testing.T) {
	action := baseRemediationAction()
	action.Spec.Action.Type = "notification"

	executor := newTestExecutor(baseApplicationPersona())

	err := executor.Apply(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for non-persona-update type")
	}

	if want := "unsupported action type"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestExecutor_Apply_RejectNilPatch(t *testing.T) {
	action := baseRemediationAction()
	action.Spec.Action.Patch = nil

	executor := newTestExecutor(baseApplicationPersona())

	err := executor.Apply(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for nil patch")
	}

	if want := "patch must not be nil"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestExecutor_Apply_RejectNilPrePatchState(t *testing.T) {
	action := baseRemediationAction()
	action.Spec.Action.PrePatchState = nil

	executor := newTestExecutor(baseApplicationPersona())

	err := executor.Apply(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for nil prePatchState")
	}

	if want := "prePatchState must not be nil"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestExecutor_Apply_RejectWrongPhase(t *testing.T) {
	action := baseRemediationAction()
	action.Status.Phase = phasePending

	executor := newTestExecutor(baseApplicationPersona())

	err := executor.Apply(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for wrong phase")
	}

	if want := "must be in Approved phase"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestExecutor_Apply_MissingPersona(t *testing.T) {
	action := baseRemediationAction()

	// No persona in the client.
	executor := newTestExecutor()

	err := executor.Apply(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for missing persona")
	}

	if want := "getting target persona"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestExecutor_Apply_PersonaBeingDeleted(t *testing.T) {
	persona := baseApplicationPersona()
	now := metav1.NewTime(time.Now())
	persona.DeletionTimestamp = &now
	persona.Finalizers = []string{"test-finalizer"} // Required for fake client to honor DeletionTimestamp.

	action := baseRemediationAction()

	executor := newTestExecutor(persona)

	err := executor.Apply(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for persona being deleted")
	}

	if want := "is being deleted"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestExecutor_Apply_InvalidPatchJSON(t *testing.T) {
	action := baseRemediationAction()
	action.Spec.Action.Patch = &apiextensionsv1.JSON{Raw: []byte(`not-json`)}

	executor := newTestExecutor(baseApplicationPersona())

	err := executor.Apply(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for invalid JSON patch")
	}

	if want := "not valid JSON"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestJsonMergePatch(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		patch   string
		wantKey string
		wantVal string
		wantErr bool
	}{
		{
			name:    "simple value replacement",
			target:  `{"memory":"256Mi"}`,
			patch:   `{"memory":"512Mi"}`,
			wantKey: "memory",
			wantVal: "512Mi",
		},
		{
			name:    "add new key",
			target:  `{"memory":"256Mi"}`,
			patch:   `{"cpu":"500m"}`,
			wantKey: "cpu",
			wantVal: "500m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := jsonMergePatch([]byte(tt.target), []byte(tt.patch))
			if (err != nil) != tt.wantErr {
				t.Fatalf("jsonMergePatch() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			var m map[string]any
			if err := json.Unmarshal(result, &m); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}

			if got, ok := m[tt.wantKey]; !ok || got != tt.wantVal {
				t.Errorf("expected %s=%s, got %v", tt.wantKey, tt.wantVal, got)
			}
		})
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
