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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/controller"
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
