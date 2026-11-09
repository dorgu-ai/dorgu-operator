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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// The step-command fallback (D1.1 finding 3, B10 c).
//
// A step command that is not a single plain kubectl invocation refuses the whole AI plan
// at Validate/step-command, however good the rest of it is: in D1.1 a bad-image incident
// had a diagnosis and no proposal for three cycles because one advisory step carried a
// jsonpath expression. The offending steps are dropped and the remainder goes through the
// full gate again, as F-5 does for a persona change the rules did not recommend.
//
// The refused command is something a person would have pasted. Dropping its step is not
// enough, because the model quotes its commands in its prose ("after running ..., fix the
// tag"). The text is cut out of every surface that is kept, and the proposal fails closed
// if any of it is left (mentionsAnyCommand).

// droppedCommandsNote is appended to the explanation of a proposal that lost steps to
// the refusal. It does not echo any command.
const droppedCommandsNote = "Steps that carried a command that is not a single plain kubectl invocation were not proposed."

// proposeNarrowed proposes a narrowed plan through the same path, and, when it is
// proposed, reports the refusal of the first plan beside it so the caller records it
// (condition, Event; the gate already counted it). If narrowing fails, the first refusal
// stands and is recorded like any refusal.
func (p *Proposer) proposeNarrowed(
	ctx context.Context,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
	plan *planner.RemediationPlan,
	steps []dorguv1.RemediationStep,
	rc *planner.RemediationContext,
	workloadRef *dorguv1.WorkloadRef,
	fb planFallbacks,
	dropped []refusedValue,
	droppedCommands []string,
	refusal *ProposalResult,
) (*ProposalResult, error) {
	result, err := p.gateAndPersistPlan(ctx, diag, incident, plan, steps, rc, workloadRef, fb, dropped, droppedCommands)
	switch {
	case err != nil && !errors.Is(err, errPersistFailed):
		// The narrowed pass failed. The first refusal is still the answer, so it is recorded
		// on the incident like any refusal (review L-6); the failure is a log line.
		p.logger.Info("could not propose the narrowed AI plan; the refusal of the first plan stands",
			"incident", incident.Name, "error", err.Error())
		return refusal, nil
	case err == nil && result != nil && result.Proposed:
		// The refusal that led here happened before any the narrowed pass met, so it goes first.
		result.PlanRefusals = append([]*ProposalResult{refusal}, result.PlanRefusals...)
		result.PlanRefusal = result.PlanRefusals[0]
	case err == nil && result != nil && !result.Proposed && result.Stage == "":
		return refusal, nil
	}
	return result, err
}

// dropRefusedCommandSteps returns the plan without the steps the gate refused a command
// on, renumbered from 1, and the refused command texts. ok is false when narrowing is not
// available: nothing was refused, a persona-update carries the command (a step the
// operator applies is never narrowed around), or no step would be left.
func dropRefusedCommandSteps(steps []dorguv1.RemediationStep) (kept []dorguv1.RemediationStep, refused []string, ok bool) {
	offending := gate.StepsWithRefusedCommand(steps)
	if len(offending) == 0 || len(offending) == len(steps) {
		return nil, nil, false
	}
	drop := make(map[int]bool, len(offending))
	for _, i := range offending {
		if steps[i].Type == dorguv1.StepTypePersonaUpdate {
			return nil, nil, false
		}
		drop[i] = true
		refused = append(refused, steps[i].Command)
	}
	for i := range steps {
		if drop[i] {
			continue
		}
		step := steps[i].DeepCopy()
		step.Order = int32(len(kept) + 1) //nolint:gosec // bounded by the plan's own step count
		step.ID = fmt.Sprintf("step-%d", step.Order)
		kept = append(kept, *step)
	}
	return kept, refused, true
}

