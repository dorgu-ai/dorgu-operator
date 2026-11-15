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
