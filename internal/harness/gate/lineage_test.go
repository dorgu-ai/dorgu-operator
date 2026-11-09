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

package gate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const sigOOM = "OOMKilled"

const baseIncident = "im-shop-web-oomkilled-a476546e10c8"

func TestIncidentLineage(t *testing.T) {
	tests := []struct {
		name, in, lineage string
		generation        int
	}{
		{"first occurrence", baseIncident, baseIncident, 0},
		{"first recurrence", baseIncident + "-r1", baseIncident, 1},
		{"tenth recurrence", baseIncident + "-r10", baseIncident, 10},
		{"a persona that happens to be called r2", "im-shop-r2-oomkilled-a476546e10c8", "im-shop-r2-oomkilled-a476546e10c8", 0},
		{"empty", "", "", 0},
		{"a suffix too large for an int is no generation we wrote", baseIncident + "-r99999999999999999999999", baseIncident, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.lineage, IncidentLineage(tt.in))
			assert.Equal(t, tt.generation, IncidentGeneration(tt.in))
		})
	}
}

func TestSameIncidentLineage(t *testing.T) {
	assert.True(t, SameIncidentLineage(baseIncident, baseIncident+"-r3"))
	assert.True(t, SameIncidentLineage(baseIncident+"-r1", baseIncident+"-r2"))
	assert.False(t, SameIncidentLineage(baseIncident, "im-shop-web-crashloopbackoff-0123456789ab"))
	assert.False(t, SameIncidentLineage("", ""), "two unnamed incidents are not the same fault")
}

func TestRecurrenceName(t *testing.T) {
	assert.Equal(t, baseIncident, RecurrenceName(baseIncident, 0))
	assert.Equal(t, baseIncident+"-r1", RecurrenceName(baseIncident, 1))
	assert.Equal(t, baseIncident+"-r4", RecurrenceName(baseIncident+"-r2", 4), "a generation is applied to the base, never stacked")
	assert.Equal(t, "short-r2", RecurrenceName("short", 2), "a base with no room to shorten is only suffixed")
}

// longBase builds a valid incident name of exactly n characters.
func longBase(n int) string {
	return "im-" + strings.Repeat("a", n-3-13) + "-" + strings.Repeat("b", 12)
}

// Truncation really happens for these: the longest base the generator can emit is
// 252 characters, and "-r12" does not fit beside it. Every generation of one
// fault must keep one lineage whatever the length, or every cooldown keyed on it
// would let the recurrence through.
func TestRecurrenceName_LongBasesKeepTheirLineage(t *testing.T) {
	for _, n := range []int{240, 241, 245, 250, 252, 253} {
		base := longBase(n)
		require.Len(t, base, n)
		for _, gen := range []int{1, 2, 12, 1000, 1234567890} {
			name := RecurrenceName(base, gen)
			assert.LessOrEqual(t, len(name), maxIncidentNameLength, "base %d gen %d", n, gen)
			assert.True(t, strings.HasSuffix(name, strings.Repeat("b", 12)+fmt.Sprintf("-r%d", gen)),
				"the hash and the suffix survive: %s", name)
			assert.Equal(t, gen, IncidentGeneration(name))
			assert.True(t, SameIncidentLineage(base, name), "base %d gen %d left the lineage of generation 0", n, gen)
			assert.True(t, SameIncidentLineage(RecurrenceName(base, 1), name), "base %d gen %d", n, gen)
		}
	}
}

// The invariant the lineage rests on, asserted for every length the generator can
// reach and a few it cannot.
func TestLineage_IsStableUnderRecurrenceNaming(t *testing.T) {
	for n := 20; n <= 253; n++ {
		base := longBase(n)
		for _, gen := range []int{1, 9, 10, 99, 100} {
			if !SameIncidentLineage(base, RecurrenceName(base, gen)) {
				t.Fatalf("length %d generation %d: lineage changed", n, gen)
			}
		}
	}
}

// The cooldowns are decided by the gate and keyed on the incident. A recurrence
// is a new incident, so they must compare lineages or a human's no, and a
// proposal already outstanding, would evaporate the moment the fault came back.
func lineageCandidate(incident string) *Candidate {
	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: incident, Namespace: testNS},
		Spec: dorguv1.IncidentMemorySpec{
			Severity:  "critical",
			Detection: dorguv1.DetectionInfo{Signal: "LivenessRestarts"},
		},
	}
	return &Candidate{Facts: Facts{Incident: im, PrimarySignal: "LivenessRestarts", Severity: "critical"}}
}

