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

package intake

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func admissionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, admissionregistrationv1.AddToScheme(s))
	return s
}

func policy(name string, mutate ...func(*admissionregistrationv1.ValidatingAdmissionPolicy)) *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	p := &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{AdmissionPolicyVersionAnnotation: AdmissionPolicyVersion}},
		Spec:       admissionregistrationv1.ValidatingAdmissionPolicySpec{FailurePolicy: &fail},
	}
	for _, m := range mutate {
		m(p)
	}
	return p
}

func binding(name, policyName string, mutate ...func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding)) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	b := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        policyName,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny, admissionregistrationv1.Audit},
		},
	}
	for _, m := range mutate {
		m(b)
	}
	return b
}

func installedPolicies(t *testing.T) []client.Object {
	t.Helper()
	var objs []client.Object
	for _, name := range AdmissionPolicyNames {
		objs = append(objs, policy(name), binding(name, name))
	}
	return objs
}

func policyReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithObjects(objs...).Build()
}

// D8: the operator stops converting proposals and issuing tickets when the policies that
// make approvedBy trustworthy are missing, at startup or later.
func TestAdmissionPoliciesCheck(t *testing.T) {
	without := func(kind, name string) []client.Object {
		var out []client.Object
		for _, o := range installedPolicies(t) {
			_, isPolicy := o.(*admissionregistrationv1.ValidatingAdmissionPolicy)
			if o.GetName() == name && isPolicy == (kind == "policy") {
				continue
			}
			out = append(out, o)
		}
		return out
	}
	replace := func(objs []client.Object, with client.Object) []client.Object {
		out := []client.Object{with}
		for _, o := range objs {
			if o.GetName() == with.GetName() && fmtKind(o) == fmtKind(with) {
				continue
			}
			out = append(out, o)
		}
		return out
	}

	cases := []struct {
		name   string
		objs   []client.Object
		reason string
		inMsg  string
	}{
		{"everything in place", installedPolicies(t), "", ""},
		{"no policies at all", nil, ReasonAdmissionPoliciesMissing, "dorgu-approval-gate"},
		{"one policy missing", without("policy", "dorgu-approval-gate"), ReasonAdmissionPoliciesMissing, "dorgu-approval-gate"},
		{"one binding missing", without("binding", "dorgu-action-integrity"), ReasonAdmissionPoliciesMissing, "dorgu-action-integrity"},
		{"a policy that fails open", replace(installedPolicies(t), policy("dorgu-approval-gate", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			ignore := admissionregistrationv1.Ignore
			p.Spec.FailurePolicy = &ignore
		})), ReasonAdmissionPoliciesWeakened, "failurePolicy"},
		{"a policy with no failurePolicy (defaults to Fail, but say so)", replace(installedPolicies(t), policy("dorgu-approval-gate", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.FailurePolicy = nil
		})), "", ""},
		{"a binding that only warns", replace(installedPolicies(t), binding("dorgu-approval-gate", "dorgu-approval-gate", func(b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.ValidationActions = []admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn, admissionregistrationv1.Audit}
		})), ReasonAdmissionPoliciesWeakened, "Deny"},
		{"a binding that points at another policy", replace(installedPolicies(t), binding("dorgu-approval-gate", "something-else")), ReasonAdmissionPoliciesWeakened, "something-else"},
		{"a binding narrowed to a namespace", replace(installedPolicies(t), binding("dorgu-approval-gate", "dorgu-approval-gate", func(b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.MatchResources = &admissionregistrationv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}}
		})), ReasonAdmissionPoliciesWeakened, "narrow"},
		{"a policy from another release", replace(installedPolicies(t), policy("dorgu-approval-gate", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Annotations[AdmissionPolicyVersionAnnotation] = "0"
		})), ReasonAdmissionPoliciesOutdated, "helm upgrade"},
		{"a policy with no version", replace(installedPolicies(t), policy("dorgu-approval-gate", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Annotations = nil
		})), ReasonAdmissionPoliciesOutdated, "version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AdmissionPoliciesCheck(policyReader(t, tc.objs...)).Run(context.Background())
			if tc.reason == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.reason, failureReason(err, "fallback"))
			assert.Contains(t, err.Error(), tc.inMsg)
		})
	}
}

func fmtKind(o client.Object) string {
	if _, ok := o.(*admissionregistrationv1.ValidatingAdmissionPolicy); ok {
		return "policy"
	}
	return "binding"
}

func TestAdmissionPoliciesCheck_AReadErrorIsAFailureNotAPass(t *testing.T) {
	forbidden := interceptor.NewClient(fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithObjects(installedPolicies(t)...).Build(),
		interceptor.Funcs{Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies"}, key.Name, errors.New("no"))
		}})

	err := AdmissionPoliciesCheck(forbidden).Run(context.Background())

	require.Error(t, err)
	assert.Equal(t, ReasonAdmissionPoliciesUnverified, failureReason(err, "fallback"))
	assert.Contains(t, err.Error(), "get", "it names the verb the operator needs")
	assert.Contains(t, err.Error(), "validatingadmissionpolic")
}

// The operator reads exactly the policies and bindings it ships, by name.
func TestAdmissionPolicyNames(t *testing.T) {
	assert.Equal(t, []string{"dorgu-proposal-identity", "dorgu-action-integrity", "dorgu-approval-gate"}, AdmissionPolicyNames)
}

