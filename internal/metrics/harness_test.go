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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestHarnessCountersAreOnTheControllerRuntimeRegistry(t *testing.T) {
	// A CounterVec with no series yet is not gathered, so touch each first.
	RecordIncident("resource", "persona")
	RecordProposal(SourceDorgu, "resource-adjustment", ProposalProposed)
	RecordTransition("Approved", "Applying")

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	found := map[string]bool{}
	for _, f := range families {
		found[f.GetName()] = true
	}
	for _, name := range []string{
		"dorgu_incidents_total", "dorgu_proposals_total", "dorgu_remediation_transitions_total",
	} {
		if !found[name] {
			t.Errorf("%s is not registered on controller-runtime's registry", name)
		}
	}
}

func TestRecordIncidentIncrements(t *testing.T) {
	before := testutil.ToFloat64(incidentsTotal.WithLabelValues("node", "unattributed"))
	RecordIncident("node", "unattributed")
	RecordIncident("node", "unattributed")
	if got := testutil.ToFloat64(incidentsTotal.WithLabelValues("node", "unattributed")) - before; got != 2 {
		t.Errorf("incidents delta = %v, want 2", got)
	}
}

func TestRecordProposalIncrements(t *testing.T) {
	series := proposalsTotal.WithLabelValues(SourceDorgu, "restart", ProposalSkipped)
	before := testutil.ToFloat64(series)
	RecordProposal(SourceDorgu, "restart", ProposalSkipped)
	if got := testutil.ToFloat64(series) - before; got != 1 {
		t.Errorf("proposals delta = %v, want 1", got)
	}
}

func TestRecordTransitionIncrements(t *testing.T) {
	series := transitionsTotal.WithLabelValues("Verifying", "RolledBack")
	before := testutil.ToFloat64(series)
	RecordTransition("Verifying", "RolledBack")
	if got := testutil.ToFloat64(series) - before; got != 1 {
		t.Errorf("transitions delta = %v, want 1", got)
	}
}

// An RA the controller first sees with no phase is labelled "none", so the
// series is never an empty label value.
func TestRecordTransitionFromEmptyPhase(t *testing.T) {
	series := transitionsTotal.WithLabelValues(PhaseNone, "Pending")
	before := testutil.ToFloat64(series)
	RecordTransition("", "Pending")
	if got := testutil.ToFloat64(series) - before; got != 1 {
		t.Errorf("transitions delta = %v, want 1", got)
	}
}

// A no-op is not a transition.
func TestRecordTransitionIgnoresSamePhase(t *testing.T) {
	series := transitionsTotal.WithLabelValues("Pending", "Pending")
	before := testutil.ToFloat64(series)
	RecordTransition("Pending", "Pending")
	if got := testutil.ToFloat64(series) - before; got != 0 {
		t.Errorf("transitions delta = %v, want 0", got)
	}
}

