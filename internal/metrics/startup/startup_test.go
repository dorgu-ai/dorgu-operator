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

// Package startup checks the operator's metrics registry in a binary of its own,
// where nothing has recorded anything: what is there is exactly what start-up
// pre-registered. Every other test in the repository shares a registry that
// earlier tests have already written to, so only here can "present before the
// first event" be asserted without a test having created the series itself.
package startup

import (
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

func key(labels map[string]string, order ...string) string {
	var out strings.Builder
	for _, name := range order {
		out.WriteString(labels[name] + "/")
	}
	return out.String()
}

func TestEveryDorguSeriesExistsAtZeroBeforeAnythingHappens(t *testing.T) {
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	byName := map[string][]map[string]string{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			byName[f.GetName()] = append(byName[f.GetName()], labels)
			switch {
			case m.GetCounter() != nil && m.GetCounter().GetValue() != 0:
				t.Errorf("%s %v = %v at start-up, want 0", f.GetName(), labels, m.GetCounter().GetValue())
			case m.GetHistogram() != nil && m.GetHistogram().GetSampleCount() != 0:
				t.Errorf("%s %v has samples at start-up", f.GetName(), labels)
			}
		}
	}

	for _, name := range []string{
		"dorgu_incidents_total", "dorgu_incidents_resolved_total", "dorgu_incident_time_to_detect_seconds",
		"dorgu_proposals_total", "dorgu_remediation_transitions_total", "dorgu_gate_decisions_total",
		"dorgu_gate_stage_duration_seconds", "dorgu_guardrail_verdicts_total", "dorgu_tickets_total",
		"dorgu_heal_credit_total", "dorgu_verifications_total", "dorgu_rollbacks_total", "dorgu_stage_duration_seconds",
		"dorgu_model_claims_stripped_total", "dorgu_healthcheck_cycle_duration_seconds", "dorgu_llm_requests_total",
		"dorgu_llm_tokens_total", "dorgu_llm_invalid_output_total", "dorgu_llm_cache_total", "dorgu_llm_request_duration_seconds",
	} {
		if len(byName[name]) == 0 {
			t.Errorf("%s has no series at start-up: a dashboard would show \"no data\" until the first event", name)
		}
	}

	have := map[string]bool{}
	for _, labels := range byName["dorgu_gate_decisions_total"] {
		have[key(labels, "phase", "stage", "result", "reason")] = true
	}
	for _, s := range gate.PreRegisteredDecisionSeries() {
		if !have[s.Phase+"/"+s.Stage+"/"+s.Result+"/"+s.Reason+"/"] {
			t.Errorf("gate series %+v is missing from the registry at start-up", s)
		}
	}
}
