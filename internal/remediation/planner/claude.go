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
	"io"
	"net/http"
	"time"
)

const (
	// defaultClaudeModel is the default Anthropic model for planning.
	defaultClaudeModel = "claude-sonnet-4-6"

	// claudeAPIURL is the Anthropic Messages API endpoint.
	claudeAPIURL = "https://api.anthropic.com/v1/messages"

	// anthropicVersion is the required API version header.
	anthropicVersion = "2023-06-01"

	// planMaxTokens is the output budget for a plan — larger than the diagnosis
	// path's 512 because an ordered, multi-step plan with rationale needs room.
	planMaxTokens = 2048

	// defaultTimeout bounds a single planning HTTP request.
	defaultTimeout = 60 * time.Second

	// maxParseRetries is the number of extra attempts after a malformed response
	// (1 retry => up to 2 total attempts).
	maxParseRetries = 1
)

// ErrMalformedPlan indicates the model's structured output could not be parsed
// into a RemediationPlan after retries. The caller should fall back to rules.
var ErrMalformedPlan = errors.New("planner: malformed structured output from model")

// ClaudePlanner implements Planner against the Anthropic Messages API using
// forced tool use for structured output: the model is required to call the
// submit_remediation_plan tool, and the validated tool input is parsed directly
// into a RemediationPlan.
type ClaudePlanner struct {
	apiKey string
	model  string
	client *http.Client
	// baseURL overrides the API URL for testing.
	baseURL string
}

// NewClaudePlanner creates a ClaudePlanner. The apiKey must be non-empty.
func NewClaudePlanner(apiKey string) (*ClaudePlanner, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("planner: API key is required")
	}
	return &ClaudePlanner{
		apiKey: apiKey,
		model:  defaultClaudeModel,
		client: &http.Client{Timeout: defaultTimeout},
	}, nil
}

// SetModel overrides the default model.
func (c *ClaudePlanner) SetModel(model string) {
	if model != "" {
		c.model = model
	}
}

// PlanRemediation asks Claude for an ordered remediation plan. It issues one
// structured-output request and retries once on a malformed/empty response;
// transport or HTTP errors are returned immediately (no retry) so the caller
// can fall back to the rule-based proposer.
func (c *ClaudePlanner) PlanRemediation(ctx context.Context, rc RemediationContext) (*RemediationPlan, error) {
	body := claudeRequest{
		Model:     c.model,
		MaxTokens: planMaxTokens,
		System:    planSystemPrompt,
		Tools: []claudeTool{{
			Name:        planToolName,
			Description: planToolDescription,
			InputSchema: planToolSchema(),
			Strict:      true,
		}},
		ToolChoice: claudeToolChoice{Type: "tool", Name: planToolName},
		Messages: []claudeMessage{
			{Role: "user", Content: buildPlanUserMessage(rc)},
		},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("planner: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= maxParseRetries; attempt++ {
		plan, parseErr, callErr := c.callOnce(ctx, jsonBody)
		if callErr != nil {
			// Transport/HTTP error — not recoverable by retrying the parse.
			return nil, callErr
		}
		if parseErr == nil {
			return plan, nil
		}
		lastErr = parseErr
	}

	return nil, fmt.Errorf("%w: %v", ErrMalformedPlan, lastErr)
}

// callOnce performs a single API call. It returns (plan, parseErr, callErr):
// callErr is a hard transport/HTTP failure (do not retry); parseErr is a
// malformed-response failure (safe to retry).
func (c *ClaudePlanner) callOnce(ctx context.Context, jsonBody []byte) (*RemediationPlan, error, error) {
	apiURL := c.baseURL
	if apiURL == "" {
		apiURL = claudeAPIURL
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, nil, fmt.Errorf("planner: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("planner: API call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("planner: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("planner: API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var claudeResp claudeResponse
	if err := json.Unmarshal(respBody, &claudeResp); err != nil {
		return nil, fmt.Errorf("unmarshal response envelope: %w", err), nil
	}

	plan, parseErr := extractPlan(claudeResp)
	if parseErr != nil {
		return nil, parseErr, nil
	}
	return plan, nil, nil
}

// extractPlan finds the tool_use block and converts its validated input into a
// RemediationPlan.
func extractPlan(resp claudeResponse) (*RemediationPlan, error) {
	for _, block := range resp.Content {
		if block.Type != "tool_use" || block.Name != planToolName {
			continue
		}
		var raw rawPlan
		if err := json.Unmarshal(block.Input, &raw); err != nil {
			return nil, fmt.Errorf("unmarshal tool input: %w", err)
		}
		return raw.toPlan(), nil
	}
	return nil, fmt.Errorf("no %s tool_use block in response", planToolName)
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
	Order       int32  `json:"order"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Rationale   string `json:"rationale"`
	Risk        string `json:"risk"`
	Patch       string `json:"patch"`
	Command     string `json:"command"`
}

func (r rawPlan) toPlan() *RemediationPlan {
	plan := &RemediationPlan{
		RootCause:  r.RootCause,
		Confidence: r.Confidence,
		Steps:      make([]PlannedStep, 0, len(r.Steps)),
	}
	for _, s := range r.Steps {
		step := PlannedStep{
			Order:       s.Order,
			Type:        s.Type,
			Description: s.Description,
			Rationale:   s.Rationale,
			Risk:        s.Risk,
			Command:     s.Command,
		}
		if s.Patch != "" && json.Valid([]byte(s.Patch)) {
			step.Patch = json.RawMessage(s.Patch)
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan
}

// Anthropic Messages API types (tool-use subset).

type claudeRequest struct {
	Model      string           `json:"model"`
	MaxTokens  int              `json:"max_tokens"`
	System     string           `json:"system"`
	Tools      []claudeTool     `json:"tools"`
	ToolChoice claudeToolChoice `json:"tool_choice"`
	Messages   []claudeMessage  `json:"messages"`
}

type claudeTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
	Strict      bool           `json:"strict,omitempty"`
}

type claudeToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	Content    []claudeContent `json:"content"`
	StopReason string          `json:"stop_reason"`
}

type claudeContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}
