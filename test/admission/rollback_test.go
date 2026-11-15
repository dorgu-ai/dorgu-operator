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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// C6 (P2-D10): asking for a rollback is the approver's authority, never an agent's.
// A rollback proposal ends as an action the operator proposed itself, which one person
// may approve alone, so the policy that stands between an agent and that path is
// admission: only a person holding the approve verb may create one, as themselves.
// The denials reuse the approval gate's reasons (no new token).

// rollbackProposal is what the CLI writes: source rollback, no steps.
func rollbackProposal(ns, name, username string) *dorguv1alpha1.RemediationProposal {
	p := admissionenv.NewProposal(ns, name, username)
	p.Spec.Source = dorguv1.ProposalSourceRollback
	p.Spec.Steps = nil
	p.Spec.RollbackOf = &dorguv1alpha1.ActionRef{Name: "ra-applied"}
	return p
}

var rollbackAskerRules = append(append([]rbacv1.PolicyRule{}, approverRules...),
	rule([]string{"remediationproposals"}, "create", "get", "list"))

func TestRollbackProposal_APersonWithTheApproveVerbMayAsk(t *testing.T) {
	ns := namespace(t, "rb-ok")
	alice := newActor(t, ns, "", nil, rollbackAskerRules)

	require.NoError(t, alice.c.Create(ctxFor(t), rollbackProposal(ns, uniq("rb"), alice.name)))
}

func TestRollbackProposal_AServiceAccountCannotAsk(t *testing.T) {
	ns := namespace(t, "rb-sa")
	// Holding every right a person with the approver role holds, plus create on proposals:
	// only being a service account stops it.
	bot := newServiceAccount(t, ns, rollbackAskerRules)

	err := bot.c.Create(ctxFor(t), rollbackProposal(ns, uniq("rb"), bot.name))

	requireDenied(t, err, "ServiceAccountCannotApprove")
	assert.Contains(t, err.Error(), "rollback", "the message says it is about a rollback, not an approval")

	// The control: the same service account proposes an ordinary change as itself, so the
	// denial is about the rollback and not about the proposal or the role.
	require.NoError(t, bot.c.Create(ctxFor(t), admissionenv.NewProposal(ns, uniq("p"), bot.name)))
}

func TestRollbackProposal_TheOperatorCannotAskEither(t *testing.T) {
	ns := namespace(t, "rb-op")
	err := env.Operator.Create(ctxFor(t), rollbackProposal(ns, uniq("rb"), admissionenv.OperatorUsername))

	requireDenied(t, err, "ServiceAccountCannotApprove")
}

func TestRollbackProposal_AnAgentRoleCannotAsk(t *testing.T) {
	ns := namespace(t, "rb-agent")
	// The shipped agent role: a person (not a service account) bound to dorgu-agent-proposer
	// still lacks the approve verb.
	person := newActor(t, ns, "", nil, proposerRules)

	err := person.c.Create(ctxFor(t), rollbackProposal(ns, uniq("rb"), person.name))

	requireDenied(t, err, "MissingApproveVerb")
	require.NoError(t, person.c.Create(ctxFor(t), admissionenv.NewProposal(ns, uniq("p"), person.name)),
		"an ordinary proposal from the same person is accepted")
}

func TestRollbackProposal_TheApproveVerbInAnotherNamespaceDoesNotCount(t *testing.T) {
	ns, other := namespace(t, "rb-ns"), namespace(t, "rb-ns-other")
	alice := newActor(t, ns, "", nil, proposerRules)
	require.NoError(t, env.GrantRules(ctxFor(t), other, alice.name, approverRules))

	requireDenied(t, alice.c.Create(ctxFor(t), rollbackProposal(ns, uniq("rb"), alice.name)), "MissingApproveVerb")
}

func TestRollbackProposal_ARequesterCannotBeSomeoneElse(t *testing.T) {
	ns := namespace(t, "rb-forged")
	mallory := newActor(t, ns, "", nil, rollbackAskerRules)

	// The proposer is the requestedBy of the action the operator makes. Naming another
	// person would put their name on mallory's request.
	err := mallory.c.Create(ctxFor(t), rollbackProposal(ns, uniq("rb"), "victim@example.com"))

	requireDenied(t, err, "ProposerMismatch")
}

func TestRollbackProposal_TheCRDRefusesWhatARollbackMustNotCarry(t *testing.T) {
	ns := namespace(t, "rb-crd")
	alice := newActor(t, ns, "", nil, rollbackAskerRules)

	withSteps := rollbackProposal(ns, uniq("rb"), alice.name)
	withSteps.Spec.Steps = admissionenv.NewProposal(ns, "x", alice.name).Spec.Steps
	err := alice.c.Create(ctxFor(t), withSteps)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "steps on a rollback are refused by the CRD: %v", err)
	assert.Contains(t, err.Error(), "carries no steps")

	claim := admissionenv.NewProposal(ns, uniq("p"), alice.name)
	claim.Spec.Source = dorguv1.ProposalSourceRollback
	err = alice.c.Create(ctxFor(t), claim)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "the reserved source needs rollbackOf: %v", err)
}

func TestRollbackProposal_ItsSpecIsImmutableLikeAnyOther(t *testing.T) {
	ns := namespace(t, "rb-imm")
	rules := append(append([]rbacv1.PolicyRule{}, rollbackAskerRules...),
		rule([]string{"remediationproposals"}, "update", "patch"))
	alice := newActor(t, ns, "", nil, rules)
	prop := rollbackProposal(ns, uniq("rb"), alice.name)
	require.NoError(t, alice.c.Create(ctxFor(t), prop))

	fresh := &dorguv1alpha1.RemediationProposal{}
	require.NoError(t, alice.c.Get(ctxFor(t), client.ObjectKeyFromObject(prop), fresh))
	fresh.Spec.RollbackOf = &dorguv1alpha1.ActionRef{Name: "another-action"}

	require.Error(t, alice.c.Update(ctxFor(t), fresh))
}
