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

	proposalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_proposals_total",
		Help: "Remediation proposal attempts, by producer (source), suggested action and result " +
			"(proposed, skipped, suppressed, error).",
	}, []string{"source", "action_type", "result"})

	transitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_remediation_transitions_total",
		Help: "RemediationAction phase transitions written by the operator.",
	}, []string{"from", "to"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(incidentsTotal, proposalsTotal, transitionsTotal)
}

// RecordIncident counts a newly opened incident.
func RecordIncident(class, scope string) {
	incidentsTotal.WithLabelValues(class, scope).Inc()
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
