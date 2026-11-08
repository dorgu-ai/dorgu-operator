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

package diagnosis

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/llm/llmtest"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics/metricstest"
)

func cacheResult(t *testing.T, result string) float64 {
	t.Helper()
	return metricstest.Counter(t, "dorgu_llm_cache_total",
		map[string]string{"purpose": llm.PurposeDiagnosis, "result": result})
}

func diagnoseOnce(t *testing.T, p *AIProvider, ctx context.Context, signals []detection.Signal) []Diagnosis {
	t.Helper()
	out, err := p.Diagnose(ctx, signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return out
}

// KPI Q3 / H-3: the same signals, cycle after cycle, are one model call.
func TestAIProvider_UnchangedFingerprintAsksTheModelOnce(t *testing.T) {
	model := llmtest.Diagnosing("the limit is too low", "cause", llm.ActionResourceAdjustment, 0.9)
	p := NewAIProvider(model, allowAll{}, logr.Discard())
	hits, misses := cacheResult(t, "hit"), cacheResult(t, "miss")

	for range 10 {
		out := diagnoseOnce(t, p, context.Background(), []detection.Signal{oomSignalIn("apps")})
		if len(out) != 1 || out[0].Provider != providerNameAI || out[0].Summary != "the limit is too low" {
			t.Fatalf("every cycle must still return the AI diagnosis, got %+v", out)
		}
	}

	if model.Calls() != 1 {
		t.Errorf("model calls = %d, want 1 for 10 unchanged cycles", model.Calls())
	}
	if got := cacheResult(t, "hit") - hits; got != 9 {
		t.Errorf("cache hits +%v, want +9", got)
	}
	if got := cacheResult(t, "miss") - misses; got != 1 {
		t.Errorf("cache misses +%v, want +1", got)
	}
}

func TestAIProvider_ChangedSignalsAskAgain(t *testing.T) {
	model := llmtest.Diagnosing("s", "c", llm.ActionInvestigate, 0.5)
	p := NewAIProvider(model, allowAll{}, logr.Discard())

	diagnoseOnce(t, p, context.Background(), []detection.Signal{oomSignalIn("apps")})
	diagnoseOnce(t, p, context.Background(), []detection.Signal{oomSignalIn("apps")})
	if model.Calls() != 1 {
		t.Fatalf("calls = %d, want 1", model.Calls())
	}

	// A different application is a different question.
	diagnoseOnce(t, p, context.Background(), []detection.Signal{oomSignalIn("shop")})
	if model.Calls() != 2 {
		t.Errorf("calls = %d, want 2 after the signals changed", model.Calls())
	}
}

func TestAIProvider_CacheExpiresAfterTheTTL(t *testing.T) {
	model := llmtest.Diagnosing("s", "c", llm.ActionInvestigate, 0.5)
	p := NewAIProvider(model, allowAll{}, logr.Discard(), WithCacheTTL(30*time.Minute))
	now := time.Now()
	p.cache.SetClock(func() time.Time { return now })

	diagnoseOnce(t, p, context.Background(), []detection.Signal{oomSignalIn("apps")})
	now = now.Add(29 * time.Minute)
	diagnoseOnce(t, p, context.Background(), []detection.Signal{oomSignalIn("apps")})
	if model.Calls() != 1 {
		t.Fatalf("calls = %d, want 1 inside the TTL", model.Calls())
	}

	now = now.Add(2 * time.Minute)
	diagnoseOnce(t, p, context.Background(), []detection.Signal{oomSignalIn("apps")})
	if model.Calls() != 2 {
		t.Errorf("calls = %d, want 2 once the TTL ran out", model.Calls())
	}
}

func TestAIProvider_ZeroTTLDisablesTheCache(t *testing.T) {
	model := llmtest.Diagnosing("s", "c", llm.ActionInvestigate, 0.5)
	p := NewAIProvider(model, allowAll{}, logr.Discard(), WithCacheTTL(0))

	for range 3 {
		diagnoseOnce(t, p, context.Background(), []detection.Signal{oomSignalIn("apps")})
	}
	if model.Calls() != 3 {
		t.Errorf("calls = %d, want one per cycle with the cache off", model.Calls())
	}
}

// The default is the documented 30 minutes.
func TestAIProvider_DefaultCacheTTL(t *testing.T) {
	p := NewAIProvider(llmtest.Diagnosing("s", "c", llm.ActionInvestigate, 0.5), allowAll{}, logr.Discard())
	if p.cache.TTL() != llm.DefaultCacheTTL || llm.DefaultCacheTTL != 30*time.Minute {
		t.Errorf("ttl = %s, want 30m", p.cache.TTL())
	}
}

// OP-11, unit level: the model's action is advisory. Whatever it recommends, the
// diagnosis keeps the rules' SuggestedAction, confidence, category and severity,
// so the dedup key cannot be steered.
func TestAIProvider_ModelAdviceNeverChangesTheRulesDecisions(t *testing.T) {
	signals := []detection.Signal{oomSignalIn("apps")}
	rules, err := NewRuleBasedProvider(logr.Discard()).Diagnose(context.Background(), signals)
	if err != nil || len(rules) == 0 {
		t.Fatalf("rule-based diagnosis: %v", err)
	}

	for _, action := range llm.RecommendedActions() {
		t.Run(action, func(t *testing.T) {
			model := llmtest.Diagnosing("model narrative", "cause", action, 0.01)
			out := diagnoseOnce(t, NewAIProvider(model, allowAll{}, logr.Discard()), context.Background(), signals)
			if len(out) != 1 {
				t.Fatalf("diagnoses = %d", len(out))
			}
			d := out[0]
			if d.SuggestedAction != rules[0].SuggestedAction {
				t.Errorf("SuggestedAction = %q, want the rules' %q", d.SuggestedAction, rules[0].SuggestedAction)
			}
			if d.Confidence != rules[0].Confidence || d.Category != rules[0].Category || d.Severity != rules[0].Severity {
				t.Errorf("model advice leaked into the diagnosis: %+v", d)
			}
			if deduplicationKey(&d) != deduplicationKey(&rules[0]) {
				t.Errorf("the dedup key moved: %q vs %q", deduplicationKey(&d), deduplicationKey(&rules[0]))
			}
			if d.Summary != "model narrative" {
				t.Errorf("summary = %q, want the model's structured summary", d.Summary)
			}
		})
	}
}

// A reply that fails the schema falls back to the rule-based diagnosis, is
// counted, and is not asked about again every cycle.
func TestAIProvider_InvalidReplyFallsBackToTheRules(t *testing.T) {
	signals := []detection.Signal{oomSignalIn("apps")}
	rules, _ := NewRuleBasedProvider(logr.Discard()).Diagnose(context.Background(), signals)

	model := llmtest.New(func(llm.Request) (*llm.Response, error) {
		return llmtest.TextReply("Do not restart the pod; that only repeats the OOM kill."), nil
	})
	p := NewAIProvider(model, allowAll{}, logr.Discard())
	before := invalidCounter(t, model.Provider())

	for range 5 {
		out := diagnoseOnce(t, p, context.Background(), signals)
		if len(out) != 1 {
			t.Fatalf("diagnoses = %d, want the rule-based one", len(out))
		}
		d := out[0]
		if d.Provider != providerNameRuleBased {
			t.Errorf("provider = %q: an unusable reply must leave the diagnosis labelled rule-based", d.Provider)
		}
		if d.Summary != rules[0].Summary || d.SuggestedAction != rules[0].SuggestedAction {
			t.Errorf("diagnosis changed by an invalid reply: %+v", d)
		}
	}

	if model.Calls() != 1 {
		t.Errorf("calls = %d, want 1: a failed reply is remembered for a while", model.Calls())
	}
	if got := invalidCounter(t, model.Provider()) - before; got != 1 {
		t.Errorf("dorgu_llm_invalid_output_total +%v, want +1", got)
	}
}

func invalidCounter(t *testing.T, provider string) float64 {
	t.Helper()
	return metricstest.Counter(t, "dorgu_llm_invalid_output_total",
		map[string]string{"provider": provider, "purpose": llm.PurposeDiagnosis})
}

func TestAIProvider_InvalidReplyIsRetriedAfterTheFailureTTL(t *testing.T) {
	model := llmtest.New(func(llm.Request) (*llm.Response, error) { return llmtest.TextReply("nope"), nil })
	p := NewAIProvider(model, allowAll{}, logr.Discard())
	now := time.Now()
	p.cache.SetClock(func() time.Time { return now })
	signals := []detection.Signal{oomSignalIn("apps")}

	diagnoseOnce(t, p, context.Background(), signals)
	now = now.Add(llm.FailureTTL - time.Second)
	diagnoseOnce(t, p, context.Background(), signals)
	if model.Calls() != 1 {
		t.Fatalf("calls = %d, want 1", model.Calls())
	}
	now = now.Add(2 * time.Second)
	diagnoseOnce(t, p, context.Background(), signals)
	if model.Calls() != 2 {
		t.Errorf("calls = %d, want a retry once the failure TTL passed", model.Calls())
	}
}

func TestAIProvider_FailedRequestIsNotRetriedEveryCycle(t *testing.T) {
	model := llmtest.Failing(errors.New("API timeout"))
	p := NewAIProvider(model, allowAll{}, logr.Discard())

	for range 10 {
		out := diagnoseOnce(t, p, context.Background(), []detection.Signal{oomSignalIn("apps")})
		if len(out) != 0 {
			t.Fatalf("a failed request must leave the rule-based diagnosis alone, got %d", len(out))
		}
	}
	if model.Calls() != 1 {
		t.Errorf("calls = %d, want 1", model.Calls())
	}
}

// A failure in one diagnosis must not discard the others' enhancements.
func TestAIProvider_OneFailureDoesNotDropTheRest(t *testing.T) {
	calls := 0
	model := llmtest.New(func(req llm.Request) (*llm.Response, error) {
		calls++
		if strings.Contains(req.User, "ImagePullBackOff") {
			return nil, errors.New("boom")
		}
		return llmtest.DiagnosisReply("narrative", "cause", llm.ActionResourceAdjustment, 0.5), nil
	})
	pull := oomSignalIn("apps")
	pull.Type = detection.SignalImagePullBackOff
	pull.Category = detection.CategoryDeployment
	pull.Resource.Name = "web-xyz"

	out := diagnoseOnce(t, NewAIProvider(model, allowAll{}, logr.Discard()), context.Background(),
		[]detection.Signal{pull, oomSignalIn("apps")})

	if len(out) != 1 || out[0].Summary != "narrative" {
		t.Errorf("the OOM diagnosis lost its enhancement: %+v (calls=%d)", out, calls)
	}
}

func TestAIProvider_NoModelOptionKeepsTheGroupAwayFromTheModel(t *testing.T) {
	model := llmtest.Diagnosing("s", "c", llm.ActionInvestigate, 0.5)
	p := NewAIProvider(model, allowAll{}, logr.Discard())
	ctx := WithCallOptions(context.Background(), CallOptions{NoModel: true})

	out := diagnoseOnce(t, p, ctx, []detection.Signal{oomSignalIn("apps")})

	if len(out) != 0 || model.Calls() != 0 {
		t.Errorf("diagnoses=%d calls=%d: a withheld group must not reach the model", len(out), model.Calls())
	}
}

func TestAIProvider_PromptCarriesTheRealOccurrenceCount(t *testing.T) {
	model := llmtest.Diagnosing("s", "c", llm.ActionInvestigate, 0.5)
	p := NewAIProvider(model, allowAll{}, logr.Discard())
	ctx := WithCallOptions(context.Background(), CallOptions{
		PreviousOccurrences: func(context.Context, Diagnosis) int32 { return 51 },
	})

	diagnoseOnce(t, p, ctx, []detection.Signal{oomSignalIn("apps")})

	reqs := model.Requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].User, "seen 51 time(s) before this cycle") {
		t.Errorf("the prompt must state the real history: %+v", reqs)
	}
}

