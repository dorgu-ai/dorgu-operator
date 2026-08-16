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
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	defaultClaudeModel = "claude-sonnet-4-6"
	claudeAPIURL       = "https://api.anthropic.com/v1/messages"
	anthropicVersion   = "2023-06-01"
	maxTokens          = 512
)

// ClaudeClient implements the Client interface using the Anthropic Messages API.
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

// EnhanceDiagnosis calls the Anthropic Messages API to enhance a rule-based diagnosis.
func (c *ClaudeClient) EnhanceDiagnosis(ctx context.Context, req DiagnosisRequest) (*DiagnosisResponse, error) {
	body := claudeRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Messages: []claudeMessage{
			{Role: "user", Content: buildUserMessage(req)},
		},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	apiURL := c.baseURL
	if apiURL == "" {
		apiURL = claudeAPIURL
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var claudeResp claudeResponse
	if err := json.Unmarshal(respBody, &claudeResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if len(claudeResp.Content) == 0 {
		return nil, fmt.Errorf("empty response from Claude API")
	}

	text := claudeResp.Content[0].Text
	return parseEnhancedResponse(text), nil
}

// parseEnhancedResponse extracts the enhanced summary from LLM text output.
// The LLM returns free-form text; we use the full response as the enhanced summary
// and extract any recommended action if present.
func parseEnhancedResponse(text string) *DiagnosisResponse {
	resp := &DiagnosisResponse{
		EnhancedSummary: strings.TrimSpace(text),
	}

	// Look for an explicit action recommendation in the text.
	lower := strings.ToLower(text)
	for _, action := range []string{"resource-adjustment", "restart", "rollback", "scale-up", "deployment-fix", "investigate"} {
		if strings.Contains(lower, action) {
			resp.RecommendedAction = action
			break
		}
	}

	return resp
}

// Claude API types

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system"`
	Messages  []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
