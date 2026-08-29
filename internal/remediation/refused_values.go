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
	"unicode"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// This file is the one place a value a guardrail refused is removed from the
// plan, and it is deliberately the only one.
//
// The bug has now been found twice in two different renderings of the same
// plan. CF6-1 fixed it for owner instructions by moving applyOwnershipShaping
// below the guardrails, so the instruction quotes post-guardrail values.
// CR5-01, clean-room run #5, found it again three lines below the refusal on
// the approval screen: the blast-radius guardrail refused 512Mi and 256Mi
// against a live 8Mi baseline, and the plan's `workload-apply` step then
// offered
//
//	kubectl set resources deployment/drifted --containers=drifted \
//	  --limits=memory=512Mi --requests=memory=256Mi -n drift
//
// as a copy-paste command, on an unmanaged workload where the paste actually
// lands, with Dorgu's own rationale calling the write "safe". One paste defeats
// the product's central safety claim, performed by a user doing exactly what
// the plan told them to do.
//
// Two instances in two places is a pattern, so the fix is not a third local
// patch. Every surface is scrubbed here, once, from the structured verdicts
// that are already recorded on the steps, immediately before the plan is
// persisted. A future code path that reintroduces a refused value into a
// command, a patch, a description or a rationale still cannot emit one.
//
// What is NOT scrubbed, and why:
//
//   - StepSafety.Message. A refusal that cannot name what it refused explains
//     nothing. These sentences are Dorgu's own and they are the reason the
//     reader knows a value was asked for and denied.
//   - PrePatchState. It records what the persona holds NOW, which is a fact,
//     not an offer. In the clean-room case the persona genuinely held 512Mi, so
//     removing it would both break rollback and turn an honest
//     `512Mi -> 16Mi` diff into a lie.

// refusedValue is one value a guardrail refused, and the field it was asked for
// on. Baseline and permitted values are not refused: they are, respectively,
// what the workload has and what Dorgu will apply.
type refusedValue struct {
	// field is the persona-spec path, e.g. "spec.resources.limits.memory".
	field string

	// value is the string the plan asked for, e.g. "512Mi".
	value string

	// quantity is value parsed as a Kubernetes quantity, when it is one. It
	// exists so `268435456` is recognised as the same refusal as `256Mi`.
	quantity *resource.Quantity
}

// refusedValues reads every refusal recorded on the plan.
//
// It is collected across the WHOLE action rather than per step, because CR5-01
// was exactly a value refused on step 1 being re-offered by step 2. A verdict
// belongs to the plan, not to the step that happened to earn it.
func refusedValues(action *dorguv1.RemediationAction) []refusedValue {
	var out []refusedValue
	seen := make(map[string]bool)

	for i := range action.Spec.Steps {
		for _, s := range action.Spec.Steps[i].Safety {
			if s.Requested == "" || s.Requested == s.Permitted {
				continue
			}
			switch s.Verdict {
			case dorguv1.SafetyVerdictRejected, dorguv1.SafetyVerdictClamped:
			default:
				continue
			}
			key := s.Field + "=" + s.Requested
			if seen[key] {
				continue
			}
			seen[key] = true

			entry := refusedValue{field: s.Field, value: s.Requested}
			if qty, err := resource.ParseQuantity(s.Requested); err == nil {
				entry.quantity = &qty
			}
			out = append(out, entry)
		}
	}
	return out
}

// scrubRefusedValues removes every refused value from every surface a client
// renders, and returns how many surfaces it changed.
//
// It runs last, after the guardrails, after Dorgu's derived patch, after
// ownership shaping and after the clamp disclosure, so it sees the plan exactly
// as it is about to be written.
func scrubRefusedValues(action *dorguv1.RemediationAction) int {
	if action == nil {
		return 0
	}
	refused := refusedValues(action)
	if len(refused) == 0 {
		return 0
	}

	explanationBefore := planExplanation(action.Spec.Steps)
	scrubbed := 0

	for i := range action.Spec.Steps {
		scrubbed += scrubStep(&action.Spec.Steps[i], refused)
	}

	// The legacy single Action is derived from the steps, so it can only carry a
	// refused value if something rebuilt it out of step. Pruned rather than
	// trusted, and demoted to advisory when nothing survives, exactly as
	// setBackCompatAction would have left it.
	if pruned, changed := pruneRefusedLeaves(action.Spec.Action.Patch, refused); changed {
		scrubbed++
		action.Spec.Action.Patch = pruned
		if pruned == nil {
			action.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
		}
	}

	if summary, changed := redactRefusedProse(action.Spec.PlanSummary, allSafetyMessages(action), refused); changed {
		scrubbed++
		action.Spec.PlanSummary = summary
	}
	if explanation, changed := redactRefusedProse(action.Spec.Explanation, allSafetyMessages(action), refused); changed {
		scrubbed++
		action.Spec.Explanation = explanation
	}

	// Scrubbing can demote a step, and the explanation states how many steps are
	// applied for the reader. Only the generated prefix is recomputed, so a
	// disclosure appended after it survives.
	if after := planExplanation(action.Spec.Steps); after != explanationBefore &&
		strings.HasPrefix(action.Spec.Explanation, explanationBefore) {
		action.Spec.Explanation = after + strings.TrimPrefix(action.Spec.Explanation, explanationBefore)
	}

	return scrubbed
}

