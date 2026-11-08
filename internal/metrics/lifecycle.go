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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The rest of KPIS.md section 6: the gate's decisions and timings, tickets, heal
// credit, verification, rollbacks and the lifecycle durations behind KPI Q1.
//
// Every label value is drawn from a fixed set (a constant in this package, or a
// gate enum the gate package registers through PreRegisterGate); none is ever an
// object name, a namespace, or text a model wrote. Every family is created with
// its series at zero at start-up (see preRegister), because a Prometheus
// CounterVec with no series is simply absent from /metrics, and a dashboard that
// loads before the first incident then shows "no data" where the truth is "zero".

// Ticket results, the values of the result label on dorgu_tickets_total.
const (
	// TicketIssued: the approval re-check passed and a ticket was written.
	TicketIssued = "issued"
	// TicketExpired: a ticket was found past its expiry when it came to be applied.
	TicketExpired = "expired"
	// TicketRefusedAtApproval: the gate refused to issue a ticket.
	TicketRefusedAtApproval = "refused_at_approval"
	// TicketRefusedAtApply: a ticket was not honoured at apply for any reason but
	// expiry.
	TicketRefusedAtApply = "refused_at_apply"
)

// Verification results, the values of the result label on dorgu_verifications_total.
const (
	VerificationHealthy  = "healthy"
	VerificationDegraded = "degraded"
	VerificationUnknown  = "unknown"
)

// Stages of a heal, the values of the stage label on dorgu_stage_duration_seconds
// (KPI Q1).
const (
	StageDetectToProposal   = "detect_to_proposal"
	StageProposalToDecision = "proposal_to_decision"
	StageDecisionToApplied  = "decision_to_applied"
	StageAppliedToVerified  = "applied_to_verified"
)

// Kinds of model claim the operator stripped, the values of the kind label on
// dorgu_model_claims_stripped_total.
const (
	// ClaimFabricatedClamp: a sentence claiming a guardrail clamp that no recorded
	// verdict supports.
	ClaimFabricatedClamp = "fabricated_clamp"
	// ClaimRefusedValue: a sentence or field offering a value a guardrail refused.
	ClaimRefusedValue = "refused_value"
)

// Heal-credit reasons for dorgu_heal_credit_total. They are the reasons of the
// HealCredited and WorkloadDiverged conditions, so the metric and the record say
// the same thing.
const (
	HealReasonWorkloadPatched    = "WorkloadPatchedByTicket"
	HealReasonNothingPatched     = "NothingPatched"
	HealReasonResourcesNotTicket = "ResourcesNotTicket"
	HealReasonWorkloadDiverged   = "WorkloadDiverged"
	HealReasonWorkloadUnreadable = "WorkloadUnreadable"
)

// OutcomeCleared labels an incident the health cycle resolved on its own evidence
// of recovery, with no RemediationAction involved. It is not a Dorgu heal and is
// kept apart from "resolved", which is.
const OutcomeCleared = "cleared"

// Incident resolution outcomes for dorgu_incidents_resolved_total: the values of
// the IncidentMemory resolution.outcome enum written by the remediation
// controller, plus OutcomeCleared.
var resolutionOutcomes = []string{"resolved", "rollback", "partial", "failed", "acknowledged", "self-resolved", OutcomeCleared}

// rollbackDiverged values for dorgu_rollbacks_total: the workload still carries
// the change (true), does not (false), or could not be read (unknown).
var rollbackDiverged = []string{"true", "false", "unknown"}

var healReasons = []struct {
	credited string
	reason   string
}{
	{"true", HealReasonWorkloadPatched},
	{"false", HealReasonNothingPatched},
	{"false", HealReasonResourcesNotTicket},
	{"false", HealReasonWorkloadDiverged},
	{"false", HealReasonWorkloadUnreadable},
}

