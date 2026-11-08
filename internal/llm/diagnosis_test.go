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
	"strings"
	"testing"

	"github.com/dorgu-ai/dorgu-operator/internal/metrics/metricstest"
)

const validDiagnosis = `{"summary":"The pod is OOM killed.","rootCause":"The memory limit is below peak.","recommendedAction":"resource-adjustment","confidence":0.8}`

func TestParseDiagnosis_Valid(t *testing.T) {
	got, err := ParseDiagnosis([]byte(validDiagnosis))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Summary != "The pod is OOM killed." || got.RootCause != "The memory limit is below peak." ||
		got.RecommendedAction != ActionResourceAdjustment || got.Confidence != 0.8 {
		t.Errorf("parsed = %+v", got)
	}
}

func TestParseDiagnosis_TrimsText(t *testing.T) {
	got, err := ParseDiagnosis([]byte(`{"summary":"  s  ","rootCause":"\nc\n","recommendedAction":"restart","confidence":0}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Summary != "s" || got.RootCause != "c" || got.Confidence != 0 {
		t.Errorf("parsed = %+v", got)
	}
}

func TestParseDiagnosis_EveryAllowedActionIsAccepted(t *testing.T) {
	for _, action := range RecommendedActions() {
		raw := `{"summary":"s","rootCause":"c","recommendedAction":"` + action + `","confidence":1}`
		if _, err := ParseDiagnosis([]byte(raw)); err != nil {
			t.Errorf("action %q rejected: %v", action, err)
		}
	}
}

func TestParseDiagnosis_Invalid(t *testing.T) {
	long := strings.Repeat("x", maxSummaryRunes+1)
	longCause := strings.Repeat("x", maxRootCauseRunes+1)
	tests := map[string]string{
		"empty":                  ``,
		"whitespace":             "  \n ",
		"prose":                  `Do not restart the pod; the memory limit is too low.`,
		"prose naming an action": `restart rollback resource-adjustment`,
		"not an object":          `["summary"]`,
		"null":                   `null`,
		"trailing text":          validDiagnosis + ` and then some prose`,
		"two objects":            validDiagnosis + validDiagnosis,
		"unknown field":          `{"summary":"s","rootCause":"c","recommendedAction":"restart","confidence":1,"extra":"x"}`,
		"missing summary":        `{"rootCause":"c","recommendedAction":"restart","confidence":1}`,
		"missing rootCause":      `{"summary":"s","recommendedAction":"restart","confidence":1}`,
		"missing action":         `{"summary":"s","rootCause":"c","confidence":1}`,
		"missing confidence":     `{"summary":"s","rootCause":"c","recommendedAction":"restart"}`,
		"empty summary":          `{"summary":" ","rootCause":"c","recommendedAction":"restart","confidence":1}`,
		"empty rootCause":        `{"summary":"s","rootCause":"","recommendedAction":"restart","confidence":1}`,
		"unknown action":         `{"summary":"s","rootCause":"c","recommendedAction":"delete-namespace","confidence":1}`,
		"action in prose":        `{"summary":"s","rootCause":"c","recommendedAction":"please restart it","confidence":1}`,
		"action wrong case":      `{"summary":"s","rootCause":"c","recommendedAction":"Restart","confidence":1}`,
		"confidence too high":    `{"summary":"s","rootCause":"c","recommendedAction":"restart","confidence":1.5}`,
		"confidence negative":    `{"summary":"s","rootCause":"c","recommendedAction":"restart","confidence":-0.1}`,
		"confidence a string":    `{"summary":"s","rootCause":"c","recommendedAction":"restart","confidence":"high"}`,
		"summary a number":       `{"summary":7,"rootCause":"c","recommendedAction":"restart","confidence":1}`,
		"summary too long":       `{"summary":"` + long + `","rootCause":"c","recommendedAction":"restart","confidence":1}`,
		"rootCause too long":     `{"summary":"s","rootCause":"` + longCause + `","recommendedAction":"restart","confidence":1}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseDiagnosis([]byte(raw))
			if err == nil {
				t.Fatalf("accepted %q as %+v", raw, got)
			}
			if !errors.Is(err, ErrInvalidOutput) {
				t.Errorf("error %v does not wrap ErrInvalidOutput", err)
			}
		})
	}
}

