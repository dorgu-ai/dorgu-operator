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

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// Actions a diagnosis may recommend. The set is closed: a reply naming anything
// else is invalid. It mirrors the SuggestedAction vocabulary of the rules, but the
// model's choice is advisory only: the rules own SuggestedAction and with it the
// deduplication key (KPI I5).
const (
	ActionResourceAdjustment = "resource-adjustment"
	ActionRestart            = "restart"
	ActionRollback           = "rollback"
	ActionScaleUp            = "scale-up"
	ActionDeploymentFix      = "deployment-fix"
	ActionInvestigate        = "investigate"
)

// RecommendedActions lists the allowed values of recommendedAction.
func RecommendedActions() []string {
	return []string{
		ActionResourceAdjustment, ActionRestart, ActionRollback,
		ActionScaleUp, ActionDeploymentFix, ActionInvestigate,
	}
}

const (
	// diagnosisSchemaName names the structured reply.
	diagnosisSchemaName = "submit_diagnosis"

	// diagnosisMaxTokens is the output budget for a diagnosis.
	diagnosisMaxTokens = 512

	// maxSummaryRunes and maxRootCauseRunes bound what a reply may put on an
	// incident. Over-long text is rejected rather than truncated: a model that
	// ignores a length contract has not followed the schema.
	maxSummaryRunes   = 1200
	maxRootCauseRunes = 600
)

// ErrInvalidOutput means the model replied, and the reply does not satisfy the
// diagnosis schema. The caller falls back to the rule-based result.
var ErrInvalidOutput = errors.New("llm: model reply does not match the schema")

// DiagnosisResponse is the validated structured reply to a diagnosis request.
type DiagnosisResponse struct {
	// Summary is the explanation shown on the incident.
	Summary string `json:"summary"`

	// RootCause is the model's one-sentence root cause. Informational.
	RootCause string `json:"rootCause"`

	// RecommendedAction is one of RecommendedActions. Advisory only.
	RecommendedAction string `json:"recommendedAction"`

	// Confidence is the model's own confidence, 0 to 1. Advisory only: the
	// diagnosis keeps the rule-based confidence.
	Confidence float64 `json:"confidence"`
}

// diagnosisSchema is the JSON Schema of DiagnosisResponse. It sticks to what
// Anthropic's strict tool schemas accept, so range and length limits are enforced
// by ParseDiagnosis instead.
func diagnosisSchema() *Schema {
	actions := RecommendedActions()
	enum := make([]any, len(actions))
	for i, a := range actions {
		enum[i] = a
	}
	return &Schema{
		Name:        diagnosisSchemaName,
		Description: "Submit the incident diagnosis.",
		Definition: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary":           map[string]any{"type": "string", "description": "2-4 sentence actionable explanation."},
				"rootCause":         map[string]any{"type": "string", "description": "The most likely root cause, one sentence."},
				"recommendedAction": map[string]any{"type": "string", "enum": enum},
				"confidence":        map[string]any{"type": "number", "description": "Confidence from 0 to 1."},
			},
			"required":             []any{"summary", "rootCause", "recommendedAction", "confidence"},
			"additionalProperties": false,
		},
	}
}

// EnhanceDiagnosis asks the model to explain a rule-based diagnosis and returns
// the validated reply.
//
// An error from the request itself is returned as is. A reply that does not
// satisfy the schema returns ErrInvalidOutput and is counted in
// dorgu_llm_invalid_output_total; the caller keeps the rule-based result. Nothing
// is parsed out of free text.
func EnhanceDiagnosis(ctx context.Context, c Client, req DiagnosisRequest) (*DiagnosisResponse, error) {
	resp, err := c.Complete(ctx, Request{
		Purpose:   PurposeDiagnosis,
		System:    diagnosisSystemPrompt,
		User:      buildUserMessage(req),
		MaxTokens: diagnosisMaxTokens,
		Schema:    diagnosisSchema(),
	})
	if err != nil {
		return nil, err
	}

	parsed, err := ParseDiagnosis(resp.JSON)
	if err != nil {
		metrics.RecordLLMInvalidOutput(c.Provider(), PurposeDiagnosis)
		return nil, err
	}
	return parsed, nil
}

