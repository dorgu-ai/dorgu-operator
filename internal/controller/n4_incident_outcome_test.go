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
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// N-4 of the C3 re-review: an action refused before anything was applied is not a
// failed heal. Nothing reached the cluster, so the incident must not be booked with
// the outcome "failed", which feeds the failed-heal counts, the planner's history of
// what has been tried and every dashboard. The existing handling of "not applied" is
// that of every Expired path (a missed deadline, a ticket that ran out, a proposal
// that predates the gate): the incident is left without an outcome, still open, and
// the operator proposes again if the problem persists. The phase stays Failed with
// its own reason, because for these refusals the reason is how a client tells them
// apart.

func TestN4_ARefusalBeforeAnythingWasAppliedBooksNoFailedOutcome(t *testing.T) {
	cases := map[string]struct {
		mutate func(*dorguv1.RemediationAction)
		reason string
	}{
		"the spec changed under the ticket": {
			mutate: func(ra *dorguv1.RemediationAction) { ra.Spec.Explanation = "edited after approval" },
			reason: dorguv1.ReasonSpecChanged,
		},
		"a v0 ticket without specHash and approvedBy": {
			mutate: func(ra *dorguv1.RemediationAction) {
				ra.Status.ApplyTicket.SpecHash = ""
				ra.Status.ApplyTicket.ApprovedBy = ""
			},
			reason: dorguv1.ReasonPreconditionRejected,
		},
		"a ticket issued in the future": {
			mutate: func(ra *dorguv1.RemediationAction) {
				future := metav1.NewTime(time.Now().Add(time.Hour))
				ra.Status.ApplyTicket.IssuedAt = &future
				ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(future.Add(5 * time.Minute))
			},
			reason: dorguv1.ReasonPreconditionRejected,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ra := lcRA("Ticketed", tc.mutate)
			rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

			_, got := lcReconcile(t, rc, ra)

			if got.Status.Phase != RemediationPhaseFailed {
				t.Fatalf("phase = %q, want Failed", got.Status.Phase)
			}
			if applied := lcCondition(got, ConditionApplied); applied == nil || applied.Reason != tc.reason {
				t.Fatalf("Applied = %+v, want reason %s", applied, tc.reason)
			}
			if outcome := lcIncidentOutcome(t, c); outcome != "" {
				t.Errorf("incident outcome = %q: nothing was applied, so no outcome (least of all %q) is booked", outcome, IncidentOutcomeFailed)
			}
		})
	}
}

func TestN4_AnExpiredTicketBooksNoOutcomeEither(t *testing.T) {
	ra := lcRA("Ticketed", func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(time.Now().Add(-time.Second))
	})
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	lcReconcile(t, rc, ra)

	if outcome := lcIncidentOutcome(t, c); outcome != "" {
		t.Errorf("incident outcome = %q, want none", outcome)
	}
}

func TestN4_AGateRefusalAtApprovalBooksNoFailedOutcome(t *testing.T) {
	ra := lcRA("Approved", func(ra *dorguv1.RemediationAction) { ra.Status.ApprovedBy = "" })
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if outcome := lcIncidentOutcome(t, c); outcome != "" {
		t.Errorf("incident outcome = %q, want none: the gate refused, nothing was applied", outcome)
	}
}

func TestN4_ATicketTheAPIServerRefusesToStoreBooksNoFailedOutcome(t *testing.T) {
	ra := lcRA("Approved")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rc.Client = interceptor.NewClient(c, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if got, ok := obj.(*dorguv1.RemediationAction); ok && got.Status.ApplyTicket != nil && got.Status.Phase == RemediationPhaseTicketed {
				return apierrors.NewInvalid(dorguv1.GroupVersion.WithKind("RemediationAction").GroupKind(), got.Name, field.ErrorList{
					field.TooLong(field.NewPath("status", "applyTicket", "approvedBy"), "x", 253),
				})
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if outcome := lcIncidentOutcome(t, c); outcome != "" {
		t.Errorf("incident outcome = %q, want none: the ticket was never stored, nothing was applied", outcome)
	}
}

// The other direction: a failure after the change reached the cluster is a failed
// heal, and is still booked as one.
func TestN4_AFailureAfterTheChangeWasAppliedStillBooksFailed(t *testing.T) {
	ra := lcRA("Applying", func(ra *dorguv1.RemediationAction) { ra.Status.AppliedAt = nil })
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})

	_, got := lcReconcile(t, rc, ra)

	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if outcome := lcIncidentOutcome(t, c); outcome != IncidentOutcomeFailed {
		t.Errorf("incident outcome = %q, want %q: this failure is not a pre-apply refusal", outcome, IncidentOutcomeFailed)
	}
}

func TestN4_AFailureOnceAppliedIsFailedEvenWithAPreApplyReason(t *testing.T) {
	// A Failed action that has an AppliedAt did touch the cluster, whatever reason
	// it carries.
	ra := lcRA("Applying")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	var stored dorguv1.RemediationAction
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ra), &stored); err != nil {
		t.Fatal(err)
	}

	if err := rc.transitionToFailedWithReason(context.Background(), &stored, dorguv1.ReasonPreconditionRejected, "x"); err != nil {
		t.Fatal(err)
	}

	if outcome := lcIncidentOutcome(t, c); outcome != IncidentOutcomeFailed {
		t.Errorf("incident outcome = %q, want %q: the action was applied", outcome, IncidentOutcomeFailed)
	}
}