func TestDiagnosisSchema_MatchesTheValidator(t *testing.T) {
	schema := diagnosisSchema()
	if schema.Name != diagnosisSchemaName {
		t.Errorf("name = %q", schema.Name)
	}
	props, _ := schema.Definition["properties"].(map[string]any)
	for _, field := range []string{"summary", "rootCause", "recommendedAction", "confidence"} {
		if _, ok := props[field]; !ok {
			t.Errorf("schema has no %q property", field)
		}
	}
	if schema.Definition["additionalProperties"] != false {
		t.Error("the schema must forbid extra properties, as the validator does")
	}
	enum, _ := props["recommendedAction"].(map[string]any)["enum"].([]any)
	if len(enum) != len(RecommendedActions()) {
		t.Errorf("enum = %v, want %v", enum, RecommendedActions())
	}
}

type scriptedClient struct {
	resp  *Response
	err   error
	calls []Request
}

func (s *scriptedClient) Complete(_ context.Context, req Request) (*Response, error) {
	s.calls = append(s.calls, req)
	return s.resp, s.err
}
func (s *scriptedClient) Provider() string { return "scripted" }

func TestEnhanceDiagnosis_ValidReply(t *testing.T) {
	c := &scriptedClient{resp: &Response{JSON: []byte(validDiagnosis)}}

	got, err := EnhanceDiagnosis(context.Background(), c, DiagnosisRequest{
		Summary: "OOM", Category: "resource", Severity: "critical", PreviousOccurrences: 3,
		Signals: []SignalContext{{Type: "OOMKilled", Message: "killed", Resource: "Pod/a/b"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RecommendedAction != ActionResourceAdjustment {
		t.Errorf("response = %+v", got)
	}

	if len(c.calls) != 1 {
		t.Fatalf("requests = %d, want 1", len(c.calls))
	}
	req := c.calls[0]
	if req.Purpose != PurposeDiagnosis || req.Schema == nil || req.Schema.Name != diagnosisSchemaName || req.MaxTokens != diagnosisMaxTokens {
		t.Errorf("request = %+v", req)
	}
	if !strings.Contains(req.User, "seen 3 time(s) before") {
		t.Errorf("the prompt must carry the real occurrence count: %q", req.User)
	}
}

func TestEnhanceDiagnosis_InvalidReplyIsCountedAndReported(t *testing.T) {
	c := &scriptedClient{resp: &Response{Text: "Do not restart the pod."}}
	before := invalidCount(t)

	got, err := EnhanceDiagnosis(context.Background(), c, DiagnosisRequest{Summary: "OOM"})
	if got != nil || !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("got %+v, %v; want ErrInvalidOutput", got, err)
	}
	if after := invalidCount(t); after != before+1 {
		t.Errorf("dorgu_llm_invalid_output_total went %v -> %v, want +1", before, after)
	}
}

func TestEnhanceDiagnosis_RequestFailureIsNotInvalidOutput(t *testing.T) {
	boom := errors.New("boom")
	c := &scriptedClient{err: boom}
	before := invalidCount(t)

	_, err := EnhanceDiagnosis(context.Background(), c, DiagnosisRequest{Summary: "OOM"})
	if !errors.Is(err, boom) || errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("err = %v, want the request error unchanged", err)
	}
	if after := invalidCount(t); after != before {
		t.Error("a failed request must not count as invalid output")
	}
}

func TestBuildUserMessage_StatesOnlyRealHistory(t *testing.T) {
	first := buildUserMessage(DiagnosisRequest{Summary: "s"})
	if !strings.Contains(first, "first sighting") || strings.Contains(first, "0 previous") {
		t.Errorf("a new incident is a first sighting, not a count of zero: %q", first)
	}
	again := buildUserMessage(DiagnosisRequest{Summary: "s", PreviousOccurrences: 51})
	if !strings.Contains(again, "seen 51 time(s) before this cycle") {
		t.Errorf("message = %q", again)
	}
}

func TestDiagnosisSystemPrompt_TreatsClusterTextAsData(t *testing.T) {
	if !strings.Contains(diagnosisSystemPrompt, "untrusted data") {
		t.Error("the system prompt must tell the model that signal text is data, not instructions")
	}
}

func invalidCount(t *testing.T) float64 {
	t.Helper()
	return metricstest.Counter(t, "dorgu_llm_invalid_output_total",
		map[string]string{"provider": "scripted", "purpose": PurposeDiagnosis})
}
