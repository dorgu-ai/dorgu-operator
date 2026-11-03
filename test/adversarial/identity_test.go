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
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Identity scenarios. Each acts as a real authenticated user that envtest
// provisions, holding exactly the RBAC the scenario names, and asserts what
// admission does with the request. They close at CP2 with the
// ValidatingAdmissionPolicies; whoever lands those must install them into
// this envtest before the assertions can pass.

// verb sets granted on RemediationActions.
var (
	statusWriter = []rbacv1.PolicyRule{
		{APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationactions"}, Verbs: []string{"get", "list"}},
		{
			APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationactions/status"},
			Verbs: []string{"get", "patch", "update"},
		},
	}
	creator = []rbacv1.PolicyRule{
		{
			APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationactions"},
			Verbs: []string{"get", "list", "create"},
		},
	}
)

// ID-01: a ServiceAccount that holds remediationactions/status patch (a CI
// bot, an agent) sets Approved. Approval is a human act: denied at admission.
func TestID01ServiceAccountApproves(t *testing.T) {
	ns := newNamespace(t, "id-01")
	createIncident(t, ns)
	ra := createRA(t, newRA(ns, "proposal"), "Pending")

	bot := userClient(t, ns, "system:serviceaccount:"+ns+":ci-bot",
		[]string{"system:serviceaccounts", "system:serviceaccounts:" + ns}, statusWriter)
	err := approveAs(bot, ra, "ci-bot")

	Expect(t, "ID-01", err != nil, fmt.Sprintf("service account approval returned err=%v", err))
}

// ID-02: the identity that created the proposal approves it.
func TestID02ProposerApprovesOwnProposal(t *testing.T) {
	ns := newNamespace(t, "id-02")
	createIncident(t, ns)

	rules := append(append([]rbacv1.PolicyRule{}, creator...), statusWriter...)
	agent := userClient(t, ns, "agent-x", nil, rules)
	ra := newRA(ns, "self-approved")
	if err := agent.Create(context.Background(), ra); err != nil {
		t.Fatalf("agent creating its proposal: %v", err)
	}
	err := approveAs(agent, ra, "agent-x")

	Expect(t, "ID-02", err != nil, fmt.Sprintf("self-approval by the proposer returned err=%v", err))
}

// ID-03: a human approves but writes approvedBy as someone else, the way the
// CLI writes the literal "cli-user". The record must name the authenticated
// user, or the write must be refused.
func TestID03ApprovedByNotTheAuthenticatedUser(t *testing.T) {
	ns := newNamespace(t, "id-03")
	createIncident(t, ns)
	ra := createRA(t, newRA(ns, "proposal"), "Pending")

	alice := userClient(t, ns, "alice", nil, statusWriter)
	err := approveAs(alice, ra, "cli-user")

	recorded := ""
	if getErr := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(ra), ra); getErr == nil {
		recorded = ra.Status.ApprovedBy
	}
	blocked := err != nil || recorded == "alice"
	Expect(t, "ID-03", blocked, fmt.Sprintf("alice approved, err=%v, approvedBy recorded as %q", err, recorded))
}

// ID-04: an agent creates a RemediationAction directly. Only the operator
// creates RAs; an agent's proposal must enter through intake.
func TestID04AgentCreatesRemediationAction(t *testing.T) {
	ns := newNamespace(t, "id-04")
	createIncident(t, ns)

	agent := userClient(t, ns, "agent-y", nil, creator)
	err := agent.Create(context.Background(), newRA(ns, "agent-written"))

	Expect(t, "ID-04", err != nil, fmt.Sprintf("agent create returned err=%v", err))
}

// userClient provisions an authenticated user with the given groups, binds a
// Role carrying rules in ns, and returns a client acting as that user.
func userClient(t *testing.T, ns, name string, groups []string, rules []rbacv1.PolicyRule) client.Client {
	t.Helper()
	user, err := testEnv.AddUser(envtest.User{Name: name, Groups: groups}, restCfg)
	if err != nil {
		t.Fatalf("adding user %s: %v", name, err)
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "adversarial-", Namespace: ns},
		Rules:      rules,
	}
	mustCreate(t, role)
	mustCreate(t, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "adversarial-", Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: name}},
	})

	c, err := client.New(user.Config(), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building client for %s: %v", name, err)
	}
	return c
}

// approveAs patches status to Approved as c, the way the CLI does.
func approveAs(c client.Client, ra *dorguv1.RemediationAction, approvedBy string) error {
	ctx := context.Background()
	var fresh dorguv1.RemediationAction
	if err := c.Get(ctx, client.ObjectKeyFromObject(ra), &fresh); err != nil {
		return fmt.Errorf("getting %s: %w", ra.Name, err)
	}
	patch := client.MergeFrom(fresh.DeepCopy())
	now := metav1.Now()
	fresh.Status.Phase = phaseApproved
	fresh.Status.ApprovedBy = approvedBy
	fresh.Status.ApprovedAt = &now
	return c.Status().Patch(ctx, &fresh, patch)
}
