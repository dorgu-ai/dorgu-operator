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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	defaultClaudeModel = "claude-sonnet-4-6"
	claudeAPIURL       = "https://api.anthropic.com/v1/messages"
	anthropicVersion   = "2023-06-01"
)

// ClaudeClient implements Client against the Anthropic Messages API.
//
// A structured request uses forced tool use: the model must call the tool named
// by the Schema, and the tool input is the reply, parsed by the caller.
type ClaudeClient struct {
	apiKey string
	model  string
	client *http.Client
	// baseURL allows overriding the API URL for testing.
	baseURL string
}

// Provider returns "claude".
func (c *ClaudeClient) Provider() string { return providerClaude }

// SetModel overrides the default model.
func (c *ClaudeClient) SetModel(model string) { c.model = model }

// Complete sends one Messages API request.
func (c *ClaudeClient) Complete(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := withTimeout(ctx, req)
	defer cancel()

	body := claudeRequest{
		Model:     c.model,
		MaxTokens: maxTokensFor(req),
		System:    req.System,
		Messages:  []claudeMessage{{Role: "user", Content: req.User}},
	}
	if req.Schema != nil {
		body.Tools = []claudeTool{{
			Name:        req.Schema.Name,
			Description: req.Schema.Description,
			InputSchema: req.Schema.Definition,
			Strict:      true,
		}}
		body.ToolChoice = &claudeToolChoice{Type: "tool", Name: req.Schema.Name}
	}

	apiURL := c.baseURL
	if apiURL == "" {
		apiURL = claudeAPIURL
	}
	respBody, err := post(ctx, c.client, apiURL, map[string]string{
		"x-api-key":         c.apiKey,
		"anthropic-version": anthropicVersion,
	}, body, c.apiKey)
	if err != nil {
		return nil, err
	}

	var cr claudeResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if len(cr.Content) == 0 {
		return nil, fmt.Errorf("empty response from Claude API")
	}

	out := &Response{Usage: Usage{InputTokens: cr.Usage.InputTokens, OutputTokens: cr.Usage.OutputTokens}}
	var text []string
	for _, block := range cr.Content {
		switch {
		case block.Type == "text":
			text = append(text, block.Text)
		case block.Type == "tool_use" && req.Schema != nil && block.Name == req.Schema.Name && out.JSON == nil:
			out.JSON = block.Input
		}
	}
	out.Text = strings.TrimSpace(strings.Join(text, "\n"))
	return out, nil
}

type claudeRequest struct {
	Model      string            `json:"model"`
	MaxTokens  int               `json:"max_tokens"`
	System     string            `json:"system"`
	Tools      []claudeTool      `json:"tools,omitempty"`
	ToolChoice *claudeToolChoice `json:"tool_choice,omitempty"`
	Messages   []claudeMessage   `json:"messages"`
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
	Content []claudeContent `json:"content"`
	Usage   claudeUsage     `json:"usage"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type claudeContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}