// C1 re-check N-4: the preflight checks the RBAC the operator needs with a
// SelfSubjectAccessReview, so a partial hand-written role closes the intake with a message
// naming what is missing, instead of a cache that never syncs and a crash loop.
func TestOperatorRBACCheck(t *testing.T) {
	allowAll := func(context.Context, authzv1.ResourceAttributes) (bool, string, error) { return true, "", nil }

	t.Run("everything granted", func(t *testing.T) {
		require.NoError(t, OperatorRBACCheck(allowAll).Run(context.Background()))
	})

	t.Run("a missing verb is named, with its resource", func(t *testing.T) {
		deny := func(_ context.Context, a authzv1.ResourceAttributes) (bool, string, error) {
			if a.Resource == "remediationproposals" && a.Verb == "watch" && a.Subresource == "" {
				return false, "no RBAC policy matched", nil
			}
			return true, "", nil
		}
		err := OperatorRBACCheck(deny).Run(context.Background())
		require.Error(t, err)
		assert.Equal(t, ReasonOperatorRBACMissing, failureReason(err, "fallback"))
		assert.Contains(t, err.Error(), "watch remediationproposals.dorgu.io")
		assert.NotContains(t, err.Error(), "create remediationactions.dorgu.io", "only what is missing")
	})

	t.Run("a subresource is named as one", func(t *testing.T) {
		deny := func(_ context.Context, a authzv1.ResourceAttributes) (bool, string, error) {
			return !(a.Resource == "remediationactions" && a.Subresource == "status" && a.Verb == "update"), "", nil
		}
		err := OperatorRBACCheck(deny).Run(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "update remediationactions.dorgu.io/status")
	})

	t.Run("a policy read is checked by name", func(t *testing.T) {
		var seen []string
		record := func(_ context.Context, a authzv1.ResourceAttributes) (bool, string, error) {
			if a.Group == "admissionregistration.k8s.io" {
				seen = append(seen, a.Resource+"/"+a.Name+":"+a.Verb)
			}
			return true, "", nil
		}
		require.NoError(t, OperatorRBACCheck(record).Run(context.Background()))
		for _, name := range AdmissionPolicyNames {
			assert.Contains(t, seen, "validatingadmissionpolicies/"+name+":get")
			assert.Contains(t, seen, "validatingadmissionpolicybindings/"+name+":get")
		}
	})

	t.Run("a review that cannot be made is not a pass", func(t *testing.T) {
		broken := func(context.Context, authzv1.ResourceAttributes) (bool, string, error) {
			return false, "", errors.New("connection refused")
		}
		err := OperatorRBACCheck(broken).Run(context.Background())
		require.Error(t, err)
		assert.Equal(t, ReasonOperatorRBACUnverified, failureReason(err, "fallback"))
		assert.Contains(t, err.Error(), "SelfSubjectAccessReview")
	})

	t.Run("a long list of gaps is bounded", func(t *testing.T) {
		denyAll := func(context.Context, authzv1.ResourceAttributes) (bool, string, error) { return false, "", nil }
		err := OperatorRBACCheck(denyAll).Run(context.Background())
		require.Error(t, err)
		assert.Less(t, len(err.Error()), 2000)
		assert.True(t, strings.Contains(err.Error(), "more"), "it says how many it left out")
	})
}

func TestOperatorRBACNeeds_CoverTheIntakeAndTheApprovalPath(t *testing.T) {
	var got []string
	record := func(_ context.Context, a authzv1.ResourceAttributes) (bool, string, error) {
		got = append(got, a.Verb+" "+a.Resource+"/"+a.Subresource)
		return true, "", nil
	}
	require.NoError(t, OperatorRBACCheck(record).Run(context.Background()))
	for _, want := range []string{
		"list remediationproposals/", "watch remediationproposals/", "get remediationproposals/", "delete remediationproposals/",
		"update remediationproposals/status", "create remediationactions/", "update remediationactions/status",
		"create incidentmemories/", "update incidentmemories/status", "create selfsubjectreviews/",
	} {
		assert.Contains(t, got, want)
	}
}

// Tickets stop with the policies: any failure of the admission or RBAC checks blocks
// them, and nothing else does.
func TestPreflight_TicketsAreBlockedByTheAdmissionAndRBACChecksOnly(t *testing.T) {
	for reason, blocks := range map[string]bool{
		ReasonAdmissionPoliciesMissing:    true,
		ReasonAdmissionPoliciesWeakened:   true,
		ReasonAdmissionPoliciesOutdated:   true,
		ReasonAdmissionPoliciesUnverified: true,
		ReasonOperatorRBACMissing:         true,
		ReasonOperatorRBACUnverified:      true,
		ReasonProposalCRDMissing:          false,
		ReasonOperatorIdentityUnknown:     false,
	} {
		p := NewPreflight(logr.Discard(), failing("c", reason, "broken"))
		p.Run(context.Background())
		got := p.TicketsBlocked()
		if blocks {
			require.NotNil(t, got, reason)
			assert.Equal(t, reason, got.Reason)
		} else {
			assert.Nil(t, got, reason)
		}
	}

	open := NewPreflight(logr.Discard(), passing("a"))
	open.Run(context.Background())
	assert.Nil(t, open.TicketsBlocked())

	var nilPreflight *Preflight
	assert.Nil(t, nilPreflight.TicketsBlocked(), "no preflight (health check off) blocks nothing")

	// Until the first run, nothing is known: fail closed.
	fresh := NewPreflight(logr.Discard(), passing("a"))
	require.NotNil(t, fresh.TicketsBlocked())
	assert.Equal(t, ReasonPreflightPending, fresh.TicketsBlocked().Reason)
}