// scrubDroppedCommands makes the narrowed plan carry none of the refused commands and
// nothing that points at the steps that were dropped. It returns how many surfaces it
// changed.
//
//   - The text of every refused command is cut out of each kept description, rationale
//     and non-plain command (removeCommandText).
//   - A kept step's references to a step by number or position ("after step 2", "the step
//     above") are neutralised: the numbers were renumbered and the steps they pointed at
//     are gone (L-3).
//   - The plan summary and the explanation are not the model's any more. They are
//     replaced by text Dorgu writes, which says that steps were left out and lists the
//     steps that were kept. A leak split across the summary and a step cannot survive
//     if the summary is not the model's prose to begin with.
func scrubDroppedCommands(action *dorguv1.RemediationAction, commands []string, droppedPersona bool) int {
	scrubbed := 0
	cut := func(text string) string {
		out, changed := removeCommandText(text, commands)
		if changed {
			scrubbed++
		}
		return out
	}
	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		step.Description = cut(step.Description)
		step.Rationale = cut(step.Rationale)
		if !gate.IsPlainKubectlCommand(step.Command) {
			// A kept command is plain by the gate's own predicate and is left exactly as it is.
			step.Command = cut(step.Command)
		}
		if text, changed := neutralizeStepReferences(step.Description); changed {
			scrubbed++
			step.Description = text
		}
		if text, changed := neutralizeStepReferences(step.Rationale); changed {
			scrubbed++
			step.Rationale = text
		}
	}

	explanation := planExplanation(action.Spec.Steps)
	if droppedPersona {
		explanation = gate.AppendNote(explanation, droppedChangeNote)
	}
	action.Spec.Explanation = gate.AppendNote(explanation, droppedCommandsNote)
	action.Spec.PlanSummary = narrowedPlanSummary(action.Spec.Steps, len(commands))
	return scrubbed + 2
}

// narrowedPlanSummary is the plan summary of a plan that lost steps: written by Dorgu,
// from the kept steps, never from the model's root cause.
func narrowedPlanSummary(steps []dorguv1.RemediationStep, dropped int) string {
	listed := make([]string, 0, len(steps))
	for i := range steps {
		listed = append(listed, fmt.Sprintf("%d (%s) %s", steps[i].Order, steps[i].Type, strings.TrimSpace(steps[i].Description)))
	}
	return fmt.Sprintf("Dorgu narrowed this plan: %d step(s) were left out because a command in them was not a single plain kubectl invocation. Steps proposed: %s.",
		dropped, strings.Join(listed, "; "))
}

// stepReference matches a pointer from one step to another by number or position. After
// steps are dropped and the rest renumbered, none of them points at what it did.
var stepReference = regexp.MustCompile(`(?i)\b(?:(?:the\s+)?(?:previous|preceding|prior|next|following|above|below)\s+(?:steps?|commands?)|(?:the\s+)?(?:steps?|commands?)\s+(?:above|below)|steps?\s+\d+(?:\s*(?:,|and|to|-)\s*\d+)*)\b`)

// bareDirection matches "above" and "below" on their own ("the output above").
var bareDirection = regexp.MustCompile(`(?i)\b(?:above|below)\b`)

const (
	stepReferenceNeutral = "a step of the original plan"
	directionNeutral     = "[reference removed]"
)

// neutralizeStepReferences replaces references to other steps in a kept step's text and
// reports whether it changed anything.
func neutralizeStepReferences(text string) (string, bool) {
	out := stepReference.ReplaceAllLiteralString(text, stepReferenceNeutral)
	out = bareDirection.ReplaceAllLiteralString(out, directionNeutral)
	return out, out != text
}

// commandMatchWindow is how many consecutive normalized tokens of a refused command, in
// order, make a passage the same text as that command (M-1). The whole command, its tail,
// a re-quoted copy or a copy split across two fields all contain such a window; three is
// short enough that a shell program cannot hide in what is left, and long enough that two
// words of an ordinary sentence do not match.
const commandMatchWindow = 3

// commandRemoved stands where a refused command's text was cut out of a sentence.
const commandRemoved = "[command removed]"

// textToken is one whitespace-separated word of a text: where it sits, and its normal form.
type textToken struct {
	norm       string
	start, end int
}

// tokenizeForMatch splits text into words and normalizes each: lower case, quote
// characters, backticks and backslashes (which also joins a shell line continuation)
// removed, and the sentence punctuation around the word trimmed. A word with nothing left
// is dropped. The same function reads the refused command and every kept text, so the two
// are always compared in the same form.
func tokenizeForMatch(text string) []textToken {
	var out []textToken
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		if norm := normalizeToken(text[start:end]); norm != "" {
			out = append(out, textToken{norm: norm, start: start, end: end})
		}
		start = -1
	}
	for i, r := range text {
		if unicode.IsSpace(r) {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(text))
	return out
}

func normalizeToken(word string) string {
	word = strings.ToLower(word)
	word = strings.Map(func(r rune) rune {
		switch r {
		case '\'', '"', '`', '\\', '\u2018', '\u2019', '\u201c', '\u201d':
			return -1
		}
		return r
	}, word)
	return strings.Trim(word, ".,;:!?()")
}