func raFor(name, incident, phase string) dorguv1.RemediationAction {
	return dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec:       dorguv1.RemediationActionSpec{IncidentRef: dorguv1.IncidentReference{Name: incident, Namespace: testNS}},
		Status:     dorguv1.RemediationActionStatus{Phase: phase},
	}
}

func TestActiveRemediationCoversTheWholeLineage(t *testing.T) {
	cand := lineageCandidate(baseIncident + "-r1")
	pending := raFor("ra-1", baseIncident, "Pending")
	assert.NotEmpty(t, activeRemediationReason(cand, []dorguv1.RemediationAction{pending}),
		"a proposal outstanding for the earlier incident of this fault already answers its recurrence")

	done := raFor("ra-1", baseIncident, "Completed")
	assert.Empty(t, activeRemediationReason(cand, []dorguv1.RemediationAction{done}),
		"a finished fix does not: the fault is back and deserves a new proposal")

	other := raFor("ra-2", "im-shop-web-crashloopbackoff-0123456789ab", "Pending")
	assert.Empty(t, activeRemediationReason(cand, []dorguv1.RemediationAction{other}),
		"another fault's proposal is not this one's")
}

func TestRejectionCooldownCoversTheWholeLineage(t *testing.T) {
	g := &Gate{now: func() time.Time { return fixedNow }}
	s := dedupCooldownStage{g: g}
	cand := lineageCandidate(baseIncident + "-r2")
	rejected := func(ago time.Duration) dorguv1.RemediationAction {
		ra := raFor("ra-1", baseIncident+"-r1", phaseRejected)
		ra.Status.Conditions = []metav1.Condition{{
			Type: ConditionRejected, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(fixedNow.Add(-ago)),
		}}
		return ra
	}

	assert.NotEmpty(t, s.rejectionReason(cand, []dorguv1.RemediationAction{rejected(10 * time.Minute)}),
		"a rejection of the earlier incident's fix holds for its recurrence")
	assert.Empty(t, s.rejectionReason(cand, []dorguv1.RemediationAction{rejected(2 * time.Hour)}),
		"until the cooldown is over")

	elsewhere := raFor("ra-9", "im-shop-web-crashloopbackoff-0123456789ab", phaseRejected)
	elsewhere.Status.Conditions = []metav1.Condition{{Type: ConditionRejected, LastTransitionTime: metav1.NewTime(fixedNow)}}
	assert.Empty(t, s.rejectionReason(cand, []dorguv1.RemediationAction{elsewhere}),
		"a rejection on a different fault does not suppress this one")
}

// F-2: the rate limit runs in the guardrails stage, BEFORE dedup, so it has to
// recognise the lineage's own outstanding proposal too. Counting it as "another
// remediation" turned a deferral into a Guardrails/rate-limit refusal.
func TestOwnActiveProposalCoversTheWholeLineage(t *testing.T) {
	cand := lineageCandidate(baseIncident + "-r1")
	assert.True(t, ownActiveProposal(cand, ptr(raFor("ra", baseIncident, "Pending"))),
		"the earlier incident's outstanding proposal is this fault's own")
	assert.True(t, ownActiveProposal(cand, ptr(raFor("ra", baseIncident+"-r1", "Applying"))))
	assert.False(t, ownActiveProposal(cand, ptr(raFor("ra", baseIncident, "Completed"))),
		"a FINISHED proposal still counts toward the limit: that is what stops propose, fail, propose")
	assert.False(t, ownActiveProposal(cand, ptr(raFor("ra", "im-shop-web-crashloopbackoff-0123456789ab", "Pending"))))
}

func ptr[T any](v T) *T { return &v }

