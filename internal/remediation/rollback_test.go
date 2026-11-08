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
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
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
			// The author's own prePatchState is a trap: rollback restores the
			// gate's recorded baseline, never this.
			Action: dorguv1.RemediationActionDetail{
				Type:          "persona-update",
				Patch:         &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
				PrePatchState: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"1Mi"}}}}`)},
			},
			Explanation: "Increase memory limit",
			Rollback: &dorguv1.RemediationRollbackSpec{
				Enabled:    true,
				MaxRetries: 1,
			},
		},
		Status: dorguv1.RemediationActionStatus{
			Phase: "Applying",
			ApplyTicket: &dorguv1.ApplyTicket{
				WorkloadUID: "uid-1",
				Patch:       &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
				ExpiresAt:   metav1.NewTime(time.Now().Add(-time.Hour)), // long expired: rollback must still run.
			},
			Gate: &dorguv1.GateStatus{
				Phase: "ApprovalRecheck", Result: "Pass",
				PrePatchState: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)},
			},
		},
	}
}

func patchedPersona() *dorguv1.ApplicationPersona {
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

	if _, err := rb.Execute(context.Background(), action); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify persona was rolled back.
	var updated dorguv1.ApplicationPersona
	if err := rb.client.Get(context.Background(), client.ObjectKeyFromObject(persona), &updated); err != nil {
		t.Fatalf("failed to get updated persona: %v", err)
	}

	if updated.Spec.Resources.Limits.Memory != personaMemory {
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

	_, err := rb.Execute(context.Background(), action)
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

	_, err := rb.Execute(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for nil rollback spec")
	}
}

func TestRollback_Execute_RefusesWithoutARecordedBaseline(t *testing.T) {
	action := rollbackAction()
	action.Status.Gate.PrePatchState = nil

	rb := newTestRollback(patchedPersona())

	_, err := rb.Execute(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for a missing baseline")
	}
	if want := "no baseline"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

// An action that never held a ticket applied nothing, so there is nothing to undo.
func TestRollback_Execute_RefusesWithoutATicket(t *testing.T) {
	action := rollbackAction()
	action.Status.ApplyTicket = nil

	rb := newTestRollback(patchedPersona())

	_, err := rb.Execute(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for an action with no ticket")
	}
	if want := "no apply ticket"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

func TestRollback_Execute_RefusesABaselineOutsideTheAllowlist(t *testing.T) {
	action := rollbackAction()
	action.Status.Gate.PrePatchState = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"policies":{"security":{"runAsNonRoot":false}}}}`)}

	rb := newTestRollback(patchedPersona())

	if _, err := rb.Execute(context.Background(), action); err == nil {
		t.Fatal("expected the allowlist to hold for the baseline too")
	}
}

func TestRollback_Execute_MissingPersona(t *testing.T) {
	action := rollbackAction()

	// No persona in client.
	rb := newTestRollback()

	_, err := rb.Execute(context.Background(), action)
	if err == nil {
		t.Fatal("expected error for missing persona")
	}

	if want := "getting target persona"; !containsStr(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

// humanMemory is what a person set the persona to after the heal.
const humanMemory = "2Gi"

// F4: a rollback restores what Dorgu changed, not what somebody changed after.
func personaWith(memory, cpu string) *dorguv1.ApplicationPersona {
	p := patchedPersona()
	p.Spec.Resources.Limits.Memory = memory
	p.Spec.Resources.Limits.CPU = cpu
	return p
}

func memoryAndCPUAction() *dorguv1.RemediationAction {
	action := rollbackAction()
	action.Status.ApplyTicket.Patch = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"cpu":"300m","memory":"512Mi"}}}}`)}
	action.Status.Gate.PrePatchState = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"cpu":"250m","memory":"256Mi"}}}}`)}
	return action
}

func personaAfter(t *testing.T, rb *Rollback) *dorguv1.ApplicationPersona {
	t.Helper()
	var got dorguv1.ApplicationPersona
	key := client.ObjectKey{Name: "test-persona", Namespace: defaultNamespace}
	if err := rb.client.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func TestF4_RollbackDoesNotOverwriteAHumanEdit(t *testing.T) {
	rb := newTestRollback(personaWith(humanMemory, "250m")) // Dorgu wrote 512Mi; a human then set 2Gi

	result, err := rb.Execute(context.Background(), rollbackAction())
	if err != nil {
		t.Fatal(err)
	}

	if got := personaAfter(t, rb).Spec.Resources.Limits.Memory; got != humanMemory {
		t.Errorf("memory = %s: a rollback overwrote the human's 2Gi", got)
	}
	if !result.Partial() || len(result.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want the one edited field reported", result.Skipped)
	}
	skipped := result.Skipped[0]
	if skipped.Path != "spec.resources.limits.memory" || skipped.Reason != gate.ReasonRollbackHumanEdited ||
		skipped.Wrote != "512Mi" || skipped.Current != "2Gi" {
		t.Errorf("skipped = %+v", skipped)
	}
}

func TestF4_RollbackRestoresOnlyTheFieldsDorguStillOwns(t *testing.T) {
	rb := newTestRollback(personaWith(humanMemory, "300m")) // memory edited, cpu is still Dorgu's 300m

	result, err := rb.Execute(context.Background(), memoryAndCPUAction())
	if err != nil {
		t.Fatal(err)
	}

	got := personaAfter(t, rb).Spec.Resources.Limits
	if got.CPU != "250m" || got.Memory != humanMemory {
		t.Errorf("limits = cpu %s memory %s, want cpu restored to 250m and memory left at the human's 2Gi", got.CPU, got.Memory)
	}
	if len(result.Restored) != 1 || result.Restored[0] != "spec.resources.limits.cpu" {
		t.Errorf("restored = %v", result.Restored)
	}
	if !result.Partial() {
		t.Error("a rollback that left a field is partial")
	}
}

func TestF4_AnEquivalentQuantityIsStillWhatDorguWrote(t *testing.T) {
	rb := newTestRollback(personaWith("0.5Gi", "250m")) // the same 512Mi, spelled differently

	result, err := rb.Execute(context.Background(), rollbackAction())
	if err != nil {
		t.Fatal(err)
	}

	if got := personaAfter(t, rb).Spec.Resources.Limits.Memory; got != "256Mi" {
		t.Errorf("memory = %s, want 256Mi", got)
	}
	if result.Partial() {
		t.Errorf("skipped = %+v, want a complete rollback", result.Skipped)
	}
}

func TestF4_AFieldWithNoRecordedBaselineIsLeftAndReported(t *testing.T) {
	rb := newTestRollback(personaWith("512Mi", "300m"))
	action := memoryAndCPUAction()
	action.Status.Gate.PrePatchState = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)}

	result, err := rb.Execute(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}

	if got := personaAfter(t, rb).Spec.Resources.Limits; got.CPU != "300m" || got.Memory != "256Mi" {
		t.Errorf("limits = %+v", got)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != gate.ReasonRollbackNoBaseline {
		t.Errorf("skipped = %+v, want cpu reported with no baseline", result.Skipped)
	}
}