// scrubStep removes refused values from one step's patch, command, description
// and rationale.
func scrubStep(step *dorguv1.RemediationStep, refused []refusedValue) int {
	scrubbed := 0

	if pruned, changed := pruneRefusedLeaves(step.Patch, refused); changed {
		scrubbed++
		step.Patch = pruned
		if pruned == nil {
			// Nothing left to write, so this step is no longer the operator's
			// to apply.
			step.AutoExecutable = false
		}
	}

	if step.Command != "" && offersRefusedValue(step.Command, refused) {
		scrubbed++
		step.Command = ""
		step.Description = appendNote(step.Description, droppedCommandNote())
	}

	messages := stepSafetyMessages(step)
	if description, changed := redactRefusedProse(step.Description, messages, refused); changed {
		scrubbed++
		step.Description = description
	}
	if rationale, changed := redactRefusedProse(step.Rationale, messages, refused); changed {
		scrubbed++
		step.Rationale = rationale
	}

	return scrubbed
}

// droppedCommandNote is what replaces a command Dorgu will not print.
//
// The command is dropped rather than rewritten. Rebuilding an arbitrary kubectl
// invocation from a post-guardrail patch means parsing a grammar Dorgu does not
// own, and a half-corrected command that still looks runnable is worse than no
// command at all. The step's prose stays, because it still tells the reader
// something true.
func droppedCommandNote() string {
	return "Dorgu removed the command this step suggested: it carried a value a guardrail refused, " +
		"and pasting it would have applied exactly the change Dorgu declined to make. " +
		"The persona-update step above applies what the guardrails permit."
}

// pruneRefusedLeaves removes patch leaves whose field AND value match a
// refusal, returning the new patch and whether anything changed.
//
// The match is on both, not on the value alone: a permitted value that happens
// to read like a refused one on a different field is not a refusal, and a
// guardrail rules on a field.
func pruneRefusedLeaves(patch *apiextensionsv1.JSON, refused []refusedValue) (*apiextensionsv1.JSON, bool) {
	if patch == nil || len(patch.Raw) == 0 {
		return patch, false
	}

	values := patchLeafValues(patch.Raw)
	var paths []string
	for _, r := range refused {
		actual, ok := values[r.field]
		if ok && sameResourceValue(actual, r) {
			paths = append(paths, r.field)
		}
	}
	if len(paths) == 0 {
		return patch, false
	}
	return prunePatchPaths(patch, paths), true
}

// offersRefusedValue reports whether a command hands the reader a refused
// value.
//
// A command is matched on value alone, without regard to which field the
// refusal was recorded against. Dorgu does not parse kubectl's flag grammar, so
// it cannot tell which field a `--limits=memory=512Mi` belongs to with the
// confidence needed to print the command anyway.
func offersRefusedValue(command string, refused []refusedValue) bool {
	for _, r := range refused {
		if mentionsRefusedValue(command, r) {
			return true
		}
	}
	return false
}

// redactRefusedProse removes the sentences of a model-authored surface that
// name a refused value, and reports whether it changed anything.
//
// Dorgu's own verdict messages are held out of the redaction verbatim: they are
// the one place a refused value belongs, and they are recognised by identity
// rather than by wording so a future rephrasing cannot silently expose them.
func redactRefusedProse(text string, protected []string, refused []refusedValue) (string, bool) {
	if strings.TrimSpace(text) == "" {
		return text, false
	}

	masked, restore := maskProtected(text, protected)

	kept := make([]string, 0, 4)
	changed := false
	for _, sentence := range splitSentences(masked) {
		if sentenceMentionsRefused(sentence, refused) {
			changed = true
			continue
		}
		kept = append(kept, sentence)
	}
	if !changed {
		return text, false
	}

	return strings.TrimSpace(restore(strings.Join(kept, " "))), true
}

// maskProtected replaces each protected span with a token that no refused value
// can match, and returns the function that puts them back.
func maskProtected(text string, protected []string) (string, func(string) string) {
	replacements := make([]string, 0, len(protected)*2)
	masked := text
	for i, span := range protected {
		if span == "" || !strings.Contains(masked, span) {
			continue
		}
		// U+0000 cannot appear in a CRD string field, so the token is unforgeable
		// by anything the model wrote.
		token := "\x00protected-" + string(rune('a'+i%26)) + "\x00"
		masked = strings.ReplaceAll(masked, span, token)
		replacements = append(replacements, token, span)
	}
	if len(replacements) == 0 {
		return masked, func(s string) string { return s }
	}
	restorer := strings.NewReplacer(replacements...)
	return masked, restorer.Replace
}

