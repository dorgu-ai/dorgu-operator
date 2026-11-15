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

package lifecycle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// convert has the operator turn the proposal into its action and returns the action, which
// the operator made and a person has yet to decide.
func convert(t *testing.T, op *operator, prop *dorguv1alpha1.RemediationProposal) *dorguv1.RemediationAction {
	t.Helper()
	op.reconcileProposal(t, prop)
	var got dorguv1alpha1.RemediationProposal
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKeyFromObject(prop), &got))
	require.Equal(t, dorguv1alpha1.PhaseConverted, got.Status.Phase, "the proposal was converted, not rejected: %s", got.Status.Conditions)
	require.NotNil(t, got.Status.RemediationActionRef)

	var ra dorguv1.RemediationAction
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKey{Namespace: prop.Namespace, Name: got.Status.RemediationActionRef.Name}, &ra))
	require.Equal(t, "Pending", ra.Status.Phase)
	require.NotNil(t, ra.Spec.Provenance)
	require.Equal(t, prop.Spec.Proposer.Username, ra.Spec.Provenance.Proposer.Username, "the action records who proposed it")
	return &ra
}

func personaMemory(t *testing.T, ns string) string {
	t.Helper()
	var persona dorguv1.ApplicationPersona
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKey{Namespace: ns, Name: admissionenv.AppName}, &persona))
	require.NotNil(t, persona.Spec.Resources)
	return persona.Spec.Resources.Limits.Memory
}

func actionPhase(t *testing.T, ra *dorguv1.RemediationAction) dorguv1.RemediationAction {
	t.Helper()
	var got dorguv1.RemediationAction
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKeyFromObject(ra), &got))
	return got
}

// waitReady lets the guard's startup window and clock margin pass, so an approval written now
// is one the guard was ready for (see intake.GuardClockMargin).
func waitReady() { time.Sleep(intake.GuardClockMargin + time.Second) }

// The whole path, as three real identities: an agent proposes, the operator converts and
// later tickets and applies, and a person who is not the agent approves. Every operator write
// goes through the policies as written; the negative legs are the policies refusing the agent
// and the operator themselves.
func TestLifecycle_ProposalToApplyOnAPolicyEnforcedServer(t *testing.T) {
	ns, err := env.NewNamespace(ctxFor(t), "life")
	require.NoError(t, err)
	require.NoError(t, env.MakeWorkload(ctxFor(t), ns))
	op := newOperator(t)

	// The agent can propose, and, to prove it is the identity rule that stops it and not a
	// missing permission, it also holds every approver permission.
	agent := newUser(t, ns, "agent-"+uniq("a"), append(append([]rbacv1.PolicyRule{}, proposerRules...), approverRules...))
	prop := memoryProposal(ns, agent.name)
	require.NoError(t, agent.c.Create(ctxFor(t), prop), "an agent may propose as itself")

	ra := convert(t, op, prop)

	// Negative legs: the proposer cannot decide its own proposal, and neither can a
	// service account or the operator.
	err = admissionenv.Approve(ctxFor(t), agent.c, ra, agent.name)
	require.True(t, admissionenv.IsDorguDenial(err, "ApproverIsProposer"), "self-approval is denied by admission: %v", err)
	require.Equal(t, "Pending", actionPhase(t, ra).Status.Phase)

	err = admissionenv.Approve(ctxFor(t), env.Operator, ra, admissionenv.OperatorUsername)
	require.True(t, admissionenv.IsDorguDenial(err, "ServiceAccountCannotApprove"), "the operator never approves: %v", err)

	// A real person who did not propose approves.
	waitReady()
	alice := newUser(t, ns, "alice-"+uniq("p"), approverRules)
	require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))

	// The operator issues the ticket and applies it: both writes pass its own policies.
	op.reconcileAction(t, ra)
	got := actionPhase(t, ra)
	assert.Contains(t, []string{"Applying", "Verifying"}, got.Status.Phase, "ticketed and applied; conditions: %+v", got.Status.Conditions)
	require.NotNil(t, got.Status.ApplyTicket, "a ticket was issued")
	assert.Equal(t, alice.name, got.Status.ApplyTicket.ApprovedBy, "the ticket carries the person who approved")
	assert.Equal(t, "384Mi", personaMemory(t, ns), "the approved change was applied")
}

