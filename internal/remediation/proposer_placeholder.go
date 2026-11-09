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
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// No step reads "placeholder" (B10 d, D1.1 finding 6).
//
// A model that has nothing to say about a step can still be made to fill the required
// fields of the plan schema, and one filled description and rationale with the word
// "placeholder": `dorgu remediation diff` then printed "[1] manual: placeholder" over a
// plan whose content was in the root cause. The word is never a step's text. The
// description is filled from the plan summary, or the step is dropped; a rationale that
// is only the word is filled from the summary or left empty.

// errOnlyPlaceholders means every step of a plan was a placeholder with nothing to fill
// it from, so nothing is left to propose.
var errOnlyPlaceholders = errors.New("the plan had only placeholder steps and no summary to fill them from")

// fallbackStepDescription describes a persona-update step that has a patch and a
// placeholder for a description. The patch is what matters; the words are Dorgu's.
const fallbackStepDescription = "Change the application's resource settings."

// placeholderWords are the fillers a model writes when it has nothing to say.
var placeholderWords = []string{"placeholder", "todo", "tbd"}

// isPlaceholderText reports whether text is filler: it begins with one of the
// placeholderWords as a whole word, in any case ("placeholder", "Placeholder step",
// "TODO: describe", "TBD"), or is "n/a". Empty text is not filler here (a rationale may be
// empty); an empty description is handled by the caller. The word inside real prose
// ("Replace the placeholder image tag.") is not filler.
func isPlaceholderText(text string) bool {
	folded := strings.ToLower(strings.TrimSpace(text))
	folded = strings.TrimLeft(folded, " .!:;-_*[](){}<>\"'`")
	if strings.Trim(folded, " .!") == "n/a" {
		return true
	}
	words := strings.FieldsFunc(folded, func(r rune) bool { return !unicode.IsLetter(r) })
	return len(words) > 0 && slices.Contains(placeholderWords, words[0])
}

// blankOrPlaceholder is isPlaceholderText for a description, which may not be empty either.
func blankOrPlaceholder(text string) bool {
	return strings.TrimSpace(text) == "" || isPlaceholderText(text)
}

// repairPlaceholderSteps fills the steps whose description or rationale is a placeholder
// from the plan summary, drops a step whose description cannot be filled, and renumbers
// what is left. A step that carries a persona patch is never dropped: the patch is the
// change, so it gets Dorgu's own description. Steps with no placeholder are returned
// untouched.
func repairPlaceholderSteps(steps []dorguv1.RemediationStep, summary string) []dorguv1.RemediationStep {
	summary = strings.TrimSpace(summary)
	found := false
	for i := range steps {
		if blankOrPlaceholder(steps[i].Description) || isPlaceholderText(steps[i].Rationale) {
			found = true
			break
		}
	}
	if !found {
		return steps
	}

	out := make([]dorguv1.RemediationStep, 0, len(steps))
	for i := range steps {
		step := steps[i].DeepCopy()
		if isPlaceholderText(step.Rationale) {
			step.Rationale = summary
		}
		if blankOrPlaceholder(step.Description) {
			switch {
			case summary != "":
				step.Description = summary
			case step.Patch != nil && len(step.Patch.Raw) > 0:
				step.Description = fallbackStepDescription
			default:
				continue
			}
		}
		out = append(out, *step)
	}
	for i := range out {
		out[i].Order = int32(i + 1) //nolint:gosec // bounded by the plan's own step count
		out[i].ID = fmt.Sprintf("step-%d", i+1)
	}
	return out
}

// repairBeforePersist is the chokepoint: whichever producer built the plan, a step with
// placeholder text is repaired before the object is written. It fails when nothing would
// be left, so the caller falls back to the rule-based proposal.
func (p *Proposer) repairBeforePersist(action *dorguv1.RemediationAction) (*dorguv1.RemediationAction, error) {
	if len(action.Spec.Steps) == 0 {
		return action, nil
	}
	repaired := repairPlaceholderSteps(action.Spec.Steps, action.Spec.PlanSummary)
	if len(repaired) == 0 {
		return nil, errOnlyPlaceholders
	}
	action.Spec.Steps = repaired
	return action, nil
}
