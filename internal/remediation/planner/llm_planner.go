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

package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

const (
	// planMaxTokens is the output budget for a plan — larger than the diagnosis
	// path's because an ordered, multi-step plan with rationale needs room.
	planMaxTokens = 2048

	// planTimeout bounds a single planning request.
	planTimeout = 60 * time.Second

	// maxParseRetries is the number of extra attempts after a malformed response
	// (1 retry => up to 2 total attempts).
	maxParseRetries = 1
)

// ErrMalformedPlan indicates the model's structured output could not be parsed
// into a RemediationPlan after retries. The caller should fall back to rules.
var ErrMalformedPlan = errors.New("planner: malformed structured output from model")

// LLMPlanner implements Planner over the one llm.Client interface, the same one
// diagnosis uses. The model is required to submit its plan through a structured
// reply (forced tool use on Anthropic, a JSON response on Gemini), and the reply
// is parsed directly into a RemediationPlan.
//
// The plan is only ever a draft: every step is checked by the gate before
// anything is proposed, and the model's free text is never parsed for meaning.
type LLMPlanner struct {
	client llm.Client
}

// New creates an LLMPlanner over client.
func New(client llm.Client) (*LLMPlanner, error) {
	if client == nil {
		return nil, fmt.Errorf("planner: an LLM client is required")
	}
	return &LLMPlanner{client: client}, nil
}

// PlanRemediation asks the model for an ordered remediation plan. It issues one
// structured request and retries once on a malformed or empty reply; a failed
// request is returned immediately (no retry) so the caller can fall back to the
// rule-based proposer.
func (p *LLMPlanner) PlanRemediation(ctx context.Context, rc RemediationContext) (*RemediationPlan, error) {
	req := llm.Request{
		Purpose:   llm.PurposePlanning,
		System:    planSystemPrompt,
		User:      buildPlanUserMessage(rc),
		MaxTokens: planMaxTokens,
		Timeout:   planTimeout,
		Schema: &llm.Schema{
			Name:        planToolName,
			Description: planToolDescription,
			Definition:  planToolSchema(),
		},
	}

	var lastErr error
	for attempt := 0; attempt <= maxParseRetries; attempt++ {
		resp, err := p.client.Complete(ctx, req)
		if err != nil {
			// A failed request is not recoverable by retrying the parse.
			return nil, fmt.Errorf("planner: %w", err)
		}
		plan, parseErr := extractPlan(resp.JSON)
		if parseErr == nil {
			return plan, nil
		}
		// The model replied and the reply is not a usable plan. Counted like an invalid
		// diagnosis reply, so a model that keeps answering with nothing shows on the dashboard.
		metrics.RecordLLMInvalidOutput(p.client.Provider(), llm.PurposePlanning)
		lastErr = parseErr
	}

	return nil, fmt.Errorf("%w: %v", ErrMalformedPlan, lastErr)
}

// extractPlan converts the structured reply into a RemediationPlan.
func extractPlan(raw json.RawMessage) (*RemediationPlan, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("no %s structured reply", planToolName)
	}
	var plan rawPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("unmarshal tool input: %w", err)
	}
	result := plan.toPlan()
	if len(result.Steps) == 0 {
		// A plan with no steps proposes nothing. It used to parse as a success and be cached
		// for the whole cache TTL (D1.1 finding 7); it is an unusable reply, asked once more.
		return nil, fmt.Errorf("the plan has no steps")
	}
	return result, nil
}

// rawPlan mirrors the tool schema. patch is a JSON-encoded string in the schema
// (kept a string so the schema stays strictly validatable); it is converted to
// json.RawMessage when valid.
type rawPlan struct {
	RootCause  string    `json:"rootCause"`
	Confidence float64   `json:"confidence"`
	Steps      []rawStep `json:"steps"`
}

type rawStep struct {
	Order       int32     `json:"order"`
	Type        string    `json:"type"`
	Description string    `json:"description"`
	Rationale   string    `json:"rationale"`
	Risk        string    `json:"risk"`
	Patch       planPatch `json:"patch"`
	Command     string    `json:"command"`
}

// planPatch decodes a step's patch from either shape the model produces.
//
// The tool schema asks for a JSON-encoded string, because that is what stays
// strictly validatable. A model that has just been instructed to emit a merge
// patch will sometimes emit the object itself instead, and decoding an object
// into a string field fails the whole response, not the one field: the plan is
// retried once, fails identically, and the entire AI path degrades to rules for
// a plan that was otherwise fine. Both shapes are accepted here.
//
// An unusable value leaves the patch empty rather than erroring. A step with no
// patch is handled downstream, where the proposer either supplies a
// deterministic one or refuses the plan; failing the decode would instead throw
// away the model's diagnosis along with its bad field.
type planPatch struct {
	raw json.RawMessage
}

// UnmarshalJSON implements json.Unmarshaler for the string-or-object patch.
func (p *planPatch) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	if trimmed[0] == '"' {
		var encoded string
		if err := json.Unmarshal(trimmed, &encoded); err != nil {
			return nil
		}
		if encoded == "" || !json.Valid([]byte(encoded)) {
			return nil
		}
		p.raw = json.RawMessage(encoded)
		return nil
	}

	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil
	}
	p.raw = append(json.RawMessage(nil), trimmed...)
	return nil
}

func (r rawPlan) toPlan() *RemediationPlan {
	plan := &RemediationPlan{
		RootCause:  r.RootCause,
		Confidence: r.Confidence,
		Steps:      make([]PlannedStep, 0, len(r.Steps)),
	}
	for _, s := range r.Steps {
		plan.Steps = append(plan.Steps, PlannedStep{
			Order:       s.Order,
			Type:        s.Type,
			Description: s.Description,
			Rationale:   s.Rationale,
			Risk:        s.Risk,
			Command:     s.Command,
			Patch:       s.Patch.raw,
		})
	}
	return plan
}
