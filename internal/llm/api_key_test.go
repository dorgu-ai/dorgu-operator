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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// API keys must never reach an error message or a log line. The Gemini key
// used to travel in the query string (?key=...). A URL ends up
// in proxy logs, in *url.Error messages and therefore in the operator's own
// error log, so the key goes in the x-goog-api-key header and never in a URL.

const secretGeminiKey = "AIzaSy-secret-gemini-key"

type captureTransport struct {
	req *http.Request
	err error
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.req = req
	if c.err != nil {
		return nil, c.err
	}
	body := `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestGeminiClient_KeyInHeaderNotURL(t *testing.T) {
	capture := &captureTransport{}
	g := &GeminiClient{apiKey: secretGeminiKey, model: defaultGeminiModel, client: &http.Client{Transport: capture}}

	if _, err := g.EnhanceDiagnosis(context.Background(), DiagnosisRequest{Summary: "x"}); err != nil {
		t.Fatalf("EnhanceDiagnosis: %v", err)
	}

	if strings.Contains(capture.req.URL.String(), secretGeminiKey) {
		t.Errorf("API key leaked into the request URL: %s", capture.req.URL.Redacted())
	}
	if capture.req.URL.Query().Has("key") {
		t.Errorf("request URL still carries a key query parameter")
	}
	if got := capture.req.Header.Get("x-goog-api-key"); got != secretGeminiKey {
		t.Errorf("x-goog-api-key header = %q, want the API key", got)
	}
	wantPath := "/v1beta/models/" + defaultGeminiModel + ":generateContent"
	if capture.req.URL.Path != wantPath {
		t.Errorf("path = %q, want %q", capture.req.URL.Path, wantPath)
	}
}

func TestGeminiClient_TransportErrorNeverCarriesKey(t *testing.T) {
	capture := &captureTransport{err: errors.New("dial tcp: connection refused")}
	g := &GeminiClient{apiKey: secretGeminiKey, model: defaultGeminiModel, client: &http.Client{Transport: capture}}

	_, err := g.EnhanceDiagnosis(context.Background(), DiagnosisRequest{Summary: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secretGeminiKey) {
		t.Errorf("error message leaks the API key: %v", err)
	}
}

func TestGeminiClient_ErrorBodyEchoingKeyIsRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"API key ` + r.Header.Get("x-goog-api-key") + ` not valid"}}`))
	}))
	defer server.Close()

	g := &GeminiClient{apiKey: secretGeminiKey, model: defaultGeminiModel, client: server.Client(), baseURL: server.URL}
	_, err := g.EnhanceDiagnosis(context.Background(), DiagnosisRequest{Summary: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secretGeminiKey) {
		t.Errorf("error message leaks the API key: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should still report the status: %v", err)
	}
}

func TestClaudeClient_ErrorBodyEchoingKeyIsRedacted(t *testing.T) {
	const key = "sk-ant-secret-claude-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid x-api-key ` + r.Header.Get("x-api-key") + `"}}`))
	}))
	defer server.Close()

	c := &ClaudeClient{apiKey: key, model: defaultClaudeModel, client: server.Client(), baseURL: server.URL}
	_, err := c.EnhanceDiagnosis(context.Background(), DiagnosisRequest{Summary: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error message leaks the API key: %v", err)
	}
}
