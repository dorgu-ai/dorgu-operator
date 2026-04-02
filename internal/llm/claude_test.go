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

func TestClaudeClient_EnhanceDiagnosis_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request format.
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q, want %q", r.Header.Get("x-api-key"), "test-key")
		}
		if r.Header.Get("anthropic-version") != anthropicVersion {
			t.Errorf("anthropic-version = %q, want %q", r.Header.Get("anthropic-version"), anthropicVersion)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}

		// Verify request body structure.
		var reqBody claudeRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if reqBody.Model != defaultClaudeModel {
			t.Errorf("model = %q, want %q", reqBody.Model, defaultClaudeModel)
		}
		if reqBody.System == "" {
			t.Error("system prompt is empty")
		}
		if len(reqBody.Messages) != 1 || reqBody.Messages[0].Role != "user" {
			t.Errorf("messages = %+v, want single user message", reqBody.Messages)
		}

		resp := claudeResponse{
			Content: []claudeContent{
				{Type: "text", Text: "The OOM kill indicates the container's memory limit of 256Mi is too low for the workload's peak usage. Increase to 512Mi and add a resource-adjustment."},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := &ClaudeClient{
		apiKey:  "test-key",
		model:   defaultClaudeModel,
		client:  server.Client(),
		baseURL: server.URL,
	}

	resp, err := client.EnhanceDiagnosis(context.Background(), DiagnosisRequest{
		Summary:         "Container memory limit insufficient for workload.",
		Category:        "resource",
		Severity:        "critical",
		Confidence:      0.85,
		SuggestedAction: "resource-adjustment",
		Signals: []SignalContext{
			{Type: "OOMKilled", Message: "Container killed due to OOM", Resource: "Pod/api-server-xyz"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.EnhancedSummary == "" {
		t.Error("enhanced summary is empty")
	}
	if resp.RecommendedAction != "resource-adjustment" {
		t.Errorf("recommended action = %q, want %q", resp.RecommendedAction, "resource-adjustment")
	}
}

func TestClaudeClient_EnhanceDiagnosis_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer server.Close()

	client := &ClaudeClient{
		apiKey:  "test-key",
		model:   defaultClaudeModel,
		client:  server.Client(),
		baseURL: server.URL,
	}

	resp, err := client.EnhanceDiagnosis(context.Background(), DiagnosisRequest{
		Summary: "test",
	})
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if resp != nil {
		t.Error("expected nil response on error")
	}
}

func TestClaudeClient_EnhanceDiagnosis_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Never respond — context cancel will trigger.
		select {}
	}))
	defer server.Close()

	client := &ClaudeClient{
		apiKey:  "test-key",
		model:   defaultClaudeModel,
		client:  server.Client(),
		baseURL: server.URL,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	resp, err := client.EnhanceDiagnosis(ctx, DiagnosisRequest{Summary: "test"})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if resp != nil {
		t.Error("expected nil response on timeout")
	}
}

func TestClaudeClient_EnhanceDiagnosis_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := claudeResponse{Content: []claudeContent{}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := &ClaudeClient{
		apiKey:  "test-key",
		model:   defaultClaudeModel,
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

func TestClaudeClient_Provider(t *testing.T) {
	client := &ClaudeClient{}
	if client.Provider() != "claude" {
		t.Errorf("provider = %q, want %q", client.Provider(), "claude")
	}
}
