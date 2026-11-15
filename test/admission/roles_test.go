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
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// The chart's four roles (task 2.4), installed from the chart's own render and exercised as
// real users against the API server and the policies together.

var installRoles sync.Once
var rolesErr error

func requireRoles(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not on PATH in CI: the role tests cannot run")
		}
		t.Skip("helm not installed on PATH; skipping the chart role tests")
	}
	installRoles.Do(func() {
		root, err := admissionenv.ModuleRoot()
		if err != nil {
			rolesErr = err
			return
		}
		out, err := exec.Command("helm", "template", "dorgu", root+"/charts/dorgu-operator",
			"--show-only", "templates/rbac-roles.yaml").CombinedOutput()
		if err != nil {
			rolesErr = err
			return
		}
		for doc := range strings.SplitSeq(string(out), "\n---") {
			var role rbacv1.ClusterRole
			if err := yaml.Unmarshal([]byte(doc), &role); err != nil || role.Kind != "ClusterRole" {
				continue
			}
			role.ResourceVersion = ""
			if err := env.Admin.Create(context.Background(), &role); err != nil && !apierrors.IsAlreadyExists(err) {
				rolesErr = err
				return
			}
		}
	})
	require.NoError(t, rolesErr)
}

// bound is a user holding the named ClusterRoles in ns.
func bound(t *testing.T, ns string, clusterRoles ...string) actor {
	t.Helper()
	requireRoles(t)
	a := newActor(t, ns, "", nil, nil)
	for _, role := range clusterRoles {
		require.NoError(t, env.Admin.Create(ctxFor(t), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "role-", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: a.name}},
		}))
	}
	return a
}

func TestRoles_AnAgentCanProposeAndReadAndNothingElse(t *testing.T) {
	ns := namespace(t, "agent")
	requireRoles(t)
	// A service account username, as a real agent would be.
	botName := "system:serviceaccount:" + ns + ":" + uniq("agent")
	bot := actor{name: botName}
	c, err := env.UserClient(botName, []string{"system:serviceaccounts", "system:authenticated"})
	require.NoError(t, err)
	bot.c = c
	for _, role := range []string{"dorgu-agent-proposer", "dorgu-agent-reader"} {
		require.NoError(t, env.Admin.Create(ctxFor(t), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "role-", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: botName}},
		}))
	}

	prop := admissionenv.NewProposal(ns, uniq("p"), bot.name)
	require.NoError(t, bot.c.Create(ctxFor(t), prop), "it proposes as itself")

	got := &dorguv1alpha1.RemediationProposal{}
	require.NoError(t, bot.c.Get(ctxFor(t), client.ObjectKeyFromObject(prop), got),
		"and reads it back (the rejection reason is in status)")
	var list dorguv1alpha1.RemediationProposalList
	require.NoError(t, bot.c.List(ctxFor(t), &list, client.InNamespace(ns)))

	forbidden := func(what string, err error) {
		t.Helper()
		require.Error(t, err, what)
		assert.True(t, apierrors.IsForbidden(err), "%s: want Forbidden, got %v", what, err)
		assert.NotContains(t, err.Error(), "dorgu: ", "%s: refused by RBAC, before any policy", what)
	}
	forbidden("delete its proposal", bot.c.Delete(ctxFor(t), prop))
	got.Spec.Rationale = "changed"
	forbidden("update its proposal", bot.c.Update(ctxFor(t), got))
	forbidden("write proposal status", bot.c.Status().Update(ctxFor(t), got))

	ra := pendingAction(t, ns, "someone-else")
	forbidden("create an action", bot.c.Create(ctxFor(t), admissionenv.NewAction(ns, uniq("ra"), bot.name)))
	forbidden("approve", admissionenv.Approve(ctxFor(t), bot.c, ra, bot.name))
	forbidden("reject", admissionenv.Reject(ctxFor(t), bot.c, ra, bot.name))
	forbidden("patch action status", setPhase(bot, ra, "Completed"))

	var actions dorguv1.RemediationActionList
	require.NoError(t, bot.c.List(ctxFor(t), &actions, client.InNamespace(ns)), "it can read actions")

	forbidden("write an incident", bot.c.Create(ctxFor(t), &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: uniq("im"), Namespace: ns}, Spec: incidentSpec(ns)}))
	forbidden("read a secret", bot.c.Get(ctxFor(t), client.ObjectKey{Namespace: ns, Name: "any"}, &corev1.Secret{}))
	forbidden("create events", bot.c.Create(ctxFor(t), &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e-", Namespace: ns},
	}))
}

