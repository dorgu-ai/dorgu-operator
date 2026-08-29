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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ClampedConfidenceDamping scales a plan's confidence when the blast-radius
// guardrail bound it. A truncated fix is a fix that may not be enough, and
// stating 0.88 for one flatly is what turned F-11 into a confident wrong answer.
const ClampedConfidenceDamping = 0.85

// clampEpsilon absorbs the floating-point noise in comparing a ratio to the cap.
const clampEpsilon = 1e-9

// discloseBlastRadiusClamp annotates a plan whose proposed resource values sit at
// the 2x blast-radius ceiling, and damps its confidence.
//
// The cap is real but it used to be invisible: report-worker needed ~120M, the
// plan proposed 48Mi to 96Mi at confidence 0.88 with a summary asserting the
// increase resolves the OOM, and the pod went straight back to OOMKilled. The
// headline summary, the confidence and the diff never mentioned that 96Mi was
// simply as far as the guardrail allowed (F-11).
//
// Returns true when the plan was annotated.
func discloseBlastRadiusClamp(action *dorguv1.RemediationAction) bool {
	if action == nil {
		return false
	}

	clamped := make(map[string]bool)

	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		if !step.AutoExecutable || step.Type != dorguv1.StepTypePersonaUpdate {
			continue
		}
		fields := fieldsAtBlastRadiusCap(step.Patch, step.PrePatchState)
		if len(fields) == 0 {
			continue
		}
		for _, f := range fields {
			clamped[f] = true
		}
		step.Rationale = appendNote(step.Rationale, blastRadiusCaveat(fields))
	}

	for _, f := range fieldsAtBlastRadiusCap(action.Spec.Action.Patch, action.Spec.Action.PrePatchState) {
		clamped[f] = true
	}

	if len(clamped) == 0 {
		return false
	}

	caveat := blastRadiusCaveat(sortedKeys(clamped))
	action.Spec.Explanation = appendNote(action.Spec.Explanation, caveat)
	if action.Spec.PlanSummary != "" {
		action.Spec.PlanSummary = appendNote(action.Spec.PlanSummary, caveat)
	}
	action.Spec.Confidence = dampConfidence(action.Spec.Confidence)

	return true
}

// fieldsAtBlastRadiusCap returns the resource fields whose proposed value lands
// at (or beyond) the maximum increase the guardrail permits, meaning the cap, not
// the diagnosis, chose the number.
func fieldsAtBlastRadiusCap(patch, prePatch *apiextensionsv1.JSON) []string {
	if patch == nil || prePatch == nil || len(patch.Raw) == 0 || len(prePatch.Raw) == 0 {
		return nil
	}

	patchValues, err := parseResourcePatch(patch.Raw)
	if err != nil {
		return nil
	}
	prePatchValues, err := parseResourcePatch(prePatch.Raw)
	if err != nil {
		return nil
	}

	var fields []string
	for field, newVal := range patchValues {
		oldVal, ok := prePatchValues[field]
		if !ok {
			continue
		}
		oldQty := oldVal.AsApproximateFloat64()
		if oldQty <= 0 {
			continue
		}
		if newVal.AsApproximateFloat64()/oldQty >= MaxBlastRadiusMultiplier-clampEpsilon {
			fields = append(fields, field)
		}
	}

	sort.Strings(fields)
	return fields
}

// blastRadiusCaveat renders the disclosure shown in the plan summary, the
// explanation and, through them, `dorgu remediation diff`.
func blastRadiusCaveat(fields []string) string {
	return fmt.Sprintf(
		"Clamped by the %.0fx blast-radius guardrail: %s could not be raised further in one step, "+
			"so this may be insufficient and a second increase may be required.",
		MaxBlastRadiusMultiplier, humanFieldList(fields))
}

// humanFieldList renders field paths as prose: "a", "a and b", "a, b and c".
func humanFieldList(fields []string) string {
	switch len(fields) {
	case 0:
		return "the proposed value"
	case 1:
		return fields[0]
	default:
		return strings.Join(fields[:len(fields)-1], ", ") + " and " + fields[len(fields)-1]
	}
}

// dampConfidence lowers a confidence string by ClampedConfidenceDamping, keeping
// the CRD's two-decimal format. An unparseable value is left alone.
func dampConfidence(confidence string) string {
	parsed, err := strconv.ParseFloat(confidence, 64)
	if err != nil {
		return confidence
	}
	return fmt.Sprintf("%.2f", parsed*ClampedConfidenceDamping)
}

// appendNote joins a note onto existing prose without doubling separators or
// repeating a note that is already there.
//
// A missing full stop is supplied. Without one the two runs together into a
// single sentence — "…1 applied on approval and 1 advisory Clamped by the 2x
// blast-radius guardrail…" is what the clean room read — and a reader is not
// the only casualty: scrubRefusedValues redacts by sentence, so a model
// sentence fused to a Dorgu-authored note cannot be removed without taking the
// note with it.
func appendNote(existing, note string) string {
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

// sortedKeys returns a set's keys in a stable order.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
