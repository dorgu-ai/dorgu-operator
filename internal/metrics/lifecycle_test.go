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
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// gathered returns every family on the registry, keyed by name, without any
// Record* call having been made by the test. That is the point: a dashboard
// that loads before the first incident must find the series.
func gathered(t *testing.T) map[string]int {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	out := map[string]int{}
	for _, f := range families {
		out[f.GetName()] = len(f.GetMetric())
	}
	return out
}

// Dogfood finding D0.6: lazy dorgu_* counters made every panel read "no data"
// until the first incident. Each family below must exist, with at least one
// series at zero, straight after start-up.
func TestEveryHarnessFamilyIsPreRegisteredAtZero(t *testing.T) {
	got := gathered(t)
	for _, name := range []string{
		"dorgu_incidents_total",
		"dorgu_incidents_resolved_total",
		"dorgu_incident_time_to_detect_seconds",
		"dorgu_proposals_total",
		"dorgu_remediation_transitions_total",
		"dorgu_tickets_total",
		"dorgu_heal_credit_total",
		"dorgu_verifications_total",
		"dorgu_rollbacks_total",
		"dorgu_stage_duration_seconds",
		"dorgu_model_claims_stripped_total",
		"dorgu_healthcheck_cycle_duration_seconds",
		"dorgu_llm_requests_total",
		"dorgu_llm_tokens_total",
		"dorgu_llm_invalid_output_total",
		"dorgu_llm_cache_total",
		"dorgu_llm_request_duration_seconds",
	} {
		if got[name] == 0 {
			t.Errorf("%s has no series at start-up: a dashboard would show \"no data\" until the first event", name)
		}
	}
}

// seriesValue reads a counter series from the registry WITHOUT creating it, so
// "present" means pre-registered and not "this assertion made it".
func seriesValue(t *testing.T, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	next:
		for _, m := range f.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			for _, l := range m.GetLabel() {
				if labels[l.GetName()] != l.GetValue() {
					continue next
				}
			}
			return m.GetCounter().GetValue(), true
		}
	}
	return 0, false
}

// F-11: the series are read from the registry, not created by the read. These
// are series no test in this package increments, so they must still be zero.
func TestPreRegisteredCountersExistAndStartAtZero(t *testing.T) {
	for name, tc := range map[string]struct {
		metric string
		labels map[string]string
	}{
		"incidents resolved": {"dorgu_incidents_resolved_total", map[string]string{"outcome": "acknowledged"}},
		"tickets refused":    {"dorgu_tickets_total", map[string]string{"result": TicketRefusedAtApproval}},
		"heal not credited":  {"dorgu_heal_credit_total", map[string]string{"credited": "false", "reason": HealReasonWorkloadUnreadable}},
		"verify unknown":     {"dorgu_verifications_total", map[string]string{"result": VerificationUnknown}},
		"proposal deferred":  {"dorgu_proposals_total", map[string]string{"source": SourceDorgu, "action_type": "restart", "result": ProposalDeferred}},
		"transition":         {"dorgu_remediation_transitions_total", map[string]string{"from": "Approved", "to": "Ticketed"}},
		"llm cache":          {"dorgu_llm_cache_total", map[string]string{"purpose": LLMPurposeDiagnosis, "result": LLMCacheMiss}},
	} {
		got, present := seriesValue(t, tc.metric, tc.labels)
		if !present {
			t.Errorf("%s: %s %v is not pre-registered", name, tc.metric, tc.labels)
		} else if got != 0 {
			t.Errorf("%s = %v before anything happened, want 0", name, got)
		}
	}
}

func TestPreRegisterGateCreatesTheSeriesItIsGiven(t *testing.T) {
	labels := map[string]string{"phase": "Proposal", "stage": "Validate", "result": "deferred", "reason": "synthetic-reason"}
	if _, present := seriesValue(t, "dorgu_gate_decisions_total", labels); present {
		t.Fatal("the synthetic series exists before it was registered")
	}

	PreRegisterGate([]GateSeries{{Phase: "Proposal", Stage: "Validate", Result: "deferred", Reason: "synthetic-reason"}},
		[]GateStagePair{{Phase: "Proposal", Stage: "Validate"}}, []string{"synthetic-guardrail"})

	if got, present := seriesValue(t, "dorgu_gate_decisions_total", labels); !present || got != 0 {
		t.Fatalf("after PreRegisterGate the series is present=%v value=%v, want present at 0", present, got)
	}
	if _, present := seriesValue(t, "dorgu_guardrail_verdicts_total", map[string]string{"guardrail": "synthetic-guardrail", "verdict": "refuse"}); !present {
		t.Error("the guardrail verdict series were not registered")
	}
	RecordGateDecision("Proposal", "Validate", "deferred", "synthetic-reason")
	if got, _ := seriesValue(t, "dorgu_gate_decisions_total", labels); got != 1 {
		t.Errorf("after one decision the series = %v, want 1", got)
	}
}

