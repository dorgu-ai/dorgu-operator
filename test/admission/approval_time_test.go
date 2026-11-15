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

package admission

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// C2 review F-4: the operator reads when an approval was written from the API server's
// managedFields. This reads it from a real API server, where a person approves through the
// status subresource and the operator writes status around it, to be sure the parser agrees
// with what the server really stores (the fake client cannot say).
func TestApprovalWrittenAt_OnARealServer(t *testing.T) {
	ns := namespace(t, "approval-time")
	ra := pendingAction(t, ns, "agent-x")
	alice := newActor(t, ns, "alice", nil, approverRules)

	before := time.Now()
	require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, "alice"))
	after := time.Now()

	// The operator writes status around the approval (a condition), as it does for a ticket.
	var got dorguv1.RemediationAction
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKeyFromObject(ra), &got))
	got.Status.Conditions = append(got.Status.Conditions, metav1.Condition{
		Type: "Noted", Status: metav1.ConditionTrue, Reason: "Test", LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, env.Operator.Status().Update(ctxFor(t), &got))
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKeyFromObject(ra), &got))
	require.Equal(t, "Approved", got.Status.Phase)

	at, err := controller.ApprovalWrittenAt(&got)
	require.NoError(t, err, "managedFields: %+v", got.ManagedFields)
	// The API server's clock and this one are the same machine; allow a second for rounding
	// (managedFields times have one-second resolution).
	assert.WithinRange(t, at, before.Add(-time.Second), after.Add(time.Second),
		"the stamp is the approval write, not the operator's later write")
}

// A second approval by the same person after the operator voided the first is a newer write.
func TestApprovalWrittenAt_ANewApprovalIsANewStamp(t *testing.T) {
	ns := namespace(t, "approval-time2")
	ra := pendingAction(t, ns, "agent-x")
	alice := newActor(t, ns, "alice", nil, approverRules)
	require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, "alice"))
	var first dorguv1.RemediationAction
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKeyFromObject(ra), &first))
	firstAt, err := controller.ApprovalWrittenAt(&first)
	require.NoError(t, err)

	// The operator sends it back, as the controller does for an approval written in a guard
	// window: Pending, approval cleared. Admission lets the operator do exactly this.
	first.Status.Phase = "Pending"
	first.Status.ApprovedBy = ""
	first.Status.ApprovedAt = nil
	require.NoError(t, env.Operator.Status().Update(ctxFor(t), &first), "the operator may send an approval back")

	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, "alice"))
	var second dorguv1.RemediationAction
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKeyFromObject(ra), &second))
	secondAt, err := controller.ApprovalWrittenAt(&second)
	require.NoError(t, err)
	assert.True(t, secondAt.After(firstAt), "re-approval is a newer write: %s then %s", firstAt, secondAt)
}

// C2 re-review N-3, end to end: an approval written while the guard was down is voided
// because its stamp falls in the window. The person who wrote it cannot refresh the stamp
// after the window closed (the old policy let them rewrite the heal record's condition on an
// Approved action, which moved the managedFields time and took the approval out of the window).
func TestApprovalWrittenAt_APersonCannotRefreshTheStampOfAnApprovalInAWindow(t *testing.T) {
	ns := namespace(t, "approval-time3")
	ctx := ctxFor(t)
	ok := intake.Check{Name: "x", Reason: intake.ReasonAdmissionPoliciesMissing, Run: func(context.Context) error { return nil }}
	preflight := intake.NewPreflight(logr.Discard(), ok) // the startup window opens now

	ra := pendingAction(t, ns, "agent-x")
	alice := newActor(t, ns, "alice", nil, approverRules)
	require.NoError(t, admissionenv.Approve(ctx, alice.c, ra, "alice")) // written inside the window
	var written dorguv1.RemediationAction
	require.NoError(t, env.Admin.Get(ctx, client.ObjectKeyFromObject(ra), &written))
	stamp, err := controller.ApprovalWrittenAt(&written)
	require.NoError(t, err)

	preflight.Run(ctx) // the first good run closes the window after the approval
	require.True(t, preflight.NotReadyAt(stamp), "the approval is inside the window: it is voided")

	time.Sleep(intake.GuardClockMargin + 1500*time.Millisecond) // out of reach of the margin
	conditions := append(conditionMaps(written.Status.Conditions), map[string]any{
		"type": "WorkloadPatched", "status": "True", "reason": "CLIAppliedResourceChange", "message": "x",
		"lastTransitionTime": nowText(),
	})
	requireDenied(t, admissionenv.PatchStatus(ctx, alice.c, ra, map[string]any{"conditions": conditions}), "InvalidTransition")

	var after dorguv1.RemediationAction
	require.NoError(t, env.Admin.Get(ctx, client.ObjectKeyFromObject(ra), &after))
	stampAfter, err := controller.ApprovalWrittenAt(&after)
	require.NoError(t, err)
	assert.Equal(t, stamp, stampAfter, "the denied write did not move the stamp")
	assert.True(t, preflight.NotReadyAt(stampAfter), "so the approval is still voided")
}
