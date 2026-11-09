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
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// The advisory fallback (EKS QA F-5, review L-2). When the gate refuses an AI plan's
// persona change because the rules did not recommend a resource adjustment, the
// persona-update steps are dropped and the advisory steps are proposed. The model's
// words for the steps that stay can still describe the dropped change ("with memory
// already raised to 384Mi, read the logs"), and a person approving from the screen
// would believe it happened. The chosen fix is the simplest safe one: the sentences
// that name a value of the dropped patch are removed, with the same redaction every
// refused value gets (refused_values.go), and the explanation says the change was
// not proposed. Nothing in the advisory steps is executed, so this is about what the
// reader is told, not about what is applied.

// droppedChangeNote is appended to the explanation of a fallback proposal.
const droppedChangeNote = "The plan also suggested a change to the application's resources. " +
	"It is not proposed here: the rules did not recommend a resource adjustment for this diagnosis."

// removedSentenceNote stands in for a step description the redaction emptied.
const removedSentenceNote = "(A sentence about a resource change that was not proposed was removed.)"

// droppedPersonaValues lists every leaf value of the persona-update patches in steps,
// as the values to keep out of the text of the steps that remain.
func droppedPersonaValues(steps []dorguv1.RemediationStep) []refusedValue {
	var out []refusedValue
	seen := make(map[string]bool)
	for i := range steps {
		if steps[i].Type != dorguv1.StepTypePersonaUpdate || steps[i].Patch == nil {
			continue
		}
		for field, value := range gate.PatchLeafValues(steps[i].Patch.Raw) {
			key := field + "=" + value
			if value == "" || seen[key] {
				continue
			}
			seen[key] = true
			entry := refusedValue{field: field, value: value}
			if qty, err := resource.ParseQuantity(value); err == nil {
				entry.quantity = &qty
			}
			out = append(out, entry)
		}
	}
	return out
}

// scrubDroppedChange removes the dropped change's values from the prose and commands
// of a fallback proposal and says the change was not proposed. It returns how many
// surfaces it changed.
func scrubDroppedChange(action *dorguv1.RemediationAction, dropped []refusedValue) int {
	scrubbed := 0
	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		if step.Command != "" && offersRefusedValue(step.Command, dropped) {
			scrubbed++
			step.Command = ""
		}
		if text, changed := redactRefusedProse(step.Description, nil, dropped); changed {
			scrubbed++
			if strings.TrimSpace(text) == "" {
				text = removedSentenceNote
			}
			step.Description = text
		}
		if text, changed := redactRefusedProse(step.Rationale, nil, dropped); changed {
			scrubbed++
			step.Rationale = text
		}
	}
	if text, changed := redactRefusedProse(action.Spec.PlanSummary, nil, dropped); changed {
		scrubbed++
		action.Spec.PlanSummary = text
	}
	if text, changed := redactRefusedProse(action.Spec.Explanation, nil, dropped); changed {
		scrubbed++
		action.Spec.Explanation = text
	}
	action.Spec.Explanation = gate.AppendNote(action.Spec.Explanation, droppedChangeNote)
	return scrubbed
}
