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
	"slices"
	"strings"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// C6b: a value a person rolled back is not proposed again. The gate takes it out of any
// plan (guardrail rolled-back-value); this file is the planner's half, the part that
// must not happen BEFORE the gate sees a plan: the rule engine sizing the value itself,
// and the proposal telling a reader why the change they expected is absent.
//
// Nothing here decides anything from model text. Which values were rolled back comes
// from the gate's reading of operator-written records (gate.RolledBackValues), and the
// sentence is Dorgu's (gate.RolledBackMessage).

// matchRolledBack returns the rolled-back values a merge patch carries.
func matchRolledBack(rolled []gate.RolledBackValue, patch []byte) []gate.RolledBackValue {
	if len(rolled) == 0 || len(patch) == 0 {
		return nil
	}
	var out []gate.RolledBackValue
	for field, value := range gate.PatchLeafValues(patch) {
		if r, ok := gate.MatchesRolledBack(rolled, field, value); ok {
			out = append(out, r)
		}
	}
	return out
}

// explainRolledBack tells the reader, in the proposal's explanation, that a value was
// left out because a person rolled it back. It says so for every rolled-back value a
// step records as withheld (the gate wrote the entry), and for the values the rule
// engine was held back from sizing (heldBack): the plan then has no step that names
// the value, so the explanation is the only place that can.
//
// The sentence is exactly the one on the step, so the refused-value scrub, which
// protects Dorgu's verdict messages by identity, leaves it alone. For a held-back value
// there is no step entry to protect it, and it is not on the scrub's list either, so
// it is not redacted.
func explainRolledBack(action *dorguv1.RemediationAction, rolled, heldBack []gate.RolledBackValue) {
	messages := make([]string, 0, len(rolled)+len(heldBack))
	for _, r := range rolled {
		if msg := gate.RolledBackMessage(r); stepsRecord(action, msg) {
			messages = append(messages, msg)
		}
	}
	for _, r := range heldBack {
		messages = append(messages, gate.RolledBackMessage(r))
	}
	slices.Sort(messages)
	for _, msg := range slices.Compact(messages) {
		if !strings.Contains(action.Spec.Explanation, msg) {
			action.Spec.Explanation = gate.AppendNote(action.Spec.Explanation, msg)
		}
	}
}

// stepsRecord reports whether some step carries message as one of its safety verdicts.
func stepsRecord(action *dorguv1.RemediationAction, message string) bool {
	for i := range action.Spec.Steps {
		for _, s := range action.Spec.Steps[i].Safety {
			if s.Message == message {
				return true
			}
		}
	}
	return false
}
