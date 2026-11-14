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

package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// Ticket v1 end to end through the phase machine (P2-D9, task 2.6): what the
// controller writes on approval, and how each refusal at apply ends the action.

func TestTicketV1_ApprovalWritesTheSpecHashAndTheApprover(t *testing.T) {
	ra := lcRA("Approved")
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	want, err := ra.Spec.Hash()
	if err != nil {
		t.Fatal(err)
	}

	_, got := lcReconcile(t, rc, ra)

	ticket := got.Status.ApplyTicket
	if ticket == nil {
		t.Fatalf("no ticket was issued: phase %q", got.Status.Phase)
	}
	if ticket.SpecHash != want {
		t.Errorf("ticket.specHash = %q, want %q", ticket.SpecHash, want)
	}
	if ticket.ApprovedBy != lcApprover {
		t.Errorf("ticket.approvedBy = %q, want the status.approvedBy at approval", ticket.ApprovedBy)
	}
	if ticket.IssuedAt == nil || !ticket.ExpiresAt.Time.Equal(ticket.IssuedAt.Add(15*time.Minute)) {
		t.Errorf("ticket lifetime = %v to %v, want 15 minutes", ticket.IssuedAt, ticket.ExpiresAt)
	}
}

func TestTicketV1_ConfiguredLifetimeIsUsed(t *testing.T) {
	ra := lcRA("Approved")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rc.Gate = gate.New(c, logr.Discard(), gate.WithTicketTTL(3*time.Minute))
	rc.Executor = remediation.NewExecutor(c, rc.Gate, logr.Discard())

	_, got := lcReconcile(t, rc, ra)

	ticket := got.Status.ApplyTicket
	if ticket == nil || ticket.IssuedAt == nil || !ticket.ExpiresAt.Time.Equal(ticket.IssuedAt.Add(3*time.Minute)) {
		t.Fatalf("ticket = %+v, want a 3 minute lifetime", ticket)
	}
}

func TestTicketV1_NoTicketWithoutAnApprover(t *testing.T) {
	ra := lcRA("Approved", func(ra *dorguv1.RemediationAction) { ra.Status.ApprovedBy = "" })
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.ApplyTicket != nil {
		t.Fatalf("a ticket was issued for an approval that names nobody: %+v", got.Status.ApplyTicket)
	}
	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	applied := lcCondition(got, ConditionApplied)
	if applied == nil || applied.Reason != dorguv1.ReasonGateRefusedAtApproval {
		t.Errorf("Applied = %+v, want GateRefusedAtApproval", applied)
	}
	refused := lcCondition(got, dorguv1.ConditionGateRefused)
	if refused == nil || refused.Reason != "NoApprover" {
		t.Errorf("GateRefused = %+v, want reason NoApprover", refused)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: nothing may be applied", mem)
	}
}

func TestTicketV1_AnExpiredTicketEndsExpiredWithTicketExpired(t *testing.T) {
	ra := lcRA("Ticketed", func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(time.Now().Add(-time.Second))
	})
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseExpired {
		t.Fatalf("phase = %q, want Expired", got.Status.Phase)
	}
	expired := lcCondition(got, ConditionExpired)
	if expired == nil || expired.Status != metav1.ConditionTrue || expired.Reason != dorguv1.ReasonTicketExpired {
		t.Fatalf("Expired condition = %+v, want True with reason TicketExpired", expired)
	}
	if got.Status.AppliedAt != nil {
		t.Error("an expired ticket was applied")
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: an expired ticket must not apply", mem)
	}
	if strings.Contains(expired.Message, "—") {
		t.Errorf("message has an em dash: %q", expired.Message)
	}
	if !strings.Contains(expired.Message, "nothing was applied") && !strings.Contains(expired.Message, "Nothing was applied") {
		t.Errorf("message must say nothing was applied: %q", expired.Message)
	}
}

