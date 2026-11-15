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

// Package admission proves the ValidatingAdmissionPolicies the chart ships, against a
// real kube-apiserver (envtest): one case per denial reason in the contract
// (P2-execution.md section 3), each asserting the exact "dorgu: <Reason>: " token the
// CLI maps to a sentence, plus the writes the policies must allow.
package admission

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

var env *admissionenv.Env

func TestMain(m *testing.M) {
	var err error
	env, err = admissionenv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting the admission environment: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}

func ctxFor(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var counter atomic.Int64

// uniq makes a name no other case uses, so certificates and RBAC never collide.
func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, counter.Add(1)) }

// rule is a policy rule on Dorgu's group.
func rule(resources []string, verbs ...string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: []string{"dorgu.io"}, Resources: resources, Verbs: verbs}
}

// Rule sets a case grants.
var (
	approverRules = []rbacv1.PolicyRule{
		rule([]string{"remediationactions"}, "get", "list", "approve", "reject"),
		rule([]string{"remediationactions/status"}, "get", "patch", "update"),
	}
	statusOnlyRules = []rbacv1.PolicyRule{
		rule([]string{"remediationactions"}, "get", "list"),
		rule([]string{"remediationactions/status"}, "get", "patch", "update"),
	}
	approveOnlyRules = []rbacv1.PolicyRule{
		rule([]string{"remediationactions"}, "get", "list", "approve"),
		rule([]string{"remediationactions/status"}, "get", "patch", "update"),
	}
	proposerRules = []rbacv1.PolicyRule{
		rule([]string{"remediationproposals"}, "create", "get", "list", "watch"),
	}
	writerRules = []rbacv1.PolicyRule{
		rule([]string{"remediationactions", "remediationproposals", "incidentmemories"},
			"get", "list", "create", "update", "patch"),
		rule([]string{"remediationactions/status", "remediationproposals/status", "incidentmemories/status"},
			"get", "patch", "update"),
	}
)

// actor is an authenticated user with rules in a namespace.
type actor struct {
	name string
	c    client.Client
}

// newActor adds a user named name (a fresh one when empty) holding rules in ns.
func newActor(t *testing.T, ns, name string, groups []string, rules []rbacv1.PolicyRule) actor {
	t.Helper()
	if name == "" {
		name = uniq("user")
	}
	c, err := env.UserClient(name, groups)
	require.NoError(t, err)
	if len(rules) > 0 {
		require.NoError(t, env.GrantRules(ctxFor(t), ns, name, rules))
	}
	return actor{name: name, c: c}
}

func newServiceAccount(t *testing.T, ns string, rules []rbacv1.PolicyRule) actor {
	t.Helper()
	name := "system:serviceaccount:" + ns + ":" + uniq("bot")
	groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"}
	return newActor(t, ns, name, groups, rules)
}

func namespace(t *testing.T, prefix string) string {
	t.Helper()
	ns, err := env.NewNamespace(ctxFor(t), prefix)
	require.NoError(t, err)
	return ns
}

// pendingAction is a Pending action the operator made on behalf of proposer ("" for one
// that predates provenance).
func pendingAction(t *testing.T, ns, proposer string) *dorguv1.RemediationAction {
	t.Helper()
	ra := admissionenv.NewAction(ns, uniq("ra"), proposer)
	require.NoError(t, env.MakePending(ctxFor(t), ra))
	return ra
}

// requireDenied asserts err is the API server refusing with exactly the Dorgu reason.
func requireDenied(t *testing.T, err error, reason string) {
	t.Helper()
	require.Error(t, err, "expected a denial with reason %s", reason)
	require.True(t, admissionenv.IsDorguDenial(err, reason), "want a %q denial, got: %v", reason, err)
	for _, other := range allReasons {
		if other != reason {
			require.NotContains(t, err.Error(), "dorgu: "+other+": ", "exactly one reason is reported")
		}
	}
}

var allReasons = []string{
	"ProposerMismatch", "OperatorOnly", "SpecImmutable", "ServiceAccountCannotApprove", "ApprovedByMismatch",
	"ApproverIsProposer", "MissingApproveVerb", "InvalidTransition", "TargetNamespaceMismatch",
}

// incidentSpec is the smallest valid IncidentMemory spec in ns.
func incidentSpec(ns string) dorguv1.IncidentMemorySpec {
	return dorguv1.IncidentMemorySpec{
		PersonaRef: dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: admissionenv.PersonaName, Namespace: ns},
		Category:   "resource",
		Severity:   "critical",
		Detection: dorguv1.DetectionInfo{
			Signal: "OOMKilled", Source: "pod-failure-detector",
			FirstSeen: metav1.Now(), LastSeen: metav1.Now(),
			AffectedResources: []dorguv1.ResourceReference{{Kind: "Pod", Name: "web-abc", Namespace: ns}},
		},
	}
}