// Re-review N-3: a rejection is a no to the shape of the fault the human saw. A
// no to the milder plan must not silence the proposal for a strictly worse shape
// of the same fault, and a no to the worse shape still holds for the milder one.
func TestRejectionCooldownIsForTheShapeThatWasRejected(t *testing.T) {
	g := &Gate{now: func() time.Time { return fixedNow }}
	s := dedupCooldownStage{g: g}
	rejected := func(answers string, ago time.Duration) dorguv1.RemediationAction {
		ra := raFor("ra-1", baseIncident, phaseRejected)
		if answers != "" {
			ra.Annotations = map[string]string{AnnotationAnswersSignal: answers}
		}
		ra.Status.Conditions = []metav1.Condition{{
			Type: ConditionRejected, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(fixedNow.Add(-ago)),
		}}
		return ra
	}
	candidate := func(signal string) *Candidate {
		cand := lineageCandidate(baseIncident)
		cand.Facts.PrimarySignal = signal
		cand.Facts.Incident.Spec.Detection.Signal = signal // the incident has escalated to it
		return cand
	}
	tests := []struct {
		name     string
		rejected string
		now      string
		held     bool
	}{
		{"a no to readiness does not silence an OOM", "ReadinessNotReady", sigOOM, false},
		{"a no to readiness does not silence a crash loop", "ReadinessNotReady", "CrashLoopBackOff", false},
		{"a no to a restart loop does not silence the liveness finding", "RestartLoop", "LivenessRestarts", false},
		{"a no to the OOM still holds when it is only readiness again", sigOOM, "ReadinessNotReady", true},
		{"a no to the same shape holds", "ReadinessNotReady", "ReadinessNotReady", true},
		{"a rejection that does not say what it answered holds, as before", "", sigOOM, true},
		{"a signal outside the progression is compared as before", "ImagePullBackOff", "ImagePullBackOff", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := s.rejectionReason(candidate(tt.now), []dorguv1.RemediationAction{rejected(tt.rejected, 10*time.Minute)})
			assert.Equal(t, tt.held, reason != "", "reason: %q", reason)
		})
	}
}

func TestRejectionCooldownStillExpires(t *testing.T) {
	g := &Gate{now: func() time.Time { return fixedNow }}
	s := dedupCooldownStage{g: g}
	ra := raFor("ra-1", baseIncident, phaseRejected)
	ra.Annotations = map[string]string{AnnotationAnswersSignal: sigOOM}
	ra.Status.Conditions = []metav1.Condition{{
		Type: ConditionRejected, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(fixedNow.Add(-2 * time.Hour)),
	}}
	cand := lineageCandidate(baseIncident)
	cand.Facts.PrimarySignal = "ReadinessNotReady"
	cand.Facts.Incident.Spec.Detection.Signal = "ReadinessNotReady"
	assert.Empty(t, s.rejectionReason(cand, []dorguv1.RemediationAction{ra}))
}

// A rejection that does not say which shape it answered (made before proposals
// recorded it, or by hand) is judged by the incident's escalation time: a no that
// predates the last escalation was a no to an earlier, milder shape.
func TestAnUnannotatedRejectionBeforeTheEscalationDoesNotHoldForTheWorseShape(t *testing.T) {
	g := &Gate{now: func() time.Time { return fixedNow }}
	s := dedupCooldownStage{g: g}
	rejection := func() dorguv1.RemediationAction {
		ra := raFor("ra-1", baseIncident, phaseRejected)
		ra.Status.Conditions = []metav1.Condition{{
			Type: ConditionRejected, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(fixedNow.Add(-10 * time.Minute)),
		}}
		return ra
	}
	candidate := func(escalatedAt string) *Candidate {
		cand := lineageCandidate(baseIncident)
		cand.Facts.PrimarySignal = sigOOM
		cand.Facts.Incident.Spec.Detection.Signal = sigOOM
		if escalatedAt != "" {
			cand.Facts.Incident.Annotations = map[string]string{AnnotationEscalatedAt: escalatedAt}
		}
		return cand
	}
	stamp := func(d time.Duration) string { return fixedNow.Add(d).Format(time.RFC3339) }

	assert.Empty(t, s.rejectionReason(candidate(stamp(-time.Minute)), []dorguv1.RemediationAction{rejection()}),
		"escalated a minute ago, rejected ten minutes ago: the no was to the earlier shape")
	assert.NotEmpty(t, s.rejectionReason(candidate(stamp(-30*time.Minute)), []dorguv1.RemediationAction{rejection()}),
		"rejected after the escalation: it was to this shape")
	assert.NotEmpty(t, s.rejectionReason(candidate(""), []dorguv1.RemediationAction{rejection()}),
		"an incident that never escalated: as before")
	assert.NotEmpty(t, s.rejectionReason(candidate("garbage"), []dorguv1.RemediationAction{rejection()}),
		"an unreadable stamp changes nothing")
}