func TestAIProvider_PromptSaysFirstSightingWithoutALookup(t *testing.T) {
	model := llmtest.Diagnosing("s", "c", llm.ActionInvestigate, 0.5)
	diagnoseOnce(t, NewAIProvider(model, allowAll{}, logr.Discard()), context.Background(), []detection.Signal{oomSignalIn("apps")})

	if u := model.Requests()[0].User; !strings.Contains(u, "first sighting") || strings.Contains(u, "0 previous") {
		t.Errorf("prompt = %q", u)
	}
}

func TestAIProvider_OccurrenceCountDoesNotDefeatTheCache(t *testing.T) {
	model := llmtest.Diagnosing("s", "c", llm.ActionInvestigate, 0.5)
	p := NewAIProvider(model, allowAll{}, logr.Discard())
	n := int32(0)
	ctx := WithCallOptions(context.Background(), CallOptions{
		PreviousOccurrences: func(context.Context, Diagnosis) int32 { n++; return n },
	})

	for range 5 {
		diagnoseOnce(t, p, ctx, []detection.Signal{oomSignalIn("apps")})
	}
	if model.Calls() != 1 {
		t.Errorf("calls = %d: the occurrence count rises every cycle and must not be part of the fingerprint", model.Calls())
	}
}

func TestCallOptionsFrom_DefaultsToTheZeroValue(t *testing.T) {
	opts := CallOptionsFrom(context.Background())
	if opts.NoModel || opts.PreviousOccurrences != nil {
		t.Errorf("opts = %+v", opts)
	}
}

