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
	"strings"
	"testing"
)

func newGeminiAt(url string, hc *http.Client) *GeminiClient {
	return &GeminiClient{apiKey: "test-key", model: defaultGeminiModel, client: hc, baseURL: url}
}

func geminiReply(text string, usage geminiUsage) geminiResponse {
	return geminiResponse{
		Candidates:    []geminiCandidate{{Content: geminiContent{Parts: []geminiPart{{Text: text}}}}},
		UsageMetadata: usage,
	}
}

func TestGeminiClient_Complete_TextRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var body geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(body.Contents) != 1 || body.Contents[0].Parts[0].Text != "hello" {
			t.Errorf("contents = %+v", body.Contents)
		}
		if body.SystemInstruction == nil || body.SystemInstruction.Parts[0].Text != "be brief" {
			t.Errorf("systemInstruction = %+v", body.SystemInstruction)
		}
		if body.GenerationConfig.ResponseMimeType != "" {
			t.Errorf("an unstructured request must not ask for JSON: %+v", body.GenerationConfig)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiReply(" hi ", geminiUsage{PromptTokenCount: 3, CandidatesTokenCount: 4}))
	}))
	defer server.Close()

	resp, err := newGeminiAt(server.URL, server.Client()).Complete(context.Background(),
		Request{System: "be brief", User: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "hi" || resp.JSON != nil {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Usage != (Usage{InputTokens: 3, OutputTokens: 4}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestGeminiClient_Complete_StructuredRequestAsksForJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.GenerationConfig.ResponseMimeType != "application/json" {
			t.Errorf("responseMimeType = %q", body.GenerationConfig.ResponseMimeType)
		}
		if body.SystemInstruction == nil || !strings.Contains(body.SystemInstruction.Parts[0].Text, `"object"`) {
			t.Errorf("the schema must be stated in the system instruction: %+v", body.SystemInstruction)
		}
		_ = json.NewEncoder(w).Encode(geminiReply(`{"ok":true}`, geminiUsage{}))
	}))
	defer server.Close()

	resp, err := newGeminiAt(server.URL, server.Client()).Complete(context.Background(),
		Request{User: "q", Schema: &Schema{Name: "x", Definition: map[string]any{"type": "object"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.JSON) != `{"ok":true}` {
		t.Errorf("json = %s", resp.JSON)
	}
}

func TestGeminiClient_Complete_StructuredReplyThatIsNotJSONHasNoJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(geminiReply("Sure! Restart it.", geminiUsage{CandidatesTokenCount: 6}))
	}))
	defer server.Close()

	resp, err := newGeminiAt(server.URL, server.Client()).Complete(context.Background(),
		Request{User: "q", Schema: &Schema{Name: "x", Definition: map[string]any{}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.JSON != nil || resp.Text == "" || resp.Usage.OutputTokens != 6 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestGeminiClient_Complete_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer server.Close()

	resp, err := newGeminiAt(server.URL, server.Client()).Complete(context.Background(), Request{User: "test"})
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if resp != nil {
		t.Error("expected nil response on error")
	}
}

func TestGeminiClient_Complete_EmptyResponse(t *testing.T) {
	for name, body := range map[string]geminiResponse{
		"no candidates": {},
		"no parts":      {Candidates: []geminiCandidate{{}}},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()

			resp, err := newGeminiAt(server.URL, server.Client()).Complete(context.Background(), Request{User: "test"})
			if err == nil {
				t.Fatal("expected error for empty response")
			}
			if resp != nil {
				t.Error("expected nil response")
			}
		})
	}
}

func TestGeminiClient_Complete_MalformedEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer server.Close()

	if _, err := newGeminiAt(server.URL, server.Client()).Complete(context.Background(), Request{User: "test"}); err == nil {
		t.Fatal("expected error for an unparseable envelope")
	}
}

func TestGeminiClient_Complete_CancelledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := newGeminiAt(server.URL, server.Client()).Complete(ctx, Request{User: "test"})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if resp != nil {
		t.Error("expected nil response on cancellation")
	}
}

func TestGeminiClient_ProviderAndModel(t *testing.T) {
	client := &GeminiClient{}
	if client.Provider() != providerGemini {
		t.Errorf("provider = %q, want %q", client.Provider(), providerGemini)
	}
	client.SetModel("gemini-x")
	if client.model != "gemini-x" {
		t.Errorf("model = %q", client.model)
	}
}
