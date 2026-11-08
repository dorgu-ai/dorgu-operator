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
	"testing"
	"time"

	"github.com/dorgu-ai/dorgu-operator/internal/metrics/metricstest"
)

type recordingClient struct {
	resp *Response
	err  error
}

func (r recordingClient) Complete(context.Context, Request) (*Response, error) { return r.resp, r.err }
func (r recordingClient) Provider() string                                     { return "instr" }

func requests(t *testing.T, purpose, outcome string) float64 {
	t.Helper()
	return metricstest.Counter(t, "dorgu_llm_requests_total",
		map[string]string{"provider": "instr", "purpose": purpose, "outcome": outcome})
}

func tokens(t *testing.T, purpose, kind string) float64 {
	t.Helper()
	return metricstest.Counter(t, "dorgu_llm_tokens_total",
		map[string]string{"provider": "instr", "purpose": purpose, "kind": kind})
}

func TestInstrument_CountsSuccessAndTokens(t *testing.T) {
	c := Instrument(recordingClient{resp: &Response{Usage: Usage{InputTokens: 120, OutputTokens: 30}}})
	okBefore, inBefore, outBefore := requests(t, PurposePlanning, "success"), tokens(t, PurposePlanning, "input"), tokens(t, PurposePlanning, "output")

	for range 2 {
		if _, err := c.Complete(context.Background(), Request{Purpose: PurposePlanning}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if got := requests(t, PurposePlanning, "success") - okBefore; got != 2 {
		t.Errorf("success requests +%v, want +2", got)
	}
	if got := tokens(t, PurposePlanning, "input") - inBefore; got != 240 {
		t.Errorf("input tokens +%v, want +240", got)
	}
	if got := tokens(t, PurposePlanning, "output") - outBefore; got != 60 {
		t.Errorf("output tokens +%v, want +60", got)
	}
	if c.Provider() != "instr" {
		t.Errorf("provider = %q", c.Provider())
	}
}

func TestInstrument_CountsErrorsWithoutTokens(t *testing.T) {
	boom := errors.New("boom")
	c := Instrument(recordingClient{err: boom})
	errBefore, inBefore := requests(t, PurposeDiagnosis, "error"), tokens(t, PurposeDiagnosis, "input")

	if _, err := c.Complete(context.Background(), Request{Purpose: PurposeDiagnosis}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the request error", err)
	}

	if got := requests(t, PurposeDiagnosis, "error") - errBefore; got != 1 {
		t.Errorf("error requests +%v, want +1", got)
	}
	if got := tokens(t, PurposeDiagnosis, "input") - inBefore; got != 0 {
		t.Errorf("a failed request must not add tokens, got +%v", got)
	}
}

func TestInstrument_ReportsNoTokensWhenTheProviderGivesNone(t *testing.T) {
	c := Instrument(recordingClient{resp: &Response{}})
	before := tokens(t, "no-usage", "input")
	if _, err := c.Complete(context.Background(), Request{Purpose: "no-usage"}); err != nil {
		t.Fatal(err)
	}
	if got := tokens(t, "no-usage", "input") - before; got != 0 {
		t.Errorf("tokens +%v, want 0", got)
	}
}

func TestInstrument_NeverWrapsTwice(t *testing.T) {
	once := Instrument(recordingClient{resp: &Response{}})
	if twice := Instrument(once); twice != once {
		t.Error("wrapping an instrumented client must return it unchanged, or every request is counted twice")
	}
	before := requests(t, "twice", "success")
	if _, err := Instrument(once).Complete(context.Background(), Request{Purpose: "twice"}); err != nil {
		t.Fatal(err)
	}
	if got := requests(t, "twice", "success") - before; got != 1 {
		t.Errorf("one request counted %v times", got)
	}
}

func TestNewClient_ReturnsAnInstrumentedClientWithTheModel(t *testing.T) {
	c, err := NewClient(providerClaude, "key", WithModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}
	inst, ok := c.(*instrumented)
	if !ok {
		t.Fatalf("NewClient returned %T, want the instrumented wrapper", c)
	}
	if got := inst.Unwrap().(*ClaudeClient).model; got != "claude-x" {
		t.Errorf("model = %q, want claude-x", got)
	}

	g, err := NewClient(providerGemini, "key", WithModel("gemini-x"))
	if err != nil {
		t.Fatal(err)
	}
	if got := g.(*instrumented).Unwrap().(*GeminiClient).model; got != "gemini-x" {
		t.Errorf("model = %q, want gemini-x", got)
	}

	def, err := NewClient(providerClaude, "key", WithModel(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.(*instrumented).Unwrap().(*ClaudeClient).model; got != defaultClaudeModel {
		t.Errorf("an empty model must keep the default, got %q", got)
	}
}

func TestCache_HitMissAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	c := NewCache[string](time.Minute, 0)
	c.SetClock(func() time.Time { return now })

	if _, ok := c.Get("k"); ok {
		t.Fatal("hit on an empty cache")
	}
	c.Set("k", "v")
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Fatalf("Get = %q, %v", v, ok)
	}

	now = now.Add(59 * time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Error("expired too early")
	}
	now = now.Add(2 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Error("an expired entry must miss")
	}
	if c.Len() != 0 {
		t.Errorf("the expired entry was not dropped, len = %d", c.Len())
	}
}

func TestCache_FailureEntriesNeverOutliveSuccess(t *testing.T) {
	now := time.Now()
	c := NewCache[string](time.Minute, 0)
	c.SetClock(func() time.Time { return now })

	c.SetFor("long", "x", time.Hour)
	c.SetFor("zero", "x", 0)
	c.SetFor("short", "x", 10*time.Second)

	now = now.Add(30 * time.Second)
	if _, ok := c.Get("short"); ok {
		t.Error("the short entry should have expired")
	}
	if _, ok := c.Get("long"); !ok {
		t.Error("the capped entry should live as long as the cache ttl")
	}
	now = now.Add(31 * time.Second)
	for _, k := range []string{"long", "zero"} {
		if _, ok := c.Get(k); ok {
			t.Errorf("%s outlived the cache ttl", k)
		}
	}
}

func TestCache_DisabledStoresNothing(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		c := NewCache[string](ttl, 0)
		c.Set("k", "v")
		if _, ok := c.Get("k"); ok {
			t.Errorf("ttl %s: a disabled cache hit", ttl)
		}
		if c.Enabled() || c.Len() != 0 {
			t.Errorf("ttl %s: enabled=%v len=%d", ttl, c.Enabled(), c.Len())
		}
	}
	if NewCache[int](time.Second, 0).TTL() != time.Second {
		t.Error("TTL() must report the configured lifetime")
	}
}

func TestCache_IsBounded(t *testing.T) {
	now := time.Now()
	c := NewCache[int](time.Hour, 3)
	c.SetClock(func() time.Time { return now })

	for i, k := range []string{"a", "b", "c"} {
		now = now.Add(time.Second)
		c.Set(k, i)
	}
	now = now.Add(time.Second)
	c.Set("d", 3)

	if c.Len() != 3 {
		t.Fatalf("len = %d, want the cap of 3", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Error("the entry closest to expiry should have been evicted")
	}
	if _, ok := c.Get("d"); !ok {
		t.Error("the new entry must be stored")
	}

	// Overwriting an existing key never evicts.
	c.Set("d", 4)
	if c.Len() != 3 {
		t.Errorf("len = %d after an overwrite", c.Len())
	}
}

func TestCache_EvictionPrefersExpiredEntries(t *testing.T) {
	now := time.Now()
	c := NewCache[int](time.Minute, 2)
	c.SetClock(func() time.Time { return now })

	c.Set("old", 1)
	now = now.Add(2 * time.Minute)
	c.Set("fresh", 2)
	c.Set("new", 3)

	if _, ok := c.Get("fresh"); !ok {
		t.Error("a live entry was evicted while an expired one was available")
	}
	if _, ok := c.Get("new"); !ok {
		t.Error("the new entry is missing")
	}
}

// B6 1.12: the request duration histogram, which KPIS.md lists beside the request
// and token counters, is observed for failed requests too: a slow error is still
// time the incident spent waiting.
func TestInstrument_ObservesTheRequestDurationOfSuccessAndError(t *testing.T) {
	labels := map[string]string{"provider": "instr", "purpose": PurposeDiagnosis}
	before := metricstest.HistogramCount(t, "dorgu_llm_request_duration_seconds", labels)

	ok := Instrument(recordingClient{resp: &Response{}})
	if _, err := ok.Complete(context.Background(), Request{Purpose: PurposeDiagnosis}); err != nil {
		t.Fatal(err)
	}
	failing := Instrument(recordingClient{err: errors.New("provider down")})
	if _, err := failing.Complete(context.Background(), Request{Purpose: PurposeDiagnosis}); err == nil {
		t.Fatal("expected the provider error")
	}

	if got := metricstest.HistogramCount(t, "dorgu_llm_request_duration_seconds", labels) - before; got != 2 {
		t.Errorf("duration samples +%d, want +2 (one success, one error)", got)
	}
}
