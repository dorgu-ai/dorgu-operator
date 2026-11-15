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

package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// stepSafetyMaxItems reads the maxItems of steps[].safety from a generated CRD, so
// the bound the gate is held to is the one the API server enforces.
func stepSafetyMaxItems(t *testing.T, crdFile string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "crd", "bases", crdFile))
	require.NoError(t, err)
	var crd map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &crd))

	node := any(crd)
	for _, key := range []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec",
		"properties", "steps", "items", "properties", "safety", "maxItems"} {
		switch n := node.(type) {
		case map[string]any:
			node = n[key]
		case []any:
			node = n[0]
		default:
			t.Fatalf("%s: no %s on the way to steps[].safety.maxItems", crdFile, key)
		}
	}
	max, ok := node.(float64)
	require.True(t, ok, "%s: steps[].safety has no maxItems", crdFile)
	return int(max)
}

// junkPatch is a persona patch with the memory change and n keys outside the
// allowlist, each of which the gate strips and records.
func junkPatch(n int) string {
	extra := make([]string, 0, n)
	for i := range n {
		extra = append(extra, fmt.Sprintf(`"junk%03d":"x"`, i))
	}
	return `{"spec":{"resources":{"limits":{"memory":"300Mi"}},"policies":{` + strings.Join(extra, ",") + `}}}`
}

// N-1 of the C1 re-review: a step with more stripped fields than the CRD allows
// in `safety` made the RemediationAction create fail Invalid, and the proposal was
// retried forever. The gate writes `safety`, so the gate bounds it, and the bound
// is below what the API server accepts on both CRDs that carry the field.
func TestStepSafety_IsBoundedByTheGateBelowTheCRDLimit(t *testing.T) {
	for _, crd := range []string{"dorgu.io_remediationactions.yaml", "dorgu.io_remediationproposals.yaml"} {
		t.Run(crd, func(t *testing.T) {
			limit := stepSafetyMaxItems(t, crd)
			require.Less(t, MaxStepSafetyEntries, limit,
				"the gate must be able to write its largest step record without the API server refusing it")

			g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
			// Far more stripped fields than the gate keeps, so the bound is the gate's, not the plan's.
			d := run(t, g, planCandidate(personaUpdateStep(jsonOf(junkPatch(MaxStepSafetyEntries*4)))), PhaseProposal)

			require.Len(t, d.Steps, 1)
			safety := d.Steps[0].Safety
			assert.LessOrEqual(t, len(safety), MaxStepSafetyEntries)
			assert.LessOrEqual(t, len(safety), limit)

			// What was left out is said, in one entry, and the rest is the gate's own records.
			last := safety[len(safety)-1]
			assert.Equal(t, dorguv1.SafetyRulePlanValidation, last.Rule)
			assert.Equal(t, dorguv1.SafetyVerdictRejected, last.Verdict)
			assert.Contains(t, last.Message, fmt.Sprint(MaxStepSafetyEntries*4-(MaxStepSafetyEntries-1)),
				"the summary says how many records were left out")
			assert.NotEmpty(t, last.Field, "a StepSafety field is required by the CRD")
			for _, s := range safety {
				assert.NotEmpty(t, s.Field)
				assert.NotEmpty(t, s.Message)
			}
		})
	}
}

// Under the bound, nothing is summarised: the cap changes nothing for a normal plan.
func TestStepSafety_UnderTheBoundIsUntouched(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	d := run(t, g, planCandidate(personaUpdateStep(jsonOf(junkPatch(3)))), PhaseProposal)
	require.Len(t, d.Steps, 1)
	assert.Len(t, d.Steps[0].Safety, 3)
	for _, s := range d.Steps[0].Safety {
		assert.NotContains(t, s.Message, "left out")
	}
}

