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

// Package metrics is Dorgu's Prometheus integration in both directions: a query
// client the ApplicationPersona reconciler uses for baseline learning
// (prometheus_client.go), and the harness counters the operator exposes on its
// own metrics endpoint (harness.go).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Harness counters: what Dorgu detected, proposed and did, exposed on the
// operator's existing metrics endpoint through controller-runtime's registry.
// They are the measurable half of "every refusal is data"; label values are
// drawn from small fixed sets, never from object names.

// SourceDorgu labels proposals made by Dorgu's own proposer. External
// producers get their own source value once intake exists.
const SourceDorgu = "dorgu"

// Proposal results.
const (
	// ProposalProposed means a RemediationAction was written.
	ProposalProposed = "proposed"
	// ProposalSkipped means the proposer declined: a guardrail, dedup, the
	// self-healing mode, or nothing to change.
	ProposalSkipped = "skipped"
	// ProposalSuppressed means a recent human rejection held the proposal back.
	ProposalSuppressed = "suppressed"
	// ProposalDeferred means a proposal for the same incident or fix is already
	// outstanding, so the proposer made no second one. It is not a refusal.
	ProposalDeferred = "deferred"
	// ProposalError means proposing failed.
	ProposalError = "error"
)

// PhaseNone labels the "from" side of a transition out of an RA with no phase.
const PhaseNone = "none"

var (
	incidentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_incidents_total",
		Help: "IncidentMemory records opened, by diagnosis category (class) and attribution (scope).",
	}, []string{"class", "scope"})

	incidentRecurrencesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_incident_recurrences_total",
		Help: "Incidents opened because a fault came back after an earlier incident of it resolved, by diagnosis " +
			"category (class) and attribution (scope). Each is also counted in dorgu_incidents_total.",
	}, []string{"class", "scope"})

	proposalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_proposals_total",
		Help: "Remediation proposal attempts, by producer (source), suggested action and result " +
			"(proposed, skipped, suppressed, deferred, error).",
	}, []string{"source", "action_type", "result"})

	transitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_remediation_transitions_total",
		Help: "RemediationAction phase transitions written by the operator.",
	}, []string{"from", "to"})
)

// LLM request outcomes, the values of the outcome label on
// dorgu_llm_requests_total.
const (
	// LLMOutcomeSuccess means the provider answered with HTTP 200 and a body.
	LLMOutcomeSuccess = "success"
	// LLMOutcomeError means the request failed: transport error, non-200 status,
	// unreadable body or timeout.
	LLMOutcomeError = "error"
)

// LLM purposes, the values of the purpose label on the LLM metrics.
const (
	// LLMPurposeDiagnosis is the AI explanation of a rule-based diagnosis.
	LLMPurposeDiagnosis = "diagnosis"
	// LLMPurposePlanning is the AI remediation planner.
	LLMPurposePlanning = "planning"
)

// LLM token kinds, the values of the kind label on dorgu_llm_tokens_total.
const (
	LLMTokensInput  = "input"
	LLMTokensOutput = "output"
)

// LLM cache results, the values of the result label on dorgu_llm_cache_total.
const (
	// LLMCacheHit means a model call was avoided because the signal fingerprint
	// had not changed since the last answer.
	LLMCacheHit = "hit"
	// LLMCacheMiss means the fingerprint was new or expired and a call was made.
	LLMCacheMiss = "miss"
)

var (
	llmRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_llm_requests_total",
		Help: "Requests sent to the LLM provider, by provider, purpose (diagnosis, planning) and outcome " +
			"(success, error). KPI Q3: model calls per incident.",
	}, []string{"provider", "purpose", "outcome"})

	llmTokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_llm_tokens_total",
		Help: "Tokens the LLM provider reported using, by provider, purpose and kind (input, output).",
	}, []string{"provider", "purpose", "kind"})

	llmInvalidOutputTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_llm_invalid_output_total",
		Help: "Model replies that failed schema validation and were replaced by the rule-based result, " +
			"by provider and purpose.",
	}, []string{"provider", "purpose"})

	llmCacheTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_llm_cache_total",
		Help: "Model calls avoided (hit) or made (miss) by the signal-fingerprint cache, by purpose.",
	}, []string{"purpose", "result"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(incidentsTotal, incidentRecurrencesTotal, proposalsTotal, transitionsTotal,
		llmRequestsTotal, llmTokensTotal, llmInvalidOutputTotal, llmCacheTotal)
}

// RecordLLMRequest counts one request to the LLM provider.
func RecordLLMRequest(provider, purpose, outcome string) {
	llmRequestsTotal.WithLabelValues(provider, purpose, outcome).Inc()
}

// RecordLLMTokens adds the tokens one request used. Non-positive counts are
// ignored: a provider that reports no usage must not register a zero series
// as if it had.
func RecordLLMTokens(provider, purpose, kind string, n int) {
	if n <= 0 {
		return
	}
	llmTokensTotal.WithLabelValues(provider, purpose, kind).Add(float64(n))
}

// RecordLLMInvalidOutput counts a model reply that failed validation.
func RecordLLMInvalidOutput(provider, purpose string) {
	llmInvalidOutputTotal.WithLabelValues(provider, purpose).Inc()
}

// RecordLLMCache counts a cache decision.
func RecordLLMCache(purpose, result string) {
	llmCacheTotal.WithLabelValues(purpose, result).Inc()
}

// RecordIncident counts a newly opened incident.
func RecordIncident(class, scope string) {
	incidentsTotal.WithLabelValues(class, scope).Inc()
}

// RecordIncidentRecurrence counts a newly opened incident that is a recurrence of
// a resolved one.
func RecordIncidentRecurrence(class, scope string) {
	incidentRecurrencesTotal.WithLabelValues(class, scope).Inc()
}

// RecordProposal counts one proposal attempt.
func RecordProposal(source, actionType, result string) {
	proposalsTotal.WithLabelValues(source, actionType, result).Inc()
}

// RecordTransition counts a RemediationAction phase change. Writing the same
// phase again is not a transition and is not counted.
func RecordTransition(from, to string) {
	if from == to {
		return
	}
	if from == "" {
		from = PhaseNone
	}
	transitionsTotal.WithLabelValues(from, to).Inc()
}