// commandWindows is the set of every window of consecutive normalized tokens of the
// refused commands, at the size commandMatchWindow (or the whole command when it is
// shorter).
func commandWindows(commands []string) map[string]bool {
	windows := make(map[string]bool)
	for _, command := range commands {
		tokens := tokenizeForMatch(command)
		size := min(commandMatchWindow, len(tokens))
		if size == 0 {
			continue
		}
		for i := 0; i+size <= len(tokens); i++ {
			windows[windowKey(tokens[i:i+size])] = true
		}
	}
	return windows
}

func windowKey(tokens []textToken) string {
	parts := make([]string, len(tokens))
	for i := range tokens {
		parts[i] = tokens[i].norm
	}
	return strings.Join(parts, " ")
}

// windowSize is the window length the windows set was built with: the smallest command
// window, so a refused command shorter than commandMatchWindow is still matched.
func windowSize(commands []string) int {
	size := commandMatchWindow
	for _, command := range commands {
		if n := len(tokenizeForMatch(command)); n > 0 && n < size {
			size = n
		}
	}
	return size
}

// matchedTokens returns, for each token of the text, whether it is part of a window of a
// refused command.
func matchedTokens(tokens []textToken, windows map[string]bool, size int) []bool {
	covered := make([]bool, len(tokens))
	for i := 0; i+size <= len(tokens); i++ {
		if windows[windowKey(tokens[i:i+size])] {
			for j := i; j < i+size; j++ {
				covered[j] = true
			}
		}
	}
	return covered
}

// removeCommandText replaces every passage of the text that is the same as a refused
// command, or any run of at least commandMatchWindow of its words, with a marker, and
// reports whether it changed anything. A run of matching words is replaced as one passage.
func removeCommandText(text string, commands []string) (string, bool) {
	windows := commandWindows(commands)
	if len(windows) == 0 {
		return text, false
	}
	tokens := tokenizeForMatch(text)
	covered := matchedTokens(tokens, windows, windowSize(commands))

	var b strings.Builder
	last, changed := 0, false
	for i := 0; i < len(tokens); {
		if !covered[i] {
			i++
			continue
		}
		j := i
		for j+1 < len(tokens) && covered[j+1] {
			j++
		}
		b.WriteString(text[last:tokens[i].start])
		b.WriteString(commandRemoved)
		last = tokens[j].end
		changed = true
		i = j + 1
	}
	if !changed {
		return text, false
	}
	b.WriteString(text[last:])
	return b.String(), true
}

// mentionsAnyCommand reports whether any refused command is still anywhere in the plan,
// in any of the forms removeCommandText cuts, and so whether the plan must be refused.
// It reads every string of the stored plan (so a field the cut never looked at is
// covered), and the plan's own words in order across fields (so a command split between
// a description and a rationale is covered). A kept command the gate accepts as plain is
// not read: it is the one place a command is allowed to be.
func mentionsAnyCommand(action *dorguv1.RemediationAction, commands []string) bool {
	windows := commandWindows(commands)
	if len(windows) == 0 {
		return false
	}
	size := windowSize(commands)
	leaks := func(text string) bool {
		tokens := tokenizeForMatch(text)
		for _, covered := range matchedTokens(tokens, windows, size) {
			if covered {
				return true
			}
		}
		return false
	}

	raw, err := json.Marshal(action.Spec)
	if err != nil {
		return true
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return true
	}
	var strs []string
	collectStrings(tree, &strs)
	for _, str := range strs {
		if gate.IsPlainKubectlCommand(str) {
			continue
		}
		if leaks(str) {
			return true
		}
	}

	ordered := make([]string, 0, 3*len(action.Spec.Steps)+2)
	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		ordered = append(ordered, step.Description, step.Rationale)
		if !gate.IsPlainKubectlCommand(step.Command) {
			ordered = append(ordered, step.Command)
		}
	}
	ordered = append(ordered, action.Spec.PlanSummary, action.Spec.Explanation)
	return leaks(strings.Join(ordered, " "))
}

// collectStrings gathers every string value in a decoded JSON tree, unescaped, so a
// quotation is compared as the reader would see it.
func collectStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			collectStrings(e, out)
		}
	case map[string]any:
		for _, e := range t {
			collectStrings(e, out)
		}
	}
}
