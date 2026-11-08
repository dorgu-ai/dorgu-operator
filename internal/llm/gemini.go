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
	defaultGeminiModel = "gemini-2.0-flash"
	geminiAPIURLFmt    = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"
	// geminiAPIKeyHeader carries the key. It never goes in the URL: URLs end up
	// in proxy logs and in *url.Error messages, and from there in ours.
	geminiAPIKeyHeader = "x-goog-api-key"
)

// GeminiClient implements Client using the Google Gemini API.
//
// A structured request sets the JSON response MIME type and states the schema in
// the system instruction. Gemini's response-schema dialect is narrower than JSON
// Schema, so the schema is not sent as one; the caller validates the reply.
type GeminiClient struct {
	apiKey string
	model  string
	client *http.Client
	// baseURL allows overriding the API URL for testing.
	baseURL string
}

// Provider returns "gemini".
func (g *GeminiClient) Provider() string { return providerGemini }

// SetModel overrides the default model.
func (g *GeminiClient) SetModel(model string) { g.model = model }

// Complete calls the Gemini generateContent API.
func (g *GeminiClient) Complete(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := withTimeout(ctx, req)
	defer cancel()

	system := req.System
	body := geminiRequest{
		Contents:         []geminiContent{{Parts: []geminiPart{{Text: req.User}}}},
		GenerationConfig: geminiGenerationConfig{MaxOutputTokens: maxTokensFor(req)},
	}
	if req.Schema != nil {
		schema, err := json.Marshal(req.Schema.Definition)
		if err != nil {
			return nil, fmt.Errorf("marshal schema: %w", err)
		}
		system += "\n\nReply with a single JSON object that conforms to this JSON Schema, and nothing else:\n" + string(schema)
		body.GenerationConfig.ResponseMimeType = mimeJSON
	}
	if system != "" {
		body.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: system}}}
	}

	apiURL := g.baseURL
	if apiURL == "" {
		apiURL = fmt.Sprintf(geminiAPIURLFmt, g.model)
	}
	respBody, err := post(ctx, g.client, apiURL, map[string]string{geminiAPIKeyHeader: g.apiKey}, body, g.apiKey)
	if err != nil {
		return nil, err
	}

	var gr geminiResponse
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty response from Gemini API")
	}

	parts := gr.Candidates[0].Content.Parts
	text := make([]string, 0, len(parts))
	for _, part := range parts {
		text = append(text, part.Text)
	}
	out := &Response{
		Text: strings.TrimSpace(strings.Join(text, "")),
		Usage: Usage{
			InputTokens:  gr.UsageMetadata.PromptTokenCount,
			OutputTokens: gr.UsageMetadata.CandidatesTokenCount,
		},
	}
	if req.Schema != nil && json.Valid([]byte(out.Text)) {
		out.JSON = json.RawMessage(out.Text)
	}
	return out, nil
}

// Gemini API types

type geminiRequest struct {
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	Contents          []geminiContent        `json:"contents"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens  int    `json:"maxOutputTokens,omitempty"`
	ResponseMimeType string `json:"responseMimeType,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata geminiUsage       `json:"usageMetadata"`
}

type geminiCandidate struct {
	Content geminiContent `json:"content"`
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
}