// diagnosisKeys are the four keys a reply must carry, matched exactly.
var diagnosisKeys = []string{"summary", "rootCause", "recommendedAction", "confidence"}

// ParseDiagnosis validates a structured reply against the diagnosis schema: one
// JSON object, exactly the four fields (keys are case-sensitive and may not
// repeat), a known action, a confidence in [0, 1], and bounded text without
// control characters. Every failure wraps ErrInvalidOutput.
func ParseDiagnosis(raw []byte) (*DiagnosisResponse, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: no structured reply", ErrInvalidOutput)
	}
	fields, err := strictObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}

	var out DiagnosisResponse
	for _, f := range []struct {
		key string
		dst any
	}{
		{"summary", &out.Summary},
		{"rootCause", &out.RootCause},
		{"recommendedAction", &out.RecommendedAction},
		{"confidence", &out.Confidence},
	} {
		if err := json.Unmarshal(fields[f.key], f.dst); err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrInvalidOutput, f.key, err)
		}
	}

	// Control characters are checked before trimming, on the decoded text, so an
	// escape written as \u001b is caught like a raw one.
	if c, bad := firstControl(out.Summary); bad {
		return nil, fmt.Errorf("%w: summary contains the control character %U", ErrInvalidOutput, c)
	}
	if c, bad := firstControl(out.RootCause); bad {
		return nil, fmt.Errorf("%w: rootCause contains the control character %U", ErrInvalidOutput, c)
	}

	out.Summary = strings.TrimSpace(out.Summary)
	out.RootCause = strings.TrimSpace(out.RootCause)
	switch {
	case out.Summary == "":
		return nil, fmt.Errorf("%w: summary is empty", ErrInvalidOutput)
	case utf8.RuneCountInString(out.Summary) > maxSummaryRunes:
		return nil, fmt.Errorf("%w: summary is longer than %d characters", ErrInvalidOutput, maxSummaryRunes)
	case out.RootCause == "":
		return nil, fmt.Errorf("%w: rootCause is empty", ErrInvalidOutput)
	case utf8.RuneCountInString(out.RootCause) > maxRootCauseRunes:
		return nil, fmt.Errorf("%w: rootCause is longer than %d characters", ErrInvalidOutput, maxRootCauseRunes)
	case !validAction(out.RecommendedAction):
		return nil, fmt.Errorf("%w: recommendedAction %q is not one of the allowed values", ErrInvalidOutput, out.RecommendedAction)
	case math.IsNaN(out.Confidence) || out.Confidence < 0 || out.Confidence > 1:
		return nil, fmt.Errorf("%w: confidence %v is outside [0, 1]", ErrInvalidOutput, out.Confidence)
	}
	return &out, nil
}

// strictObject reads raw as one JSON object, token by token, and returns its
// members. encoding/json cannot do this: it matches keys to struct fields
// case-insensitively and lets the last of a repeated key win, so a reply of
// {"summary":"a","SUMMARY":"b"} would be read as "b" by Go and as "a" by anything
// that looks at the text. Here a key is taken exactly as written, a repeat is an
// error, and any key outside diagnosisKeys (including a case variant of one) is
// an error, as is a missing one.
func strictObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("the reply is not a JSON object")
	}

	fields := make(map[string]json.RawMessage, len(diagnosisKeys))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("object key is not a string")
		}
		if !slices.Contains(diagnosisKeys, key) {
			return nil, fmt.Errorf("unexpected key %q (keys are case-sensitive)", key)
		}
		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the object")
	}

	for _, k := range diagnosisKeys {
		if _, ok := fields[k]; !ok {
			return nil, fmt.Errorf("missing %q", k)
		}
	}
	return fields, nil
}

// firstControl returns the first control character in s other than a newline or
// a tab: C0 (which holds ESC, BEL and CR, the carriers of ANSI CSI and OSC
// sequences and of line overwriting), DEL, and C1 (U+0080 to U+009F, whose
// U+009B and U+009D are single-character CSI and OSC on some terminals).
func firstControl(s string) (rune, bool) {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return r, true
		}
	}
	return 0, false
}

func validAction(a string) bool { return slices.Contains(RecommendedActions(), a) }