// An expired ticket is not a failed remediation: no cooldown holds the persona.
func TestTicketV1_AnExpiredTicketStartsNoCooldown(t *testing.T) {
	ra := lcRA("Ticketed", func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(time.Now().Add(-time.Second))
	})
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseExpired {
		t.Fatalf("phase = %q, want Expired", got.Status.Phase)
	}
	if outcome := lcIncidentOutcome(t, c); outcome == IncidentOutcomeFailed {
		t.Errorf("incident outcome = %q: an expired ticket is not a failure", outcome)
	}
}

func TestTicketV1_ASpecChangedUnderTheTicketEndsFailedWithSpecChanged(t *testing.T) {
	cases := map[string]func(*dorguv1.RemediationAction){
		"explanation edited": func(ra *dorguv1.RemediationAction) { ra.Spec.Explanation = "edited after approval" },
		"step patch edited": func(ra *dorguv1.RemediationAction) {
			ra.Spec.Steps[0].Patch.Raw = []byte(`{"spec":{"resources":{"limits":{"memory":"4Gi"}}}}`)
		},
		"step added": func(ra *dorguv1.RemediationAction) {
			ra.Spec.Steps = append(ra.Spec.Steps, dorguv1.RemediationStep{Order: 2, ID: "extra", Type: dorguv1.StepTypeManual, Description: "x"})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ra := lcRA("Ticketed", mutate)
			rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

			_, got := lcReconcile(t, rc, ra)

			if got.Status.Phase != RemediationPhaseFailed {
				t.Fatalf("phase = %q, want Failed", got.Status.Phase)
			}
			applied := lcCondition(got, ConditionApplied)
			if applied == nil || applied.Status != metav1.ConditionFalse || applied.Reason != dorguv1.ReasonSpecChanged {
				t.Fatalf("Applied = %+v, want False with reason SpecChanged", applied)
			}
			if got.Status.AppliedAt != nil {
				t.Error("a ticket for another spec was applied")
			}
			if mem := lcPersonaMemory(t, c); mem != lcBaseline {
				t.Errorf("persona memory = %s: a changed spec must not apply", mem)
			}
		})
	}
}

// A spec edited between the proposal and the approval never reaches a ticket: the
// existing generation check refuses it at approval, and the ticket path is not used.
func TestTicketV1_ASpecEditedBeforeApprovalIsRefusedAtApproval(t *testing.T) {
	ra := lcRA("Approved", func(ra *dorguv1.RemediationAction) { ra.Generation = 3 })
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.ApplyTicket != nil {
		t.Fatalf("a ticket was issued for an edited spec: %+v", got.Status.ApplyTicket)
	}
}

// v0 tickets still unapplied at upgrade are refused, not honoured: the action ends
// Failed/PreconditionRejected, nothing is applied and no cooldown starts.
func TestTicketV1_AV0TicketUnappliedAtUpgradeIsRefusedCleanly(t *testing.T) {
	ra := lcRA("Ticketed", func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.SpecHash = ""
		ra.Status.ApplyTicket.ApprovedBy = ""
	})
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	applied := lcCondition(got, ConditionApplied)
	if applied == nil || applied.Reason != dorguv1.ReasonPreconditionRejected || !strings.Contains(applied.Message, "nothing was applied") {
		t.Fatalf("Applied = %+v, want PreconditionRejected, nothing applied", applied)
	}
	refused := lcCondition(got, dorguv1.ConditionGateRefused)
	if refused == nil || refused.Reason != "TicketIncomplete" {
		t.Errorf("GateRefused = %+v, want reason TicketIncomplete", refused)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: a v0 ticket must not apply", mem)
	}
}

// A v0 ticket whose change was already applied (Applying and later) is not touched
// by the upgrade: verification, rollback and incident accounting carry on.
func TestTicketV1_AV0TicketAlreadyAppliedCarriesOn(t *testing.T) {
	ra := lcRA("Applying", func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.SpecHash = ""
		ra.Status.ApplyTicket.ApprovedBy = ""
		ra.Status.ApprovedBy = ""
	})
	rc, _ := lcController(t, []client.Object{lcPersonaObj("512Mi"), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseVerifying {
		t.Fatalf("phase = %q, want Verifying: an applied v0 ticket must not be disturbed", got.Status.Phase)
	}
}
