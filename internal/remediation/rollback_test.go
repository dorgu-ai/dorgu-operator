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
	"testing"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func newTestRollback(objs ...client.Object) *Rollback {
	c := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&dorguv1.ApplicationPersona{}).
		Build()
	return NewRollback(c, logr.Discard())
}

func rollbackAction() *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rollback-action",
			Namespace: "default",
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{
				Name:      "test-incident",
				Namespace: "default",
			},
			PersonaRef: dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      "test-persona",
				Namespace: "default",
			},
			TrustLevel: 2,
			Confidence: "0.85",
			Action: dorguv1.RemediationActionDetail{
				Type:          "persona-update",
				Patch:         &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
				PrePatchState: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)},
			},
			Explanation: "Increase memory limit",
			Rollback: &dorguv1.RemediationRollbackSpec{
				Enabled:    true,
				MaxRetries: 1,
			},
		},
	}
}

func patchedPersona() *dorguv1.ApplicationPersona {
	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-persona",
			Namespace: "default",
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Type: "api",
			Tier: "standard",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{
					Memory: "512Mi",
					CPU:    "250m",
				},
			},
		},
	}
}

func TestRollback_Execute_Success(t *testing.T) {
	persona := patchedPersona()
	action := rollbackAction()

	rb := newTestRollback(persona)

	if err := rb.Execute(context.Background(), action); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify persona was rolled back.
	var updated dorguv1.ApplicationPersona
	if err := rb.client.Get(context.Background(), client.ObjectKeyFromObject(persona), &updated); err != nil {
		t.Fatalf("failed to get updated persona: %v", err)
	}

	if updated.Spec.Resources.Limits.Memory != "256Mi" {
		t.Errorf("expected memory=256Mi after rollback, got %s", updated.Spec.Resources.Limits.Memory)
	}

	// CPU should remain unchanged.
	if updated.Spec.Resources.Limits.CPU != "250m" {
		t.Errorf("expected cpu=250m (unchanged), got %s", updated.Spec.Resources.Limits.CPU)
	}
}

func TestRollback_Execute_Disabled(t *testing.T) {
	action := rollbackAction()
	action.Spec.Rollback.Enabled = false

	rb := newTestRollback(patchedPersona())

	err := rb.Execute(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for disabled rollback")
	}

	if want := "rollback is not enabled"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestRollback_Execute_NilRollbackSpec(t *testing.T) {
	action := rollbackAction()
	action.Spec.Rollback = nil

	rb := newTestRollback(patchedPersona())

	err := rb.Execute(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for nil rollback spec")
	}
}

func TestRollback_Execute_MissingPrePatchState(t *testing.T) {
	action := rollbackAction()
	action.Spec.Action.PrePatchState = nil

	rb := newTestRollback(patchedPersona())

	err := rb.Execute(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for missing prePatchState")
	}

	if want := "prePatchState is nil"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestRollback_Execute_MissingPersona(t *testing.T) {
	action := rollbackAction()

	// No persona in client.
	rb := newTestRollback()

	err := rb.Execute(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for missing persona")
	}

	if want := "getting target persona"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}
