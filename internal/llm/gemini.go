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
)

const (
	defaultGeminiModel = "gemini-2.0-flash"
	geminiAPIURLFmt    = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s"
)

// GeminiClient implements the Client interface using the Google Gemini API.
type GeminiClient struct {
	apiKey string
	model  string
	client *http.Client
	// baseURL allows overriding the API URL for testing. When set, apiKey is appended as ?key=.
	baseURL string
}

// Provider returns "gemini".
func (g *GeminiClient) Provider() string { return providerGemini }

// SetModel overrides the default model.
func (g *GeminiClient) SetModel(model string) { g.model = model }

// EnhanceDiagnosis calls the Gemini generateContent API to enhance a rule-based diagnosis.
func (g *GeminiClient) EnhanceDiagnosis(ctx context.Context, req DiagnosisRequest) (*DiagnosisResponse, error) {
	userMessage := buildUserMessage(req)
	prompt := systemPrompt + "\n\n" + userMessage

	body := geminiRequest{
		Contents: []geminiContent{
			{
				Parts: []geminiPart{
					{Text: prompt},
				},
			},
		},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	apiURL := g.baseURL
	if apiURL == "" {
		apiURL = fmt.Sprintf(geminiAPIURLFmt, g.model, g.apiKey)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
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

	var geminiResp geminiResponse
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if len(geminiResp.Candidates) == 0 ||
		len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty response from Gemini API")
	}

	text := geminiResp.Candidates[0].Content.Parts[0].Text
	return parseEnhancedResponse(text), nil
}

// Gemini API types

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates []geminiCandidate `json:"candidates"`
}

type geminiCandidate struct {
	Content geminiContent `json:"content"`
}