func TestRoles_TheProposerRoleAloneCannotEvenRead(t *testing.T) {
	ns := namespace(t, "proposer")
	only := bound(t, ns, "dorgu-agent-proposer")
	require.NoError(t, only.c.Create(ctxFor(t), admissionenv.NewProposal(ns, uniq("p"), only.name)))
	var actions dorguv1.RemediationActionList
	err := only.c.List(ctxFor(t), &actions, client.InNamespace(ns))
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err))
}

func TestRoles_AnApproverDecidesWhatItDidNotPropose(t *testing.T) {
	ns := namespace(t, "approver")
	alice := bound(t, ns, "dorgu-approver")
	bob := bound(t, ns, "dorgu-approver")

	ra := pendingAction(t, ns, "an-agent")
	got := &dorguv1.RemediationAction{}
	require.NoError(t, alice.c.Get(ctxFor(t), client.ObjectKeyFromObject(ra), got))
	require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))

	rejected := pendingAction(t, ns, "an-agent")
	require.NoError(t, admissionenv.Reject(ctxFor(t), bob.c, rejected, bob.name))

	// The approver of an action it proposed itself: the policy, not the role, says no.
	own := pendingAction(t, ns, alice.name)
	requireDenied(t, admissionenv.Approve(ctxFor(t), alice.c, own, alice.name), "ApproverIsProposer")

	// It records a decline as an Event, and creates nothing else.
	require.NoError(t, alice.c.Create(ctxFor(t), &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "decline.", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "dorgu.io/v1", Kind: "RemediationAction", Name: ra.Name, Namespace: ns,
		},
		Reason: "DorguCLIDeclined", Message: "declined", Type: "Warning",
		Source: corev1.EventSource{Component: "dorgu-cli"},
	}))
	// It may write a proposal as itself: that is how `dorgu remediation rollback` asks for a
	// rollback (C6). It never writes an action, and cannot decide what it proposed.
	require.NoError(t, alice.c.Create(ctxFor(t), admissionenv.NewProposal(ns, uniq("p"), alice.name)))
	forbiddenWrite := alice.c.Create(ctxFor(t), admissionenv.NewAction(ns, uniq("ra"), ""))
	assert.True(t, apierrors.IsForbidden(forbiddenWrite))
}

func TestRoles_TheViewerReadsEverythingAndWritesNothing(t *testing.T) {
	ns := namespace(t, "viewer")
	v := bound(t, ns, "dorgu-viewer")
	ra := pendingAction(t, ns, "x")
	var list dorguv1.RemediationActionList
	require.NoError(t, v.c.List(ctxFor(t), &list, client.InNamespace(ns)))
	var events dorguv1.DorguEventList
	require.NoError(t, v.c.List(ctxFor(t), &events, client.InNamespace(ns)))
	err := admissionenv.Approve(ctxFor(t), v.c, ra, v.name)
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err))
}

// No chart role grants a Secret, a wildcard or a workload write.
func TestRoles_GrantNothingTheyShouldNot(t *testing.T) {
	requireRoles(t)
	for _, name := range []string{"dorgu-viewer", "dorgu-agent-reader", "dorgu-agent-proposer", "dorgu-approver"} {
		var role rbacv1.ClusterRole
		require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKey{Name: name}, &role))
		for _, rule := range role.Rules {
			assert.NotContains(t, rule.Resources, "*", name)
			assert.NotContains(t, rule.Resources, "secrets", name)
			assert.NotContains(t, rule.Verbs, "*", name)
			assert.NotContains(t, rule.Verbs, "delete", name)
			assert.NotContains(t, rule.Verbs, "deletecollection", name)
			assert.NotContains(t, rule.APIGroups, "apps", name)
			if name != "dorgu-approver" {
				assert.NotContains(t, rule.Verbs, "patch", name)
				assert.NotContains(t, rule.Verbs, "update", name)
			}
		}
	}
	var proposer rbacv1.ClusterRole
	require.NoError(t, env.Admin.Get(ctxFor(t), client.ObjectKey{Name: "dorgu-agent-proposer"}, &proposer))
	require.Len(t, proposer.Rules, 1)
	assert.ElementsMatch(t, []string{"create", "get", "list", "watch"}, proposer.Rules[0].Verbs)
}
