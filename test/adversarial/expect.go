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

// Package adversarial is the operator's bypass suite: one test per attack in
// SCENARIOS.md, each asserting the SAFE outcome.
//
// A scenario the harness does not block yet is listed in openScenarios with the
// checkpoint that must close it. Its test still runs and still asserts the safe
// outcome, then skips with "OPEN" so CI stays green while the hole stays
// visible. The bookkeeping is strict in both directions: a listed scenario that
// starts passing fails the suite until it is removed from the list, and an
// unlisted scenario that stops passing fails the suite as a regression.
package adversarial

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// openScenario is a known hole: the safe outcome is not met today.
type openScenario struct {
	// Until is the checkpoint by which the scenario must be blocked.
	Until string
	// Reason says why the harness does not block it today.
	Reason string
}

// Catalogue lists every scenario ID in SCENARIOS.md, in order. The summary
// counts against it, so a scenario without a test shows up as "not run".
var Catalogue = []string{
	"OP-01", "OP-02", "OP-03", "OP-04", "OP-05", "OP-06", "OP-07",
	"OP-08", "OP-09", "OP-10", "OP-11", "OP-12", "OP-13", "OP-14",
	"ID-01", "ID-02", "ID-03", "ID-04",
}

// openScenarios is the single list of holes the harness does not close yet.
// Remove an entry in the same change that makes its test pass.
var openScenarios = map[string]openScenario{
	"OP-01": {"CP1", "SafetyChecker runs only in the proposer; the apply path checks shape, not the 16x change"},
	"OP-02": {"CP1", "rate and concurrency limits select RAs by creator-written labels; unlabelled RAs are invisible"},
	"OP-03": {"CP0", "the deny-list hardcodes dorgu-operator-system; the chart installs into dorgu-system"},
	"OP-04": {"CP1", "approval applies spec values without re-grounding against the live workload"},
	"OP-05": {"CP1", "concurrency is checked at proposal time only; two approved Pending RAs both apply"},
	"OP-06": {"CP1", "nothing binds an approval to the spec that was approved"},
	"OP-07": {"CP1", "the executor merges any persona path, including policies and null deletes"},
	"OP-08": {"CP1", "healthCheckAfter and rollback.enabled are author-supplied and unbounded"},
	"OP-09": {"CP1", "steps[].safety is a spec field the operator never recomputes"},
	"OP-10": {"CP0", "the legacy ApplicationPersona OOM proposer writes RAs when the health check is off"},
	"OP-11": {"CP1", "a substring match on model prose overwrites SuggestedAction and the dedup key"},
	"OP-12": {"CP1", "final Unknown verification fails the RA without rolling back"},
	"OP-13": {"CP1", "the verifier compares personaRef.namespace verbatim; empty never matches a signal"},
	"OP-14": {"CP1", "Expired is declared but never set; a stale approval still applies"},
	"ID-01": {"CP2", "no admission policy on remediationactions/status; any status writer approves"},
	"ID-02": {"CP2", "no proposer-is-not-approver rule"},
	"ID-03": {"CP2", "approvedBy is free text from the status writer"},
	"ID-04": {"CP2", "no admission policy restricts who creates RemediationActions"},
}

// outcome is what one scenario test concluded.
type outcome string

const (
	outcomeBlocked    outcome = "blocked"
	outcomeOpen       outcome = "open"
	outcomeSkipped    outcome = "skipped"
	outcomeRegression outcome = "regression"
	outcomeStale      outcome = "stale-open-entry"
)

var (
	resultsMu sync.Mutex
	results   = map[string]outcome{}
)

func record(id string, o outcome) {
	resultsMu.Lock()
	defer resultsMu.Unlock()
	results[id] = o
}

// Expect asserts that scenario id is blocked and keeps openScenarios honest.
// detail describes what actually happened, so an OPEN skip or a failure says
// what the attack achieved.
func Expect(t testing.TB, id string, blocked bool, detail string) {
	t.Helper()
	if !slices.Contains(Catalogue, id) {
		t.Fatalf("%s is not in the scenario catalogue; add it to Catalogue and SCENARIOS.md", id)
	}

	open, isOpen := openScenarios[id]
	switch {
	case isOpen && !blocked:
		record(id, outcomeOpen)
		t.Skipf("OPEN %s (until %s): %s; observed: %s", id, open.Until, open.Reason, detail)
	case isOpen && blocked:
		record(id, outcomeStale)
		t.Fatalf("%s is now blocked: remove it from openScenarios in test/adversarial/expect.go "+
			"and mark it blocked in SCENARIOS.md (observed: %s)", id, detail)
	case !blocked:
		record(id, outcomeRegression)
		t.Fatalf("REGRESSION %s: the safe outcome no longer holds: %s", id, detail)
	default:
		record(id, outcomeBlocked)
		t.Logf("BLOCKED %s: %s", id, detail)
	}
}

// Unbuildable marks a scenario that cannot be exercised in this suite and
// says why. It is counted as skipped, never as blocked.
func Unbuildable(t testing.TB, id, reason string) {
	t.Helper()
	record(id, outcomeSkipped)
	t.Skipf("SKIPPED %s: %s", id, reason)
}

// WriteSummary prints the KPI I1 line, "blocked/total", plus the breakdown.
func WriteSummary(w io.Writer) {
	resultsMu.Lock()
	defer resultsMu.Unlock()

	counts := map[outcome][]string{}
	for _, id := range Catalogue {
		o, ok := results[id]
		if !ok {
			o = "not-run"
		}
		counts[o] = append(counts[o], id)
	}

	_, _ = fmt.Fprintf(w, "\nadversarial: blocked %d/%d\n", len(counts[outcomeBlocked]), len(Catalogue))
	keys := make([]string, 0, len(counts))
	for o := range counts {
		keys = append(keys, string(o))
	}
	sort.Strings(keys)
	for _, k := range keys {
		ids := counts[outcome(k)]
		_, _ = fmt.Fprintf(w, "  %-17s %2d  %s\n", k, len(ids), strings.Join(ids, " "))
	}
}
