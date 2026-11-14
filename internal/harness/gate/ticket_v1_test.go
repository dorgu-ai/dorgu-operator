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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Ticket v1 (P2-D9, task 2.6): the ticket binds the spec that was cleared
// (specHash), the human who approved it (approvedBy) and a short life (expiresAt).

func issuedTicket(t *testing.T, g *Gate, ra *dorguv1.RemediationAction) *dorguv1.ApplyTicket {
	t.Helper()
	d := approvalRun(t, g, ra)
	require.True(t, d.Allowed(), d.SkipReason())
	ticket, err := d.Ticket()
	require.NoError(t, err)
	return ticket
}

func TestTicketV1_CarriesTheSpecHashTheApproverAndTheExpiry(t *testing.T) {
	ra := proposedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApprovedBy = "alice@example.com" })
	want, err := ra.Spec.Hash()
	require.NoError(t, err)

	ticket := issuedTicket(t, standardGate(t), ra)

	assert.Equal(t, want, ticket.SpecHash, "specHash is the hash of the spec the gate cleared")
	assert.Equal(t, "alice@example.com", ticket.ApprovedBy, "approvedBy is copied from status at approval")
	require.NotNil(t, ticket.IssuedAt)
	assert.True(t, ticket.ExpiresAt.Time.Equal(ticket.IssuedAt.Add(15*time.Minute)), "expiresAt is issuedAt + the 15 minute default")
}

func TestTicketV1_TheDefaultLifetimeIsFifteenMinutes(t *testing.T) {
	assert.Equal(t, 15*time.Minute, DefaultTicketTTL)
	assert.Equal(t, DefaultTicketTTL, standardGate(t).TicketTTL())
}

func TestTicketV1_TheLifetimeIsConfigurable(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")), WithTicketTTL(2*time.Minute))
	assert.Equal(t, 2*time.Minute, g.TicketTTL())

	ra := proposedAction()
	d := approvalRun(t, g, ra)
	require.True(t, d.Allowed(), d.SkipReason())
	ticket, err := d.Ticket()
	require.NoError(t, err)
	assert.True(t, ticket.ExpiresAt.Time.Equal(fixedNow.Add(2*time.Minute)))
}

func TestTicketV1_ANonPositiveLifetimeFallsBackToTheDefault(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Minute} {
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")), WithTicketTTL(ttl))
		assert.Equal(t, DefaultTicketTTL, g.TicketTTL(), "ttl %v", ttl)
	}
}

func TestTicketV1_ATicketLongerThanTheConfiguredLifetimeIsRefused(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")), WithTicketTTL(2*time.Minute))
	ra := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(fixedNow.Add(10 * time.Minute))
	})

	problem := checkTicket(t, g, ra)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailTicket, problem.Guardrail)
}

func TestTicketV1_NoTicketIsIssuedWithoutAnApprover(t *testing.T) {
	for name, approvedBy := range map[string]string{"empty": "", "blank": "   "} {
		t.Run(name, func(t *testing.T) {
			ra := proposedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApprovedBy = approvedBy })

			d := approvalRun(t, standardGate(t), ra)

			assert.False(t, d.Allowed(), "an approval that names nobody is not an approval")
			stage, guardrail := d.RefusedBy()
			assert.Equal(t, StageGuardrails, stage)
			assert.Equal(t, GuardrailNoApprover, guardrail)
			_, err := d.Ticket()
			assert.Error(t, err, "a refused decision carries no ticket")
		})
	}
}

func TestTicketV1_AnAdvisoryPlanNeedsNoApproverBecauseItIssuesNoTicket(t *testing.T) {
	ra := proposedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApprovedBy = ""
		ra.Spec.Steps = []dorguv1.RemediationStep{{Order: 1, ID: "m", Type: dorguv1.StepTypeManual, Description: "look"}}
	})

	d := approvalRun(t, standardGate(t), ra)

	assert.True(t, d.Allowed(), d.SkipReason())
}

