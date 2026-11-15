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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// crdVerdictsMaxItems reads the maxItems of status.<field>.verdicts from the
// generated RemediationAction CRD, so the bound the gate is held to is the one the
// API server enforces.
func crdVerdictsMaxItems(t *testing.T, field string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "crd", "bases", "dorgu.io_remediationactions.yaml"))
	require.NoError(t, err)
	var crd map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	node := any(crd)
	for _, key := range []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "status",
		"properties", field, "properties", "verdicts", "maxItems"} {
		switch n := node.(type) {
		case map[string]any:
			node = n[key]
		case []any:
			node = n[0]
		default:
			t.Fatalf("no %s on the way to status.%s.verdicts.maxItems", key, field)
		}
	}
	max, ok := node.(float64)
	require.True(t, ok, "status.%s.verdicts has no maxItems", field)
	return int(max)
}

// decisionWithVerdicts records n pass verdicts, then the given refusal and clamp, as
// a plan with many steps does (one guardrail record per step).
func decisionWithVerdicts(n int) *Decision {
	d := &Decision{Phase: PhaseProposal}
	for i := range n {
		d.pass(StageGuardrails, GuardrailBlastRadius, fmt.Sprintf("step %d is within the cap", i), map[string]string{"step": fmt.Sprint(i)})
	}
	d.clamp(StageGuardrails, GuardrailBlastRadius, "step 99 was clamped", nil)
	d.refuse(StageGuardrails, GuardrailDenyList, "step 100 targets a protected namespace", nil)
	return d
}

// A plan with many steps wrote more verdicts than the CRD allows, and the API server
// refused the status (the C1 round 3 carry-over). The gate bounds what it renders to
// status and to the ticket, below the CRD limit, and says how many it left out.
func TestStatusVerdicts_AreBoundedBelowTheCRDLimit(t *testing.T) {
	limit := crdVerdictsMaxItems(t, "gate")
	require.Equal(t, limit, crdVerdictsMaxItems(t, "applyTicket"), "both lists share one bound")
	require.Less(t, MaxStatusVerdicts, limit, "the gate keeps room below what the API server accepts")

	for _, n := range []int{0, MaxStatusVerdicts - 3, MaxStatusVerdicts, limit, limit * 3} {
		t.Run(fmt.Sprintf("%d verdicts", n), func(t *testing.T) {
			d := decisionWithVerdicts(n)
			got := d.Status().Verdicts

			assert.LessOrEqual(t, len(got), MaxStatusVerdicts)
			assert.LessOrEqual(t, len(got), limit, "the API server must never be asked to store more than maxItems")
			if n+2 <= MaxStatusVerdicts {
				assert.Len(t, got, n+2, "under the bound nothing is summarised or reordered")
				return
			}
			last := got[len(got)-1]
			assert.Equal(t, GuardrailVerdictsTruncated, last.Guardrail)
			assert.Equal(t, dorguv1.GateVerdictPass, last.Verdict, "the summary must never read as a refusal")
			assert.Contains(t, last.Reason, fmt.Sprint(n+2-(MaxStatusVerdicts-1)))

			var refused, clamped bool
			for _, v := range got {
				refused = refused || v.Verdict == dorguv1.GateVerdictRefuse
				clamped = clamped || v.Verdict == dorguv1.GateVerdictClamp
			}
			assert.True(t, refused, "the refusal is kept before any pass is")
			assert.True(t, clamped, "the clamp is kept before any pass is")
		})
	}
}

func TestStatusVerdicts_TheTicketCarriesTheSameBoundedList(t *testing.T) {
	require.Equal(t, MaxStatusVerdicts, len(capVerdicts(decisionWithVerdicts(500).Verdicts)))
}

func TestCapVerdicts_KeepsEveryNonPassFirstInOrder(t *testing.T) {
	in := make([]dorguv1.GateVerdict, 0, MaxStatusVerdicts*2)
	for i := range MaxStatusVerdicts * 2 {
		verdict := dorguv1.GateVerdictPass
		if i%10 == 0 {
			verdict = dorguv1.GateVerdictClamp
		}
		in = append(in, dorguv1.GateVerdict{Stage: StageGuardrails, Guardrail: fmt.Sprintf("g%03d", i), Verdict: verdict})
	}
	out := capVerdicts(in)
	require.Len(t, out, MaxStatusVerdicts)
	prev := -1
	clamps := 0
	for _, v := range out[:len(out)-1] {
		var idx int
		_, err := fmt.Sscanf(v.Guardrail, "g%03d", &idx)
		require.NoError(t, err)
		assert.Greater(t, idx, prev, "survivors stay in the order they were written")
		prev = idx
		if v.Verdict == dorguv1.GateVerdictClamp {
			clamps++
		}
	}
	assert.Equal(t, (MaxStatusVerdicts*2+9)/10, clamps, "every clamp survives")
}