// sentenceMentionsRefused reports whether one sentence offers a refused value.
func sentenceMentionsRefused(sentence string, refused []refusedValue) bool {
	for _, r := range refused {
		if mentionsRefusedValue(sentence, r) {
			return true
		}
	}
	return false
}

// splitSentences breaks prose after terminal punctuation, keeping the
// punctuation with the sentence it ends. A trailing fragment with no
// punctuation is returned as its own sentence, which is what most model-authored
// step descriptions are.
func splitSentences(text string) []string {
	var out []string
	start := 0
	runes := []rune(text)
	for i, r := range runes {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		if i+1 < len(runes) && !unicode.IsSpace(runes[i+1]) {
			continue
		}
		if sentence := strings.TrimSpace(string(runes[start : i+1])); sentence != "" {
			out = append(out, sentence)
		}
		start = i + 1
	}
	if tail := strings.TrimSpace(string(runes[start:])); tail != "" {
		out = append(out, tail)
	}
	return out
}

// mentionsResourceValue reports whether text offers the given value, comparing
// both as written and as a parsed quantity.
//
// Exported to the package's tests as the definition of "carries a refused
// value", so the invariant the tests assert and the invariant the scrub
// enforces are the same predicate rather than two that have to be kept in step.
func mentionsResourceValue(text, value string) bool {
	entry := refusedValue{value: value}
	if qty, err := resource.ParseQuantity(value); err == nil {
		entry.quantity = &qty
	}
	return mentionsRefusedValue(text, entry)
}

// mentionsRefusedValue reports whether text names a refused value as a token of
// its own.
//
// Two ways of naming it count. Literally, bounded so `1256Mi` is not a mention
// of `256Mi`. And numerically, so a command that writes `268435456` where the
// refusal was recorded as `256Mi` is caught: the bytes reaching the cluster are
// what the guardrail refused, whatever the notation.
func mentionsRefusedValue(text string, refused refusedValue) bool {
	if refused.value == "" || text == "" {
		return false
	}
	if containsToken(text, refused.value) {
		return true
	}
	if refused.quantity == nil {
		return false
	}
	for _, token := range quantityTokens(text) {
		if token.Cmp(*refused.quantity) == 0 {
			return true
		}
	}
	return false
}

// sameResourceValue reports whether a patch leaf holds the refused value,
// tolerating a different notation for the same quantity.
func sameResourceValue(actual string, refused refusedValue) bool {
	if actual == refused.value {
		return true
	}
	if refused.quantity == nil {
		return false
	}
	qty, err := resource.ParseQuantity(actual)
	if err != nil {
		return false
	}
	return qty.Cmp(*refused.quantity) == 0
}

// containsToken reports whether value appears in text delimited by something
// other than an alphanumeric, so a longer number that merely ends in the
// refused one does not count as a mention of it.
func containsToken(text, value string) bool {
	for offset := 0; ; {
		idx := strings.Index(text[offset:], value)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(value)
		if !alphanumericAt(text, start-1) && !alphanumericAt(text, end) {
			return true
		}
		offset = start + 1
	}
}

// alphanumericAt reports whether the byte at index i is a letter or digit,
// treating out-of-range as not.
func alphanumericAt(text string, i int) bool {
	if i < 0 || i >= len(text) {
		return false
	}
	r := rune(text[i])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// quantityTokens returns every substring of text that parses as a Kubernetes
// resource quantity.
//
// Tokens are cut on anything that cannot appear inside a quantity, so
// `--limits=memory=512Mi` yields `512Mi` and `deployment/my-app` yields
// nothing parseable.
func quantityTokens(text string) []resource.Quantity {
	var out []resource.Quantity
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.'
	}) {
		if !strings.ContainsFunc(field, unicode.IsDigit) {
			continue
		}
		if qty, err := resource.ParseQuantity(field); err == nil {
			out = append(out, qty)
		}
	}
	return out
}

// allSafetyMessages returns every guardrail message on the plan, for the
// action-level surfaces that quote them.
func allSafetyMessages(action *dorguv1.RemediationAction) []string {
	var out []string
	for i := range action.Spec.Steps {
		out = append(out, stepSafetyMessages(&action.Spec.Steps[i])...)
	}
	return out
}

// stepSafetyMessages returns one step's guardrail messages.
func stepSafetyMessages(step *dorguv1.RemediationStep) []string {
	out := make([]string, 0, len(step.Safety))
	for _, s := range step.Safety {
		if s.Message != "" {
			out = append(out, s.Message)
		}
	}
	return out
}