func TestTicketV1_ARecordedTicketFromTheIssuerPassesCheckTicket(t *testing.T) {
	assert.Nil(t, checkTicket(t, standardGate(t), ticketedAction()))
}

func TestTicketV1_RefusalsAtApplyAreEnumerated(t *testing.T) {
	g := standardGate(t)
	cases := map[string]struct {
		mutate    func(*dorguv1.RemediationAction)
		guardrail string
	}{
		"no specHash":   {func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.SpecHash = "" }, GuardrailTicketIncomplete},
		"no approvedBy": {func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.ApprovedBy = "" }, GuardrailTicketIncomplete},
		"no expiresAt":  {func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.ExpiresAt = metav1.Time{} }, GuardrailTicketIncomplete},
		"no issuedAt":   {func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.IssuedAt = nil }, GuardrailTicketIncomplete},
		"expired": {func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(fixedNow.Add(-time.Second))
		}, GuardrailTicketExpired},
		"expired exactly now": {func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(fixedNow)
		}, GuardrailTicketExpired},
		"spec edited after clearance": {func(ra *dorguv1.RemediationAction) {
			ra.Spec.Explanation = "someone edited the explanation"
		}, GuardrailSpecChanged},
		"patch in the spec edited after clearance": {func(ra *dorguv1.RemediationAction) {
			ra.Spec.Steps[0].Patch = memPatch("1Gi")
		}, GuardrailSpecChanged},
		"hash of another spec": {func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.SpecHash = "0000000000000000000000000000000000000000000000000000000000000000"
		}, GuardrailSpecChanged},
		"hash in upper case is not the hash": {func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.SpecHash = upper(ra.Status.ApplyTicket.SpecHash)
		}, GuardrailSpecChanged},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			problem := checkTicket(t, g, ticketedAction(tc.mutate))
			require.NotNil(t, problem, "the ticket must be refused")
			assert.Equal(t, tc.guardrail, problem.Guardrail, problem.Reason)
		})
	}
}

func upper(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'f' {
			out[i] = c - 32
		}
	}
	return string(out)
}

// A ticket that is both expired and for a spec that changed reports the spec: the
// integrity failure is the more serious of the two.
func TestTicketV1_ASpecChangeOutranksExpiry(t *testing.T) {
	ra := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Explanation = "edited"
		ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(fixedNow.Add(-time.Hour))
	})

	problem := checkTicket(t, standardGate(t), ra)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailSpecChanged, problem.Guardrail)
}

// The fields are checked before anything is read from the cluster: an incomplete
// ticket is refused without a single live read.
func TestTicketV1_AnIncompleteTicketIsRefusedBeforeTheClusterIsRead(t *testing.T) {
	g := newTestGate(t, fakeClient(t)) // no Deployment, no persona: any live read would say workload-gone
	ra := ticketedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.SpecHash = "" })

	problem := checkTicket(t, g, ra)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailTicketIncomplete, problem.Guardrail)
}

// v0 tickets (before specHash and approvedBy existed) that are still unapplied at
// upgrade are refused cleanly: nothing is applied, the reason names the ticket
// version, and the operator proposes again. They are not honoured until they expire.
func TestTicketV1_AV0TicketIsRefusedCleanlyEvenWhenUnexpired(t *testing.T) {
	ra := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.SpecHash = ""
		ra.Status.ApplyTicket.ApprovedBy = ""
	})

	problem := checkTicket(t, standardGate(t), ra)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailTicketIncomplete, problem.Guardrail)
	assert.Contains(t, problem.Reason, "predates")
	assert.NotContains(t, problem.Reason, "—", "no em dashes in user-facing text")
}

func TestTicketV1_NewGuardrailsAreInTheMetricCatalog(t *testing.T) {
	assert.Contains(t, allGuardrails(), GuardrailNoApprover)
	assert.Contains(t, allGuardrails(), GuardrailTicketIncomplete)
	assert.Contains(t, applyReasons(), GuardrailTicketIncomplete)
	assert.Contains(t, applyReasons(), GuardrailSpecChanged)
}