// R-1 of the C1 re-check 3: the cap kept the first entries, which are always the
// field-allowlist strips (the Validate stage runs first), so 40 junk keys pushed the
// guardrail's own record (blast radius: requested, permitted) off the step. That
// record is what an approver reads and what the refused-value check reads to stop a
// refused value being offered again, so the cap keeps every record that is not an
// allowlist strip first and fills what is left with the strips.
func TestStepSafety_PaddingNeverHidesAGuardrailRecord(t *testing.T) {
	for _, junk := range []int{0, 5, 40, MaxStepSafetyEntries * 4} {
		t.Run(fmt.Sprintf("%d junk keys", junk), func(t *testing.T) {
			g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
			patch := strings.Replace(junkPatch(junk), `"300Mi"`, `"8Gi"`, 1)
			d := run(t, g, planCandidate(personaUpdateStep(jsonOf(patch))), PhaseProposal)

			require.Len(t, d.Steps, 1)
			safety := d.Steps[0].Safety
			assert.LessOrEqual(t, len(safety), MaxStepSafetyEntries)

			var blast []dorguv1.StepSafety
			for _, s := range safety {
				if s.Rule == dorguv1.SafetyRuleBlastRadius {
					blast = append(blast, s)
				}
			}
			require.Len(t, blast, 1, "the blast-radius record survives any amount of padding: %+v", safety)
			assert.NotEmpty(t, blast[0].Requested)
			assert.NotEmpty(t, blast[0].Baseline)
			assert.NotEmpty(t, blast[0].MaxRatio)
		})
	}
}

// Every record that is not an allowlist strip is kept before the strips are, in the
// order the gate wrote them, and the summary still comes last.
func TestCapStepSafety_KeepsGuardrailRecordsFirstThenFillsWithStrips(t *testing.T) {
	strip := func(i int) dorguv1.StepSafety {
		return dorguv1.StepSafety{Rule: dorguv1.SafetyRulePlanValidation, Verdict: dorguv1.SafetyVerdictRejected,
			Field: fmt.Sprintf("strip-%02d", i), Message: "left out"}
	}
	guardrail := func(i int) dorguv1.StepSafety {
		return dorguv1.StepSafety{Rule: dorguv1.SafetyRuleBlastRadius, Verdict: dorguv1.SafetyVerdictRejected,
			Field: fmt.Sprintf("guard-%02d", i), Message: "refused"}
	}
	in := make([]dorguv1.StepSafety, 0, 42)
	for i := range 40 {
		in = append(in, strip(i))
	}
	in = append(in, guardrail(0), guardrail(1))
	steps := []dorguv1.RemediationStep{{Safety: in}}

	capStepSafety(steps)

	out := steps[0].Safety
	require.Len(t, out, MaxStepSafetyEntries)
	// The two guardrail records survive; the strips fill the other 29 places, oldest first.
	fields := make([]string, 0, len(out))
	for _, s := range out {
		fields = append(fields, s.Field)
	}
	assert.Equal(t, "strip-00", out[0].Field, "survivors stay in the order they were written")
	assert.Equal(t, "strip-28", out[28].Field)
	assert.Equal(t, "guard-00", out[29].Field)
	assert.Equal(t, "guard-01", out[30].Field)
	assert.NotContains(t, fields, "strip-29")
	assert.Equal(t, "(more fields)", out[len(out)-1].Field)
	assert.Contains(t, out[len(out)-1].Message, fmt.Sprint(len(in)-(MaxStepSafetyEntries-1)))
}

// More guardrail records than the cap holds are cut as well: the bound is absolute.
func TestCapStepSafety_NeverExceedsTheBoundEvenWithOnlyGuardrailRecords(t *testing.T) {
	in := make([]dorguv1.StepSafety, 0, MaxStepSafetyEntries*2)
	for i := range MaxStepSafetyEntries * 2 {
		in = append(in, dorguv1.StepSafety{Rule: dorguv1.SafetyRuleBlastRadius, Verdict: dorguv1.SafetyVerdictRejected,
			Field: fmt.Sprintf("guard-%02d", i), Message: "refused"})
	}
	steps := []dorguv1.RemediationStep{{Safety: in}}
	capStepSafety(steps)
	assert.Len(t, steps[0].Safety, MaxStepSafetyEntries)
	assert.Equal(t, "(more fields)", steps[0].Safety[MaxStepSafetyEntries-1].Field)
}
