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

package adversarial

import (
	"context"
	"fmt"
	"sync"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// Identity scenarios (ID-01 to ID-04), closed by the ValidatingAdmissionPolicies in P2.
//
// They run against their own API server, started with the policies the chart ships
// installed (config/admission/policies.yaml, which a chart test holds equal to the
// chart's template). The OP scenarios do not: they model a cluster where admission was
// bypassed (an action that predates the policies, a policy removed) and prove the
// operator's own checks still hold, which is defence in depth, not the enforcement point.
//
// Each acts as a real authenticated user that envtest provisions, holding exactly the RBAC
// the scenario names, and writes what the CLI writes (a JSON merge patch of the status
// subresource).

var (
	identityEnvOnce sync.Once
	identityEnv     *admissionenv.Env
	identityEnvErr  error
)

// admissionEnvFor starts the policy-enforcing API server the first time an identity
// scenario needs it, and stops it when the suite ends (stopIdentityEnv).
func admissionEnvFor(t *testing.T) *admissionenv.Env {
	t.Helper()
	identityEnvOnce.Do(func() { identityEnv, identityEnvErr = admissionenv.Start() })
	if identityEnvErr != nil {
		t.Fatalf("starting the API server with the admission policies: %v", identityEnvErr)
	}
	return identityEnv
}

func stopIdentityEnv() {
	if identityEnv != nil {
		_ = identityEnv.Stop()
	}
}

const phasePending = "Pending"

// Rules a scenario grants.
var (
	// statusWriter is what the old fixtures gave an approver: the status patch and nothing
	// else. The approve verb is the part the policy adds.
	statusWriter = []rbacv1.PolicyRule{
		{APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationactions"}, Verbs: []string{"get", "list"}},
		{
			APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationactions/status"},
			Verbs: []string{"get", "patch", "update"},
		},
	}
	// approverRules is the dorgu-approver role: the status patch and the approve and reject
	// verbs. The scenarios that attack approval grant it, so the attack is stopped by the
	// identity rules and not by a missing permission.
	approverRules = append(append([]rbacv1.PolicyRule{}, statusWriter...), rbacv1.PolicyRule{
		APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationactions"},
		Verbs: []string{"approve", "reject"},
	})
	creator = []rbacv1.PolicyRule{
		{
			APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationactions"},
			Verbs: []string{"get", "list", "create"},
		},
	}
)

// idUser provisions an authenticated user with the given groups and rules in ns.
func idUser(
	t *testing.T, e *admissionenv.Env, ns, name string, groups []string, rules []rbacv1.PolicyRule,
) client.Client {
	t.Helper()
	c, err := e.UserClient(name, groups)
	if err != nil {
		t.Fatalf("adding user %s: %v", name, err)
	}
	if len(rules) > 0 {
		if err := e.GrantRules(context.Background(), ns, name, rules); err != nil {
			t.Fatalf("granting %s its rules: %v", name, err)
		}
	}
	return c
}

// pendingFor creates a Pending action the way the operator does after it converts a
// proposal: created and given its status by the operator, naming proposer in provenance
// ("" for an action that predates provenance).
func pendingFor(t *testing.T, e *admissionenv.Env, ns, name, proposer string) *dorguv1.RemediationAction {
	t.Helper()
	ra := admissionenv.NewAction(ns, name, proposer)
	if err := e.MakePending(context.Background(), ra); err != nil {
		t.Fatalf("making the %s action Pending as the operator: %v", name, err)
	}
	return ra
}

// recordedDecision is who the API server says decided ra, and its phase.
func recordedDecision(t *testing.T, e *admissionenv.Env, ra *dorguv1.RemediationAction) (phase, by string) {
	t.Helper()
	var got dorguv1.RemediationAction
	if err := e.Admin.Get(context.Background(), client.ObjectKeyFromObject(ra), &got); err != nil {
		t.Fatalf("re-reading %s: %v", ra.Name, err)
	}
	return got.Status.Phase, got.Status.ApprovedBy
}

// ID-01: a ServiceAccount that holds remediationactions/status patch (a CI bot, an agent)
// sets Approved. Approval is a human act: denied at admission. The bot is given the approve
// verb as well, so only its being a ServiceAccount stops it.
func TestID01ServiceAccountApproves(t *testing.T) {
	e := admissionEnvFor(t)
	ns, _ := e.NewNamespace(context.Background(), "id-01")
	ra := pendingFor(t, e, ns, "proposal", "someone-else")

	botName := "system:serviceaccount:" + ns + ":ci-bot"
	bot := idUser(t, e, ns, botName,
		[]string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"}, approverRules)
	err := admissionenv.Approve(context.Background(), bot, ra, botName)

	phase, by := recordedDecision(t, e, ra)
	blocked := admissionenv.IsDorguDenial(err, "ServiceAccountCannotApprove") && phase == phasePending && by == ""
	Expect(t, "ID-01", blocked,
		fmt.Sprintf("service account approval returned err=%v, phase %q, approvedBy %q", err, phase, by))
}

// ID-02: the identity that proposed the remediation approves it. The agent is the recorded
// proposer: it wrote the proposal (as itself, which it may), the operator converted it into
// an action that names the agent in its provenance, and the agent, holding every permission
// an approver has, approves its own proposal. A person who did not propose it may.
func TestID02ProposerApprovesOwnProposal(t *testing.T) {
	e := admissionEnvFor(t)
	ns, _ := e.NewNamespace(context.Background(), "id-02")

	rules := append(append([]rbacv1.PolicyRule{}, approverRules...), rbacv1.PolicyRule{
		APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationproposals"},
		Verbs: []string{"create", "get", "list"},
	})
	agent := idUser(t, e, ns, "agent-x", nil, rules)
	if err := agent.Create(context.Background(), admissionenv.NewProposal(ns, "self-approved", "agent-x")); err != nil {
		t.Fatalf("agent proposing as itself: %v", err)
	}
	ra := pendingFor(t, e, ns, "self-approved", "agent-x")

	err := admissionenv.Approve(context.Background(), agent, ra, "agent-x")

	phase, by := recordedDecision(t, e, ra)
	blocked := admissionenv.IsDorguDenial(err, "ApproverIsProposer") && phase == phasePending && by == ""
	Expect(t, "ID-02", blocked,
		fmt.Sprintf("self-approval by the proposer returned err=%v, phase %q, approvedBy %q", err, phase, by))

	// The control: someone else approves the same action, so the denial is about the
	// proposer and not about the action or the verb.
	other := idUser(t, e, ns, "alice", nil, approverRules)
	if err := admissionenv.Approve(context.Background(), other, ra, "alice"); err != nil {
		t.Errorf("a person who did not propose it must be able to approve it: %v", err)
	}
}

// ID-03: a human approves but writes approvedBy as someone else, the way the CLI used to
// write the literal "cli-user". The record must name the authenticated user, or the write
// must be refused. (The policy refuses; it does not rewrite.)
func TestID03ApprovedByNotTheAuthenticatedUser(t *testing.T) {
	e := admissionEnvFor(t)
	ns, _ := e.NewNamespace(context.Background(), "id-03")
	ra := pendingFor(t, e, ns, "proposal", "someone-else")

	alice := idUser(t, e, ns, "alice", nil, approverRules)
	err := admissionenv.Approve(context.Background(), alice, ra, "cli-user")

	phase, recorded := recordedDecision(t, e, ra)
	blocked := (err != nil && admissionenv.IsDorguDenial(err, "ApprovedByMismatch")) || recorded == "alice"
	Expect(t, "ID-03", blocked && recorded != "cli-user",
		fmt.Sprintf("alice approved, err=%v, phase %q, approvedBy recorded as %q", err, phase, recorded))
}

// ID-04: an agent creates a RemediationAction directly. Only the operator creates RAs; an
// agent's proposal must enter through intake.
func TestID04AgentCreatesRemediationAction(t *testing.T) {
	e := admissionEnvFor(t)
	ns, _ := e.NewNamespace(context.Background(), "id-04")

	agent := idUser(t, e, ns, "agent-y", nil, creator)
	err := agent.Create(context.Background(), admissionenv.NewAction(ns, "agent-written", "agent-y"))

	Expect(t, "ID-04", admissionenv.IsDorguDenial(err, "OperatorOnly"),
		fmt.Sprintf("agent create returned err=%v", err))
}