func TestRecordGateStageDuration(t *testing.T) {
	RecordGateStageDuration("ApprovalRecheck", "Ownership", 40*time.Millisecond)
	if got := testutil.CollectAndCount(gateStageDuration); got == 0 {
		t.Error("no gate stage duration series after an observation")
	}
}

func TestRecordGuardrailVerdict(t *testing.T) {
	series := guardrailVerdictsTotal.WithLabelValues("blast-radius", "clamp")
	before := testutil.ToFloat64(series)
	RecordGuardrailVerdict("blast-radius", "clamp")
	if got := testutil.ToFloat64(series) - before; got != 1 {
		t.Errorf("delta = %v, want 1", got)
	}
}

func TestRecordTicketAndHealCreditAndVerification(t *testing.T) {
	ticket := ticketsTotal.WithLabelValues(TicketExpired)
	credit := healCreditTotal.WithLabelValues("false", "NothingPatched")
	verify := verificationsTotal.WithLabelValues(VerificationDegraded)
	t0, c0, v0 := testutil.ToFloat64(ticket), testutil.ToFloat64(credit), testutil.ToFloat64(verify)

	RecordTicket(TicketExpired)
	RecordHealCredit(false, "NothingPatched")
	RecordVerification(VerificationDegraded)

	if got := testutil.ToFloat64(ticket) - t0; got != 1 {
		t.Errorf("tickets delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(credit) - c0; got != 1 {
		t.Errorf("heal credit delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(verify) - v0; got != 1 {
		t.Errorf("verification delta = %v, want 1", got)
	}
}

func TestRecordIncidentResolvedAndTimeToDetect(t *testing.T) {
	series := incidentsResolvedTotal.WithLabelValues("self-resolved")
	before := testutil.ToFloat64(series)
	RecordIncidentResolved("self-resolved")
	if got := testutil.ToFloat64(series) - before; got != 1 {
		t.Errorf("resolved delta = %v, want 1", got)
	}

	// A negative duration (clock skew between a signal and the operator) is not a
	// measurement and must not be observed as one.
	count := func() uint64 { return histogramCount(t, "dorgu_incident_time_to_detect_seconds") }
	c0 := count()
	RecordTimeToDetect(-5 * time.Second)
	if count() != c0 {
		t.Error("a negative time-to-detect was observed")
	}
	RecordTimeToDetect(30 * time.Second)
	if count() != c0+1 {
		t.Error("a time-to-detect of 30s was not observed")
	}
}

func TestRecordStageDurationIgnoresNonPositive(t *testing.T) {
	c0 := histogramCount(t, "dorgu_stage_duration_seconds")
	RecordStageDuration(StageDecisionToApplied, -time.Second)
	RecordStageDuration(StageDecisionToApplied, 0)
	if histogramCount(t, "dorgu_stage_duration_seconds") != c0 {
		t.Error("a non-positive stage duration was observed")
	}
	RecordStageDuration(StageDecisionToApplied, 2*time.Second)
	if histogramCount(t, "dorgu_stage_duration_seconds") != c0+1 {
		t.Error("a 2s stage duration was not observed")
	}
}

func TestRecordRollbackClaimsAndCycle(t *testing.T) {
	rb := rollbacksTotal.WithLabelValues("true")
	claim := modelClaimsStrippedTotal.WithLabelValues(ClaimRefusedValue)
	r0, c0 := testutil.ToFloat64(rb), testutil.ToFloat64(claim)
	cycle0 := histogramCount(t, "dorgu_healthcheck_cycle_duration_seconds")
	llm0 := histogramCount(t, "dorgu_llm_request_duration_seconds")

	RecordRollback("true")
	RecordModelClaimsStripped(ClaimRefusedValue, 3)
	RecordModelClaimsStripped(ClaimRefusedValue, 0)
	RecordHealthCycle(1500 * time.Millisecond)
	RecordLLMRequestDuration("claude", LLMPurposeDiagnosis, 2*time.Second)

	if got := testutil.ToFloat64(rb) - r0; got != 1 {
		t.Errorf("rollbacks delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(claim) - c0; got != 3 {
		t.Errorf("claims delta = %v, want 3 (a zero count adds nothing)", got)
	}
	if histogramCount(t, "dorgu_healthcheck_cycle_duration_seconds") != cycle0+1 {
		t.Error("cycle duration not observed")
	}
	if histogramCount(t, "dorgu_llm_request_duration_seconds") != llm0+1 {
		t.Error("llm request duration not observed")
	}
}

// histogramCount sums the sample counts over every series of a histogram family.
func histogramCount(t *testing.T, name string) uint64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	var total uint64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetHistogram().GetSampleCount()
		}
	}
	return total
}