func TestRecordLLMRequestAndTokens(t *testing.T) {
	req := llmRequestsTotal.WithLabelValues("claude", LLMPurposeDiagnosis, LLMOutcomeSuccess)
	in := llmTokensTotal.WithLabelValues("claude", LLMPurposeDiagnosis, LLMTokensInput)
	out := llmTokensTotal.WithLabelValues("claude", LLMPurposeDiagnosis, LLMTokensOutput)
	reqBefore, inBefore, outBefore := testutil.ToFloat64(req), testutil.ToFloat64(in), testutil.ToFloat64(out)

	RecordLLMRequest("claude", LLMPurposeDiagnosis, LLMOutcomeSuccess)
	RecordLLMTokens("claude", LLMPurposeDiagnosis, LLMTokensInput, 120)
	RecordLLMTokens("claude", LLMPurposeDiagnosis, LLMTokensOutput, 30)

	if got := testutil.ToFloat64(req) - reqBefore; got != 1 {
		t.Errorf("requests delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(in) - inBefore; got != 120 {
		t.Errorf("input tokens delta = %v, want 120", got)
	}
	if got := testutil.ToFloat64(out) - outBefore; got != 30 {
		t.Errorf("output tokens delta = %v, want 30", got)
	}
}

// A provider that reports no usage must not create a series as if it had.
func TestRecordLLMTokensIgnoresNonPositiveCounts(t *testing.T) {
	series := llmTokensTotal.WithLabelValues("gemini", LLMPurposePlanning, LLMTokensInput)
	before := testutil.ToFloat64(series)
	RecordLLMTokens("gemini", LLMPurposePlanning, LLMTokensInput, 0)
	RecordLLMTokens("gemini", LLMPurposePlanning, LLMTokensInput, -5)
	if got := testutil.ToFloat64(series) - before; got != 0 {
		t.Errorf("tokens delta = %v, want 0", got)
	}
}

func TestRecordLLMInvalidOutputAndCache(t *testing.T) {
	invalid := llmInvalidOutputTotal.WithLabelValues("claude", LLMPurposeDiagnosis)
	hit := llmCacheTotal.WithLabelValues(LLMPurposePlanning, LLMCacheHit)
	miss := llmCacheTotal.WithLabelValues(LLMPurposePlanning, LLMCacheMiss)
	invalidBefore, hitBefore, missBefore := testutil.ToFloat64(invalid), testutil.ToFloat64(hit), testutil.ToFloat64(miss)

	RecordLLMInvalidOutput("claude", LLMPurposeDiagnosis)
	RecordLLMCache(LLMPurposePlanning, LLMCacheHit)
	RecordLLMCache(LLMPurposePlanning, LLMCacheMiss)
	RecordLLMCache(LLMPurposePlanning, LLMCacheMiss)

	if got := testutil.ToFloat64(invalid) - invalidBefore; got != 1 {
		t.Errorf("invalid output delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(hit) - hitBefore; got != 1 {
		t.Errorf("cache hit delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(miss) - missBefore; got != 2 {
		t.Errorf("cache miss delta = %v, want 2", got)
	}
}

func TestLLMCountersAreRegistered(t *testing.T) {
	RecordLLMRequest("claude", LLMPurposePlanning, LLMOutcomeError)
	RecordLLMTokens("claude", LLMPurposePlanning, LLMTokensInput, 1)
	RecordLLMInvalidOutput("claude", LLMPurposePlanning)
	RecordLLMCache(LLMPurposeDiagnosis, LLMCacheHit)

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	found := map[string]bool{}
	for _, f := range families {
		found[f.GetName()] = true
	}
	for _, name := range []string{
		"dorgu_llm_requests_total", "dorgu_llm_tokens_total",
		"dorgu_llm_invalid_output_total", "dorgu_llm_cache_total",
	} {
		if !found[name] {
			t.Errorf("%s is not registered on controller-runtime's registry", name)
		}
	}
}

func TestRecordIncidentRecurrenceIncrements(t *testing.T) {
	series := incidentRecurrencesTotal.WithLabelValues("resource", "persona")
	before := testutil.ToFloat64(series)
	RecordIncidentRecurrence("resource", "persona")
	if got := testutil.ToFloat64(series) - before; got != 1 {
		t.Errorf("recurrences delta = %v, want 1", got)
	}
}

func TestRecordIncidentRecurrenceCappedAndPruneIncrement(t *testing.T) {
	capped := incidentRecurrencesCappedTotal.WithLabelValues("health", "persona")
	before := testutil.ToFloat64(capped)
	RecordIncidentRecurrenceCapped("health", "persona")
	if got := testutil.ToFloat64(capped) - before; got != 1 {
		t.Errorf("capped delta = %v, want 1", got)
	}
	for _, r := range []string{PruneDeleted, PruneKeptReferenced, PruneError} {
		series := incidentPruneTotal.WithLabelValues(r)
		before := testutil.ToFloat64(series)
		RecordIncidentPrune(r)
		if got := testutil.ToFloat64(series) - before; got != 1 {
			t.Errorf("prune %s delta = %v, want 1", r, got)
		}
	}
}