var (
	gateDecisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_gate_decisions_total",
		Help: "Gate runs, by phase (Proposal, ApprovalRecheck, Apply), the stage that refused (or \"complete\"), " +
			"result (pass, clamp, refuse, error) and reason (the refusing guardrail enum, or none). KPI Q6.",
	}, []string{"phase", "stage", "result", "reason"})

	gateStageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dorgu_gate_stage_duration_seconds",
		Help:    "Time each gate stage took, by phase and stage.",
		Buckets: []float64{.001, .005, .01, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"phase", "stage"})

	guardrailVerdictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_guardrail_verdicts_total",
		Help: "Guardrail verdicts the gate recorded, by guardrail and verdict (pass, clamp, refuse).",
	}, []string{"guardrail", "verdict"})

	ticketsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_tickets_total",
		Help: "Apply tickets, by result (issued, expired, refused_at_approval, refused_at_apply).",
	}, []string{"result"})

	incidentsResolvedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_incidents_resolved_total",
		Help: "Incidents that reached a resolution, by outcome (resolved, rollback, partial, failed, " +
			"acknowledged, self-resolved, cleared). Only resolved counts as a clean heal; cleared means the incident " +
			"recovered with no Dorgu action involved.",
	}, []string{"outcome"})

	incidentTimeToDetect = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "dorgu_incident_time_to_detect_seconds",
		Help: "From the earliest contributing signal's detection time to the incident being recorded. " +
			"Most signals are stamped when first observed, so this is mostly detector-to-record latency.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 14),
	})

	healCreditTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_heal_credit_total",
		Help: "Healthy verifications, by whether the recovery is credited as a Dorgu heal and the reason. " +
			"Only credited=true counts toward safe heals.",
	}, []string{"credited", "reason"})

	verificationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_verifications_total",
		Help: "Post-apply verification results (healthy, degraded, unknown).",
	}, []string{"result"})

	rollbacksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_rollbacks_total",
		Help: "Rollbacks, by whether the live workload still carries the change afterwards (true, false, unknown).",
	}, []string{"workload_diverged"})

	stageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "dorgu_stage_duration_seconds",
		Help: "Time a heal spent in each stage: detect_to_proposal, proposal_to_decision, " +
			"decision_to_applied, applied_to_verified. KPI Q1.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 16),
	}, []string{"stage"})

	modelClaimsStrippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dorgu_model_claims_stripped_total",
		Help: "Model-authored claims the operator removed, by kind (fabricated_clamp, refused_value).",
	}, []string{"kind"})

	healthCycleDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "dorgu_healthcheck_cycle_duration_seconds",
		Help:    "Duration of one health-check cycle (collect, diagnose, record, propose).",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	})

	llmRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dorgu_llm_request_duration_seconds",
		Help:    "Duration of requests to the LLM provider, by provider and purpose.",
		Buckets: []float64{.25, .5, 1, 2, 4, 8, 16, 32, 64},
	}, []string{"provider", "purpose"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(gateDecisionsTotal, gateStageDuration, guardrailVerdictsTotal, ticketsTotal,
		incidentsResolvedTotal, incidentTimeToDetect, healCreditTotal, verificationsTotal, rollbacksTotal,
		stageDuration, modelClaimsStrippedTotal, healthCycleDuration, llmRequestDuration)
	preRegister()
}

// preRegister creates every series whose labels are known up front, at zero.
func preRegister() {
	for _, o := range resolutionOutcomes {
		incidentsResolvedTotal.WithLabelValues(o)
	}
	for _, r := range []string{TicketIssued, TicketExpired, TicketRefusedAtApproval, TicketRefusedAtApply} {
		ticketsTotal.WithLabelValues(r)
	}
	for _, h := range healReasons {
		healCreditTotal.WithLabelValues(h.credited, h.reason)
	}
	for _, r := range []string{VerificationHealthy, VerificationDegraded, VerificationUnknown} {
		verificationsTotal.WithLabelValues(r)
	}
	for _, d := range rollbackDiverged {
		rollbacksTotal.WithLabelValues(d)
	}
	for _, s := range []string{StageDetectToProposal, StageProposalToDecision, StageDecisionToApplied, StageAppliedToVerified} {
		stageDuration.WithLabelValues(s)
	}
	for _, k := range []string{ClaimFabricatedClamp, ClaimRefusedValue} {
		modelClaimsStrippedTotal.WithLabelValues(k)
	}
	for _, p := range []string{"claude", "gemini"} {
		for _, purpose := range []string{LLMPurposeDiagnosis, LLMPurposePlanning} {
			llmRequestDuration.WithLabelValues(p, purpose)
			llmInvalidOutputTotal.WithLabelValues(p, purpose)
			for _, o := range []string{LLMOutcomeSuccess, LLMOutcomeError} {
				llmRequestsTotal.WithLabelValues(p, purpose, o)
			}
			for _, k := range []string{LLMTokensInput, LLMTokensOutput} {
				llmTokensTotal.WithLabelValues(p, purpose, k)
			}
		}
	}
	for _, purpose := range []string{LLMPurposeDiagnosis, LLMPurposePlanning} {
		for _, r := range []string{LLMCacheHit, LLMCacheMiss} {
			llmCacheTotal.WithLabelValues(purpose, r)
		}
	}
	preRegisterProposals()
	// healthCycleDuration and incidentTimeToDetect are plain histograms: unlike a
	// vec they are always exported, with zero samples, so they need no pre-registration.
}

// preRegisterProposals zeroes the producer/action/result series and the
// incident series for every class and scope the operator can emit, and the
// phase transitions the RemediationAction state machine can make.
func preRegisterProposals() {
	for _, a := range proposalActionTypes {
		for _, r := range []string{ProposalProposed, ProposalSkipped, ProposalSuppressed, ProposalError} {
			proposalsTotal.WithLabelValues(SourceDorgu, a, r)
		}
	}
	for _, class := range incidentClasses {
		for _, scope := range []string{"persona", "unattributed"} {
			incidentsTotal.WithLabelValues(class, scope)
		}
	}
	for _, t := range phaseTransitions {
		transitionsTotal.WithLabelValues(t[0], t[1])
	}
}

