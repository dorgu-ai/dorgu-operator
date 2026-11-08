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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newClaudeAt(url string, hc *http.Client) *ClaudeClient {
	return &ClaudeClient{apiKey: "test-key", model: defaultClaudeModel, client: hc, baseURL: url}
}

func TestClaudeClient_Complete_TextRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != anthropicVersion {
			t.Errorf("anthropic-version = %q", r.Header.Get("anthropic-version"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}

		var body claudeRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Model != defaultClaudeModel || body.System != "be brief" || body.MaxTokens != 77 {
			t.Errorf("request = %+v", body)
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != "hello" {
			t.Errorf("messages = %+v", body.Messages)
		}
		if len(body.Tools) != 0 || body.ToolChoice != nil {
			t.Errorf("an unstructured request must not force a tool: %+v", body)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(claudeResponse{
			Content: []claudeContent{{Type: "text", Text: " hi "}, {Type: "text", Text: "there"}},
			Usage:   claudeUsage{InputTokens: 11, OutputTokens: 22},
		})
	}))
	defer server.Close()

	resp, err := newClaudeAt(server.URL, server.Client()).Complete(context.Background(),
		Request{System: "be brief", User: "hello", MaxTokens: 77})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "hi \nthere" {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.JSON != nil {
		t.Errorf("json = %s, want none", resp.JSON)
	}
	if resp.Usage != (Usage{InputTokens: 11, OutputTokens: 22}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestClaudeClient_Complete_StructuredRequestForcesTheTool(t *testing.T) {
	schema := &Schema{Name: "submit_x", Description: "d", Definition: map[string]any{"type": "object"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body claudeRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.ToolChoice == nil || body.ToolChoice.Type != "tool" || body.ToolChoice.Name != "submit_x" {
			t.Errorf("tool_choice = %+v, want forced submit_x", body.ToolChoice)
		}
		if len(body.Tools) != 1 || !body.Tools[0].Strict || body.Tools[0].Name != "submit_x" {
			t.Errorf("tools = %+v, want one strict tool", body.Tools)
		}
		if body.MaxTokens != defaultMaxTokens {
			t.Errorf("max_tokens = %d, want the default %d", body.MaxTokens, defaultMaxTokens)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(claudeResponse{
			Content: []claudeContent{
				{Type: "tool_use", Name: "some_other_tool", Input: json.RawMessage(`{"wrong":true}`)},
				{Type: "tool_use", Name: "submit_x", Input: json.RawMessage(`{"ok":true}`)},
			},
		})
	}))
	defer server.Close()

	resp, err := newClaudeAt(server.URL, server.Client()).Complete(context.Background(), Request{User: "q", Schema: schema})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.JSON) != `{"ok":true}` {
		t.Errorf("json = %s, want the named tool's input", resp.JSON)
	}
}

// A model that answers in prose when asked for a tool call is a reply, not an
// error: it was billed, and the caller's validation decides what to do with it.
func TestClaudeClient_Complete_StructuredRequestWithoutToolUseHasNoJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(claudeResponse{
			Content: []claudeContent{{Type: "text", Text: "I think you should restart it."}},
			Usage:   claudeUsage{InputTokens: 5, OutputTokens: 9},
		})
	}))
	defer server.Close()

	resp, err := newClaudeAt(server.URL, server.Client()).Complete(context.Background(),
		Request{User: "q", Schema: &Schema{Name: "submit_x", Definition: map[string]any{}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.JSON != nil {
		t.Errorf("json = %s, want none", resp.JSON)
	}
	if resp.Text == "" || resp.Usage.OutputTokens != 9 {
		t.Errorf("the reply and its usage must still come back: %+v", resp)
	}
}

func TestClaudeClient_Complete_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer server.Close()

	resp, err := newClaudeAt(server.URL, server.Client()).Complete(context.Background(), Request{User: "test"})
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if resp != nil {
		t.Error("expected nil response on error")
	}
}

func TestClaudeClient_Complete_CancelledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := newClaudeAt(server.URL, server.Client()).Complete(ctx, Request{User: "test"})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if resp != nil {
		t.Error("expected nil response on cancellation")
	}
}

func TestClaudeClient_Complete_RequestTimeout(t *testing.T) {
	// The handler never reads the request body, so the server cannot notice the
	// client giving up; release it explicitly or server.Close would wait forever.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	start := time.Now()
	_, err := newClaudeAt(server.URL, server.Client()).Complete(context.Background(),
		Request{User: "test", Timeout: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the per-request timeout was not applied: took %s", time.Since(start))
	}
}

func TestClaudeClient_Complete_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(claudeResponse{Content: []claudeContent{}})
	}))
	defer server.Close()

	resp, err := newClaudeAt(server.URL, server.Client()).Complete(context.Background(), Request{User: "test"})
	if err == nil {
		t.Fatal("expected error for empty response")
	}
	if resp != nil {
		t.Error("expected nil response")
	}
}

func TestClaudeClient_Complete_MalformedEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer server.Close()

	if _, err := newClaudeAt(server.URL, server.Client()).Complete(context.Background(), Request{User: "test"}); err == nil {
		t.Fatal("expected error for an unparseable envelope")
	}
}

func TestClaudeClient_SetModelAndProvider(t *testing.T) {
	c := &ClaudeClient{model: defaultClaudeModel}
	c.SetModel("claude-x")
	if c.model != "claude-x" {
		t.Errorf("model = %q", c.model)
	}
	if c.Provider() != providerClaude {
		t.Errorf("provider = %q", c.Provider())
	}
}
