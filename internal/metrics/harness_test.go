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
