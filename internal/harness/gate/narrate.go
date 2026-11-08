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
	"slices"
	"strings"
	"unicode"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// AbsentKeyNote explains, in the plan itself, which fields Dorgu declined to
// introduce and why. Silence here would be the same silent surprise as adding
// them.
func AbsentKeyNote(ref *dorguv1.WorkloadRef, dropped []string) string {
	names := make([]string, 0, len(dropped))
	for _, path := range dropped {
		names = append(names, path[strings.LastIndex(path, ".")+1:]+" ("+sectionOf(path)+")")
	}
	slices.Sort(names)
	return fmt.Sprintf(
		"Left out %s: container %q on %s does not set %s today, and introducing a resource field as a side effect of another fix changes how the workload is scheduled and throttled. Ask for it as its own change if you want it.",
		HumanFieldList(names), ref.Container, ref.Name,
		pluralField(len(names)))
}

// sectionOf names the resource section a persona-spec path belongs to.
func sectionOf(path string) string {
	if strings.HasPrefix(path, PathPrefixRequests) {
		return "requests"
	}
	return "limits"
}

func pluralField(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// recordAbsentFields is F-05 expressed as structured data alongside its prose:
// a resource key the live container does not set was removed from the patch, so
// the step will not introduce it.
//
// Recording it matters beyond rendering. dropPatchlessPersonaUpdates uses the
// presence of a Safety entry to tell an empty step Dorgu has something to say
// about from an empty step that is simply noise, and this is a step of the first
// kind: the reader learns which field Dorgu declined to add, and why.
func recordAbsentFields(
	step *dorguv1.RemediationStep,
	requested *apiextensionsv1.JSON,
	dropped []string,
	ref *dorguv1.WorkloadRef,
) {
	values := PatchLeafValues(requested.Raw)
	for _, field := range dropped {
		leaf := field[strings.LastIndex(field, ".")+1:]
		step.Safety = append(step.Safety, dorguv1.StepSafety{
			Rule:      dorguv1.SafetyRuleAbsentField,
			Verdict:   dorguv1.SafetyVerdictRejected,
			Field:     field,
			Requested: values[field],
			Message: fmt.Sprintf(
				"Left out: container %q on %s does not set %s today, so Dorgu will not introduce it as a side effect of another fix. Ask for it as its own change.",
				ref.Container, ref.Name, leaf),
		})
	}
}

// refuseStepFields applies the blast-radius guardrail's refusals to a step.
//
// CR-04: the guardrail fired correctly and was then reported incoherently. The
// verdict was a "[safety:blast-radius] …" prefix spliced onto the model's own
// rationale, so a computed refusal read as part of the model's reasoning, one
// line below the model's claim that the same 16x change was "well within a 2x
// ceiling". The step rendered "(no changes)" while the headline diff still
// advertised the refused value.
//
// So: the verdict goes in Safety as numbers, the refused field is removed from
// the patch so nothing can advertise a change that will not happen, and Dorgu
// writes the sentences on any step a guardrail has touched. A step keeps its
// remaining fields and stays auto-executable if any survive.
func refuseStepFields(step *dorguv1.RemediationStep, refusals []Violation) {
	if len(refusals) == 0 {
		return
	}

	fields := make([]string, 0, len(refusals))
	for _, v := range refusals {
		entry := dorguv1.StepSafety{
			Rule:    dorguv1.SafetyRuleBlastRadius,
			Verdict: dorguv1.SafetyVerdictRejected,
			Message: v.Message,
		}
		if d := v.BlastRadius; d != nil {
			entry.Field = d.Field
			entry.Baseline = d.Baseline
			entry.Requested = d.Requested
			entry.Ratio = FormatRatio(d.Ratio)
			entry.MaxRatio = FormatRatio(d.MaxRatio)
			entry.Message = BlastRadiusRejectedMessage(entry)
			fields = append(fields, d.Field)
		}
		step.Safety = append(step.Safety, entry)
	}

	step.Patch = PrunePatchPaths(step.Patch, fields)
	step.PrePatchState = PrunePatchPaths(step.PrePatchState, fields)
	if step.Patch == nil {
		step.AutoExecutable = false
	}

	step.Description = DorguAuthoredDescription(describeStepPatch(step.Patch), step.Safety)
	step.Rationale = GuardedRationale()
}

// AppendNote joins a note onto existing prose without doubling separators or
// repeating a note that is already there.
//
// A missing full stop is supplied. Without one the two runs together into a
// single sentence — "…1 applied on approval and 1 advisory Clamped by the 2x
// blast-radius guardrail…" is what the clean room read — and a reader is not
// the only casualty: scrubRefusedValues redacts by sentence, so a model
// sentence fused to a Dorgu-authored note cannot be removed without taking the
// note with it.
func AppendNote(existing, note string) string {
	if existing == "" {
		return note
	}
	if strings.Contains(existing, note) {
		return existing
	}
	return endSentence(existing) + " " + note
}

// endSentence terminates prose that does not terminate itself. Text already
// ending in punctuation is left exactly as it is.
func endSentence(text string) string {
	trimmed := strings.TrimRight(text, " ")
	if trimmed == "" {
		return trimmed
	}
	last := []rune(trimmed)[len([]rune(trimmed))-1]
	if unicode.IsLetter(last) || unicode.IsDigit(last) {
		return trimmed + "."
	}
	return trimmed
}