// proposalActionTypes are the diagnosis SuggestedAction values.
var proposalActionTypes = []string{
	"resource-adjustment", "restart", "node-restart", "scale-up", "deployment-fix", "investigate", "rollback", "health",
}

// incidentClasses are the diagnosis categories an incident can carry.
var incidentClasses = []string{
	"resource", "health", "node", "controlplane", "deployment", "dependency", "scaling", "security",
}

// phaseTransitions are the RemediationAction phase changes the controller can write.
var phaseTransitions = [][2]string{
	{PhaseNone, "Pending"},
	{PhaseNone, "Approved"},
	{PhaseNone, "Rejected"},
	{"Pending", "Approved"},
	{"Pending", "Rejected"},
	{"Pending", "Expired"},
	{"Approved", "Ticketed"},
	{"Approved", "Applying"},
	{"Approved", "Acknowledged"},
	{"Approved", "Expired"},
	{"Approved", "Failed"},
	{"Ticketed", "Applying"},
	{"Ticketed", "Failed"},
	{"Applying", "Verifying"},
	{"Applying", "Failed"},
	{"Verifying", "Completed"},
	{"Verifying", "RolledBack"},
	{"Verifying", "Failed"},
}

// GateSeries is one dorgu_gate_decisions_total series to create at zero.
type GateSeries struct{ Phase, Stage, Result, Reason string }

// GateStagePair is one dorgu_gate_stage_duration_seconds series to create.
type GateStagePair struct{ Phase, Stage string }

// PreRegisterGate creates the gate's series at zero. The gate package owns the
// enums (stages, reasons, guardrails) and calls this from its own init, so this
// package never imports the gate.
func PreRegisterGate(decisions []GateSeries, stages []GateStagePair, guardrails []string) {
	for _, s := range decisions {
		gateDecisionsTotal.WithLabelValues(s.Phase, s.Stage, s.Result, s.Reason)
	}
	for _, s := range stages {
		gateStageDuration.WithLabelValues(s.Phase, s.Stage)
	}
	for _, g := range guardrails {
		for _, v := range []string{"pass", "clamp", "refuse"} {
			guardrailVerdictsTotal.WithLabelValues(g, v)
		}
	}
}

// RecordGateDecision counts one gate run.
func RecordGateDecision(phase, stage, result, reason string) {
	gateDecisionsTotal.WithLabelValues(phase, stage, result, reason).Inc()
}

// RecordGateStageDuration observes how long one gate stage took.
func RecordGateStageDuration(phase, stage string, d time.Duration) {
	gateStageDuration.WithLabelValues(phase, stage).Observe(d.Seconds())
}

// RecordGuardrailVerdict counts one verdict a guardrail recorded.
func RecordGuardrailVerdict(guardrail, verdict string) {
	guardrailVerdictsTotal.WithLabelValues(guardrail, verdict).Inc()
}

// RecordTicket counts a ticket event (TicketIssued and friends).
func RecordTicket(result string) { ticketsTotal.WithLabelValues(result).Inc() }

// RecordIncidentResolved counts an incident reaching a resolution outcome.
func RecordIncidentResolved(outcome string) { incidentsResolvedTotal.WithLabelValues(outcome).Inc() }

// RecordTimeToDetect observes the time from the earliest signal to the incident
// record. A negative duration is clock skew, not a measurement, and is dropped.
func RecordTimeToDetect(d time.Duration) {
	if d < 0 {
		return
	}
	incidentTimeToDetect.Observe(d.Seconds())
}

// RecordHealCredit counts a healthy verification as credited or not, with why.
func RecordHealCredit(credited bool, reason string) {
	value := "false"
	if credited {
		value = "true"
	}
	healCreditTotal.WithLabelValues(value, reason).Inc()
}

// RecordVerification counts a verification result.
func RecordVerification(result string) { verificationsTotal.WithLabelValues(result).Inc() }

// RecordRollback counts a rollback by whether the workload still diverges.
func RecordRollback(workloadDiverged string) { rollbacksTotal.WithLabelValues(workloadDiverged).Inc() }

// RecordStageDuration observes how long a heal spent in a stage. A non-positive
// duration means a missing or skewed timestamp and is dropped rather than
// recorded as an instant.
func RecordStageDuration(stage string, d time.Duration) {
	if d <= 0 {
		return
	}
	stageDuration.WithLabelValues(stage).Observe(d.Seconds())
}

// RecordModelClaimsStripped counts model-authored claims removed from a plan.
func RecordModelClaimsStripped(kind string, n int) {
	if n <= 0 {
		return
	}
	modelClaimsStrippedTotal.WithLabelValues(kind).Add(float64(n))
}

// RecordHealthCycle observes the duration of one health-check cycle.
func RecordHealthCycle(d time.Duration) { healthCycleDuration.Observe(d.Seconds()) }

// RecordLLMRequestDuration observes the duration of one provider request.
func RecordLLMRequestDuration(provider, purpose string, d time.Duration) {
	llmRequestDuration.WithLabelValues(provider, purpose).Observe(d.Seconds())
}