func TestFingerprint(t *testing.T) {
	rules := func(sigs ...detection.Signal) Diagnosis {
		out, err := NewRuleBasedProvider(logr.Discard()).Diagnose(context.Background(), sigs)
		if err != nil || len(out) == 0 {
			t.Fatalf("rule-based diagnosis: %v", err)
		}
		return out[0]
	}
	base := rules(oomSignalIn("apps"))

	t.Run("stable", func(t *testing.T) {
		if Fingerprint(base) != Fingerprint(rules(oomSignalIn("apps"))) {
			t.Error("the same signals must fingerprint the same")
		}
	})

	t.Run("ignores what moves while the situation does not", func(t *testing.T) {
		noisy := oomSignalIn("apps")
		noisy.Message = "Container killed due to OOM (restart count 99)"
		noisy.DetectedAt = time.Now().Add(time.Hour)
		v := 97.3
		noisy.Value = &v
		if Fingerprint(base) != Fingerprint(rules(noisy)) {
			t.Error("message, value and time must not change the fingerprint")
		}
	})

	// Review V1 audit: the planner fingerprint hashed a timestamp the gate stamps
	// every cycle. Nothing in the diagnosis fingerprint may move with the clock
	// either: every time on a Diagnosis and on its signals advances by a minute
	// per "cycle" here, and the key must not.
	t.Run("a moving clock does not change it", func(t *testing.T) {
		start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		seen := map[string]int{}
		for cycle := range 10 {
			at := start.Add(time.Duration(cycle) * time.Minute)
			sig := oomSignalIn("apps")
			sig.DetectedAt = at
			d := rules(sig)
			d.DiagnosedAt = at
			for i := range d.Contributing {
				d.Contributing[i].Signal.DetectedAt = at
			}
			seen[Fingerprint(d)]++
		}
		if len(seen) != 1 {
			t.Errorf("ten cycles at one-minute steps produced %d distinct fingerprints, want 1", len(seen))
		}
	})

	t.Run("a restarted pod is the same workload", func(t *testing.T) {
		restarted := oomSignalIn("apps")
		restarted.Resource.Name = "worker-zzz99"
		restarted.Metadata = map[string]string{detection.MetadataKeyDeployment: "worker"}
		original := oomSignalIn("apps")
		original.Metadata = map[string]string{detection.MetadataKeyDeployment: "worker"}
		if Fingerprint(rules(original)) != Fingerprint(rules(restarted)) {
			t.Error("a new pod name for the same Deployment must not look like a new incident")
		}
	})

	t.Run("a different application differs", func(t *testing.T) {
		if Fingerprint(base) == Fingerprint(rules(oomSignalIn("shop"))) {
			t.Error("different namespaces must not share a fingerprint")
		}
	})

	t.Run("a different signal differs", func(t *testing.T) {
		pull := oomSignalIn("apps")
		pull.Type = detection.SignalImagePullBackOff
		pull.Category = detection.CategoryDeployment
		if Fingerprint(base) == Fingerprint(rules(pull)) {
			t.Error("a different signal type must not share a fingerprint")
		}
	})

	t.Run("severity is part of it", func(t *testing.T) {
		warn := base
		warn.Severity = detection.SeverityWarning
		if Fingerprint(base) == Fingerprint(warn) {
			t.Error("an escalation is a new question")
		}
	})

	t.Run("signal order and duplicates do not matter", func(t *testing.T) {
		a, b := base, base
		extra := ContributingSignal{Signal: oomSignalIn("apps")}
		extra.Signal.Type = detection.SignalCrashLoopBackOff
		a.Contributing = append(append([]ContributingSignal{}, base.Contributing...), extra, extra)
		b.Contributing = append([]ContributingSignal{extra}, base.Contributing...)
		if Fingerprint(a) != Fingerprint(b) {
			t.Error("order and duplicates must not change the fingerprint")
		}
	})
}
