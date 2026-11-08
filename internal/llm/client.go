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
	"net/http"
	"strings"
	"time"
)

// Purposes a request can serve. They label the LLM metrics and nothing else:
// the purpose never changes what a provider sends.
const (
	PurposeDiagnosis = "diagnosis"
	PurposePlanning  = "planning"
)

const (
	providerClaude = "claude"
	providerGemini = "gemini"

	// defaultTimeout bounds one request when the caller names no timeout.
	defaultTimeout = 30 * time.Second

	// maxTimeout is the HTTP client's own ceiling, above any per-request timeout.
	maxTimeout = 2 * time.Minute

	// mimeJSON is the media type of every request body and of a structured reply.
	mimeJSON = "application/json"

	// defaultMaxTokens is the output budget when a request names none.
	defaultMaxTokens = 512
)

// Client is the one LLM provider interface. Diagnosis and the remediation planner
// both go through Complete, so there is one place that sends a request, one place
// that counts it (see Instrument), and one place a provider is swapped.
type Client interface {
	// Complete sends one request and returns the provider's reply.
	//
	// With a Schema the provider is asked for one JSON object, and a reply that
	// carries none comes back with an empty Response.JSON rather than an error:
	// the request was made and billed, and deciding that the reply is unusable is
	// the caller's validation. An error means the request itself failed.
	Complete(ctx context.Context, req Request) (*Response, error)

	// Provider returns the provider name (e.g., "claude", "gemini").
	Provider() string
}

// Request is one model call.
type Request struct {
	// Purpose is PurposeDiagnosis or PurposePlanning. Used for metrics only.
	Purpose string

	// System is the standing instruction.
	System string

	// User is the message the model answers.
	User string

	// MaxTokens is the output budget. Zero means defaultMaxTokens.
	MaxTokens int

	// Timeout bounds the request. Zero means defaultTimeout.
	Timeout time.Duration

	// Schema, when set, asks for a single JSON object conforming to it.
	Schema *Schema
}

// Schema describes the JSON object a structured request asks for.
type Schema struct {
	// Name identifies the structured reply (the tool name on Anthropic).
	Name string

	// Description says what the object is.
	Description string

	// Definition is the JSON Schema of the object.
	Definition map[string]any
}

// Response is a provider's reply.
type Response struct {
	// Text is the reply text. Informational only; nothing is decided from it.
	Text string

	// JSON is the structured reply when the request carried a Schema, empty when
	// the model produced none.
	JSON json.RawMessage

	// Usage is what the provider reported using.
	Usage Usage
}

// Usage is the token count a provider reports for one request.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Option configures NewClient.
type Option func(*clientOptions)

type clientOptions struct {
	model string
}

// WithModel overrides the provider's default model. An empty name keeps the
// default.
func WithModel(model string) Option {
	return func(o *clientOptions) { o.model = model }
}

func redactKey(text, apiKey string) string {
	if apiKey == "" {
		return text
	}
	return strings.ReplaceAll(text, apiKey, "[REDACTED]")
}

// NewClient creates an instrumented client for the named provider
// ("claude" or "gemini"). Every request it sends is counted in
// dorgu_llm_requests_total and dorgu_llm_tokens_total.
func NewClient(provider, apiKey string, opts ...Option) (Client, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("API key is required for LLM provider %q", provider)
	}
	var o clientOptions
	for _, opt := range opts {
		opt(&o)
	}

	httpClient := &http.Client{Timeout: maxTimeout}

	switch provider {
	case providerClaude:
		c := &ClaudeClient{apiKey: apiKey, model: defaultClaudeModel, client: httpClient}
		if o.model != "" {
			c.model = o.model
		}
		return Instrument(c), nil
	case providerGemini:
		g := &GeminiClient{apiKey: apiKey, model: defaultGeminiModel, client: httpClient}
		if o.model != "" {
			g.model = o.model
		}
		return Instrument(g), nil
	default:
		return nil, fmt.Errorf("unknown LLM provider %q (supported: claude, gemini)", provider)
	}
}

// maxResponseBytes caps how much of a provider reply is read. A reply is at most
// a few thousand tokens; anything near this is not one.
const maxResponseBytes = 4 << 20

// post sends one JSON request and returns the body of a 200 reply. Every error
// text has the API key redacted: transport errors and error bodies end up in
// logs.
func post(ctx context.Context, hc *http.Client, apiURL string, headers map[string]string, body any, apiKey string) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", mimeJSON)
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, errors.New("API call failed: " + redactKey(err.Error(), apiKey))
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, redactKey(string(respBody), apiKey))
	}
	return respBody, nil
}

// withTimeout applies the request's timeout, or the default.
func withTimeout(ctx context.Context, req Request) (context.Context, context.CancelFunc) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func maxTokensFor(req Request) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return defaultMaxTokens
}
