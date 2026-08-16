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
)

func TestGeminiClient_EnhanceDiagnosis_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}

		// Verify request body structure.
		var reqBody geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(reqBody.Contents) != 1 || len(reqBody.Contents[0].Parts) != 1 {
			t.Errorf("contents = %+v, want single content with single part", reqBody.Contents)
		}

		resp := geminiResponse{
			Candidates: []geminiCandidate{
				{
					Content: geminiContent{
						Parts: []geminiPart{
							{Text: "The node is running out of memory due to resource-hungry pods. Consider scale-up by adding more nodes."},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := &GeminiClient{
		apiKey:  "test-key",
		model:   defaultGeminiModel,
		client:  server.Client(),
		baseURL: server.URL,
	}

	resp, err := client.EnhanceDiagnosis(context.Background(), DiagnosisRequest{
		Summary:         "Node under pressure: memory.",
		Category:        "node",
		Severity:        "warning",
		Confidence:      0.80,
		SuggestedAction: "resource-adjustment",
		Signals: []SignalContext{
			{Type: "NodeMemoryPressure", Message: "Node has memory pressure", Resource: "Node/worker-1"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.EnhancedSummary == "" {
		t.Error("enhanced summary is empty")
	}
	if resp.RecommendedAction != "scale-up" {
		t.Errorf("recommended action = %q, want %q", resp.RecommendedAction, "scale-up")
	}
}

func TestGeminiClient_EnhanceDiagnosis_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error": "forbidden"}`))
	}))
	defer server.Close()

	client := &GeminiClient{
		apiKey:  "test-key",
		model:   defaultGeminiModel,
		client:  server.Client(),
		baseURL: server.URL,
	}

	resp, err := client.EnhanceDiagnosis(context.Background(), DiagnosisRequest{Summary: "test"})
	if err == nil {
		t.Fatal("expected error for HTTP 403")
	}
	if resp != nil {
		t.Error("expected nil response on error")
	}
}

func TestGeminiClient_EnhanceDiagnosis_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := geminiResponse{Candidates: []geminiCandidate{}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := &GeminiClient{
		apiKey:  "test-key",
		model:   defaultGeminiModel,
		client:  server.Client(),
		baseURL: server.URL,
	}

	resp, err := client.EnhanceDiagnosis(context.Background(), DiagnosisRequest{Summary: "test"})
	if err == nil {
		t.Fatal("expected error for empty response")
	}
	if resp != nil {
		t.Error("expected nil response")
	}
}

func TestGeminiClient_EnhanceDiagnosis_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {}
	}))
	defer server.Close()

	client := &GeminiClient{
		apiKey:  "test-key",
		model:   defaultGeminiModel,
		client:  server.Client(),
		baseURL: server.URL,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := client.EnhanceDiagnosis(ctx, DiagnosisRequest{Summary: "test"})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if resp != nil {
		t.Error("expected nil response on timeout")
	}
}

func TestGeminiClient_Provider(t *testing.T) {
	client := &GeminiClient{}
	if client.Provider() != providerGemini {
		t.Errorf("provider = %q, want %q", client.Provider(), providerGemini)
	}
}
