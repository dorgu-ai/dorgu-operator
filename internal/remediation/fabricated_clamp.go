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

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// CR5-03, clean-room run #5: a clamp was announced on two plans where nothing
// was clamped.
//
// The model asked for exactly 2x. No guardrail refused anything.
// `spec.steps[].safety` was null on every step, no GUARDRAIL column appeared,
// no guardrail block was printed — and the sentence "Clamped by the 2x
// blast-radius guardrail: … could not be raised further in one step" appeared
// three times regardless, in `explanation`, in `planSummary` and in a step's
// `rationale`. Two paragraphs apart, the model states that it chose to double.
// The two claims contradict each other and nothing tells the reader which one
// was computed. Then `explanation` is copied verbatim into
// `IncidentMemory.spec.resolution.action`, so the fabricated verdict outlives
// the incident it was invented for.
//
// Two producers can write that sentence, and both are covered here because the
// enforcement point is the reader's surface, not the author:
//
//   - Dorgu's own discloseBlastRadiusClamp, which fires when a plan merely SITS
//     at the cap. Sitting at a cap is not being clamped: the guardrail refused
//     nothing, so calling it a clamp reports a refusal that never happened.
//   - the model, which prompt rule G7 forbids from commenting on caps at all,
//     and which does it anyway. A prompt is a request, not an enforcement point.
//
// The rule is therefore keyed on the only thing that is ever computed: a
// guardrail spoke if and only if it left a `spec.steps[].safety` entry behind.
// With no entry, no surface of the plan may claim one ruled.

// guardrailTerms are the words that only appear when a surface is asserting
// something about a guardrail. Each is matched anywhere in a sentence.
var guardrailTerms = []string{
	"clamp",
	"guardrail",
	"blast radius",
	"blast-radius",
	"ceiling",
}

// guardrailWords are matched as whole words only. "cap" is the word a plan
// reaches for when it means the ceiling, and it is also the first three letters
// of "capacity", which a plan reaches for when it means something else
// entirely.
var guardrailWords = []string{
	"cap",
	"caps",
	"capped",
	"capping",
}

// scrubFabricatedGuardrailClaims removes guardrail assertions from a plan's
// prose wherever no guardrail verdict backs them, and returns how many surfaces
// it changed.
//
// The condition is the plan's, not the individual step's. A verdict is recorded
// on the step whose patch earned it, and the sentences about it land wherever
// they are most useful to the reader: the plan summary, the explanation, a
// neighbouring advisory step explaining why its command is gone. Scoping the
// rule per step would silence those cross-references while changing nothing
// about the case the clean room actually found, where no step carried a verdict
// at all.
//
// It runs last, after every producer of prose has had its say, so it sees the
// plan as the reader will.
func scrubFabricatedGuardrailClaims(action *dorguv1.RemediationAction) int {
	if action == nil {
		return 0
	}
	for i := range action.Spec.Steps {
		if len(action.Spec.Steps[i].Safety) > 0 {
			return 0
		}
	}

	scrubbed := 0
	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		if description, changed := dropGuardrailClaims(step.Description); changed {
			scrubbed++
			step.Description = description
		}
		if rationale, changed := dropGuardrailClaims(step.Rationale); changed {
			scrubbed++
			step.Rationale = rationale
		}
	}

	if summary, changed := dropGuardrailClaims(action.Spec.PlanSummary); changed {
		scrubbed++
		action.Spec.PlanSummary = summary
	}
	if explanation, changed := dropGuardrailClaims(action.Spec.Explanation); changed {
		scrubbed++
		action.Spec.Explanation = explanation
	}

	return scrubbed
}

// dropGuardrailClaims removes the sentences of a surface that assert a
// guardrail ruled, and reports whether it removed any.
//
// It removes whole sentences rather than rewriting them. A sentence that
// mentions a guardrail is about the guardrail, and there is no honest shorter
// version of a claim that should not have been made.
func dropGuardrailClaims(text string) (string, bool) {
	if strings.TrimSpace(text) == "" {
		return text, false
	}

	kept := make([]string, 0, 4)
	changed := false
	for _, sentence := range splitSentences(text) {
		if assertsGuardrailVerdict(sentence) {
			changed = true
			continue
		}
		kept = append(kept, sentence)
	}
	if !changed {
		return text, false
	}
	return strings.TrimSpace(strings.Join(kept, " ")), true
}

// assertsGuardrailVerdict reports whether a piece of prose claims a guardrail
// did something.
//
// The test is blunt on purpose, in the same way SanitizeStepCommand is: an
// over-eager strip costs the reader one sentence of model prose, and a missed
// one costs them the ability to tell a computed refusal from a sentence a model
// wrote. Those are not comparable losses.
func assertsGuardrailVerdict(text string) bool {
	lower := strings.ToLower(text)
	for _, term := range guardrailTerms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	for _, word := range guardrailWords {
		if containsWord(lower, word) {
			return true
		}
	}
	return false
}

// containsWord reports whether word appears in text delimited by non-letters,
// so "capacity" is not a mention of "cap".
func containsWord(text, word string) bool {
	for offset := 0; ; {
		idx := strings.Index(text[offset:], word)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(word)
		if !letterAt(text, start-1) && !letterAt(text, end) {
			return true
		}
		offset = start + 1
	}
}

// letterAt reports whether the byte at index i is a letter, treating
// out-of-range as not.
func letterAt(text string, i int) bool {
	if i < 0 || i >= len(text) {
		return false
	}
	c := text[i]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
