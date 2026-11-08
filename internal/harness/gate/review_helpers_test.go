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
	"testing"

	"github.com/stretchr/testify/require"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Tests for the independent adversarial review of #54 (findings R1 to R12). Each
// was written red first, against the code as the review found it. They assert on
// the stage and guardrail (the reason enum) of a refusal, never on message text.

// refusedBy asserts the run was refused and returns the stage and guardrail.
func refusedBy(t *testing.T, d *Decision) (string, string) {
	t.Helper()
	require.Equal(t, ResultRefuse, d.Result, "verdicts: %+v", d.Verdicts)
	return d.RefusedBy()
}

func withActionPatch(cand Candidate, patch string) Candidate {
	cand.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: jsonOf(patch)}
	return cand
}

func actionValues(d *Decision) map[string]string {
	if d.Action.Patch == nil {
		return nil
	}
	return PatchLeafValues(d.Action.Patch.Raw)
}
