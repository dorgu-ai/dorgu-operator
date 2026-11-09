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

// commandRemoved stands where a refused command's text was cut out of a sentence.
const commandRemoved = "[command removed]"

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
		if result.PlanRefusal == nil {
			result.PlanRefusal = refusal
		}
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

// scrubDroppedCommands cuts the refused commands' text out of every surface of the plan
// that is kept, and says that steps were not proposed. It returns how many surfaces it
// changed.
func scrubDroppedCommands(action *dorguv1.RemediationAction, commands []string) int {
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
		step.Command = cut(step.Command)
	}
	action.Spec.PlanSummary = cut(action.Spec.PlanSummary)
	action.Spec.Explanation = cut(action.Spec.Explanation)
	action.Spec.Explanation = gate.AppendNote(action.Spec.Explanation, droppedCommandsNote)
	return scrubbed
}

// removeCommandText replaces each occurrence of a refused command with a marker and
// reports whether it changed anything. The match ignores case and how much whitespace
// separates the words, because a model that quotes a command does not keep its spacing.
func removeCommandText(text string, commands []string) (string, bool) {
	out := text
	for _, command := range commands {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			continue
		}
		quoted := make([]string, len(fields))
		for i, f := range fields {
			quoted[i] = regexp.QuoteMeta(f)
		}
		re, err := regexp.Compile(`(?i)` + strings.Join(quoted, `\s+`))
		if err != nil {
			continue
		}
		out = re.ReplaceAllLiteralString(out, commandRemoved)
	}
	return out, out != text
}

// mentionsAnyCommand reports whether any refused command is still anywhere in the plan's
// stored form, compared with whitespace collapsed and case folded, so a re-spaced or
// re-cased quotation counts as a leak.
func mentionsAnyCommand(action *dorguv1.RemediationAction, commands []string) bool {
	if len(commands) == 0 {
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
	for _, s := range strs {
		haystack := normalizeCommandText(s)
		for _, command := range commands {
			if needle := normalizeCommandText(command); needle != "" && strings.Contains(haystack, needle) {
				return true
			}
		}
	}
	return false
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

func normalizeCommandText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