// F-4 end to end: an approval written while the approval gate's binding was gone is never
// honoured, even after the binding is back. The guard sees the missing binding at the moment
// of the ticket (not a minute later), and when it is ready again the approval goes back to
// Pending and a person approves it again.
func TestLifecycle_AnApprovalWrittenWhilePoliciesWereGoneIsSentBack(t *testing.T) {
	ns, err := env.NewNamespace(ctxFor(t), "life-gap")
	require.NoError(t, err)
	require.NoError(t, env.MakeWorkload(ctxFor(t), ns))
	op := newOperator(t)
	agent := newUser(t, ns, "agent-"+uniq("a"), append(append([]rbacv1.PolicyRule{}, proposerRules...), approverRules...))
	prop := memoryProposal(ns, agent.name)
	require.NoError(t, agent.c.Create(ctxFor(t), prop))
	ra := convert(t, op, prop)
	waitReady()

	// The approval gate's binding is deleted (an admin, or GitOps pruning).
	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKey{Name: "dorgu-approval-gate"}, &binding))
	saved := binding.DeepCopy()
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		saved.ResourceVersion, saved.UID = "", ""
		require.NoError(t, env.Admin.Create(ctxFor(t), saved))
		require.NoError(t, env.WaitEnforcing(ctxFor(t)))
	}
	t.Cleanup(restore)
	require.NoError(t, env.Admin.Delete(ctxFor(t), &binding))

	// With it gone, the agent approves its own proposal under a made-up name. Wait until the
	// API server has noticed the binding is gone (until then admission still refuses it).
	require.Eventually(t, func() bool {
		return admissionenv.Approve(ctxFor(t), agent.c, ra, "someone-else") == nil
	}, 30*time.Second, 200*time.Millisecond, "with the binding gone the forged approval is accepted")

	// The operator is asked to ticket it: it asks the cluster now, sees the missing binding,
	// and issues nothing.
	res := op.reconcileAction(t, ra)
	got := actionPhase(t, ra)
	assert.Equal(t, "Approved", got.Status.Phase, "left as it is while the policies are gone")
	assert.Nil(t, got.Status.ApplyTicket, "no ticket")
	assert.Positive(t, res.RequeueAfter)
	assert.Equal(t, "256Mi", personaMemory(t, ns), "nothing applied")

	// The binding comes back. The forged approval is still there, but it was written while
	// the guard was not ready: it goes back to Pending instead of being ticketed.
	restore()
	op.reconcileAction(t, ra)
	got = actionPhase(t, ra)
	assert.Equal(t, "Pending", got.Status.Phase, "sent back for a new approval; conditions: %+v", got.Status.Conditions)
	assert.Empty(t, got.Status.ApprovedBy)
	assert.Nil(t, got.Status.ApplyTicket)
	voided := false
	for _, c := range got.Status.Conditions {
		voided = voided || c.Type == dorguv1.ConditionApprovalVoided
	}
	assert.True(t, voided, "a condition says why")
	assert.Equal(t, "256Mi", personaMemory(t, ns))

	// A real person approves it again, once the guard has been ready for a while, and it goes through.
	waitReady()
	alice := newUser(t, ns, "alice-"+uniq("p"), approverRules)
	require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))
	op.reconcileAction(t, ra)
	got = actionPhase(t, ra)
	require.NotNil(t, got.Status.ApplyTicket, "the second approval is honoured; conditions: %+v", got.Status.Conditions)
	assert.Equal(t, alice.name, got.Status.ApplyTicket.ApprovedBy)
	assert.Equal(t, "384Mi", personaMemory(t, ns))
}
