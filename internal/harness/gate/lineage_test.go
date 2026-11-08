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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

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

	long := "im-" + strings.Repeat("a", 230) + "-" + strings.Repeat("b", 12) // 246 chars, valid base
	name := RecurrenceName(long, 12)
	assert.LessOrEqual(t, len(name), 253)
	assert.True(t, strings.HasSuffix(name, strings.Repeat("b", 12)+"-r12"), "the hash and the suffix survive truncation: %s", name)
	assert.Equal(t, 12, IncidentGeneration(name))
	assert.Equal(t, IncidentLineage(RecurrenceName(long, 3)), IncidentLineage(name), "all generations of a long name still share a lineage")
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
