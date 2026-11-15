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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// C2 review F-3: the preflight used to check that the policies exist, not that they say what
// the chart wrote. A policy edited to never evaluate, to allow everything, or to trust
// another identity as "the operator" kept it green while a ServiceAccount approved.

type (
	policyEdit  func(*admissionregistrationv1.ValidatingAdmissionPolicy)
	bindingEdit func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
)

func withPolicyEdit(t *testing.T, name string, edit policyEdit) []client.Object {
	t.Helper()
	installed := installedPolicies(t)
	objs := make([]client.Object, 0, len(installed))
	for _, o := range installed {
		if p, ok := o.(*admissionregistrationv1.ValidatingAdmissionPolicy); ok && p.Name == name {
			edit(p)
		}
		objs = append(objs, o)
	}
	return objs
}

// withBindingEdit changes the approval gate's binding (the one the review tampered with).
func withBindingEdit(t *testing.T, edit bindingEdit) []client.Object {
	t.Helper()
	installed := installedPolicies(t)
	objs := make([]client.Object, 0, len(installed))
	for _, o := range installed {
		if b, ok := o.(*admissionregistrationv1.ValidatingAdmissionPolicyBinding); ok && b.Name == "dorgu-approval-gate" {
			edit(b)
		}
		objs = append(objs, o)
	}
	return objs
}

func TestAdmissionPoliciesCheck_ThePoliciesAsShippedAreHealthy(t *testing.T) {
	require.NoError(t, policiesCheck(policyReader(t, installedPolicies(t)...)).Run(context.Background()))
}

// Every tampering the review tried closes the intake with AdmissionPoliciesWeakened.
func TestAdmissionPoliciesCheck_ATamperedPolicyIsWeakened(t *testing.T) {
	const gate = "dorgu-approval-gate"
	cases := []struct {
		name string
		objs []client.Object
	}{
		{"matchConditions=[false]: the policy never evaluates", withPolicyEdit(t, gate, func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.MatchConditions = []admissionregistrationv1.MatchCondition{{Name: "never", Expression: "false"}}
		})},
		{"a validation replaced by true", withPolicyEdit(t, gate, func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.Validations[0].Expression = "true"
		})},
		{"a validation removed", withPolicyEdit(t, gate, func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.Validations = p.Spec.Validations[1:]
		})},
		{"a validation's message changed (the CLI keys on it)", withPolicyEdit(t, gate, func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.Validations[0].Message = "denied"
		})},
		{"another identity is the operator", withPolicyEdit(t, gate, func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			for i := range p.Spec.Variables {
				if p.Spec.Variables[i].Name == "operator" {
					p.Spec.Variables[i].Expression = "'system:serviceaccount:other:other'"
				}
			}
		})},
		{"another variable edited", withPolicyEdit(t, gate, func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			for i := range p.Spec.Variables {
				if p.Spec.Variables[i].Name == "proposer" {
					p.Spec.Variables[i].Expression = "'nobody'"
				}
			}
		})},
		{"the policy stops matching status writes", withPolicyEdit(t, gate, func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.MatchConstraints.ResourceRules[0].Resources = []string{"remediationactions"}
		})},
		{"the policy only matches CREATE", withPolicyEdit(t, "dorgu-action-integrity", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.MatchConstraints.ResourceRules[0].Operations = []admissionregistrationv1.OperationType{admissionregistrationv1.Create}
		})},
		{"a proposal-identity validation truncated", withPolicyEdit(t, "dorgu-proposal-identity", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.Validations = p.Spec.Validations[:1]
		})},
		{"the binding only warns and audits", withBindingEdit(t, func(b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.ValidationActions = []admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn, admissionregistrationv1.Audit}
		})},
		{"the binding gains a parameter reference", withBindingEdit(t, func(b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.ParamRef = &admissionregistrationv1.ParamRef{Name: "x"}
		})},
		{"the binding excludes a resource", withBindingEdit(t, func(b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.MatchResources = &admissionregistrationv1.MatchResources{
				ExcludeResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{}},
			}
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := policiesCheck(policyReader(t, tc.objs...)).Run(context.Background())
			require.Error(t, err, "a tampered policy must not pass")
			assert.Equal(t, ReasonAdmissionPoliciesWeakened, failureReason(err, "fallback"), err.Error())
		})
	}
}

// Harmless differences are not tampering: the order of the binding's actions, an extra label,
// and the empty selectors the API server fills in.
func TestAdmissionPoliciesCheck_HarmlessDifferencesPass(t *testing.T) {
	objs := withBindingEdit(t, func(b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
		b.Spec.ValidationActions = []admissionregistrationv1.ValidationAction{admissionregistrationv1.Audit, admissionregistrationv1.Deny}
		b.Labels = map[string]string{"extra": "label"}
	})
	objs = append(objs[:0:0], objs...)
	for i, o := range objs {
		if p, ok := o.(*admissionregistrationv1.ValidatingAdmissionPolicy); ok {
			p = p.DeepCopy()
			p.Labels = map[string]string{"extra": "label"}
			p.Spec.MatchConstraints.NamespaceSelector = &metav1.LabelSelector{}
			p.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{}
			p.Spec.MatchConstraints.MatchPolicy = ptr.To(admissionregistrationv1.Equivalent)
			objs[i] = p
		}
	}
	require.NoError(t, policiesCheck(policyReader(t, objs...)).Run(context.Background()))
}

// (a) A policy that trusts another identity as the operator denies every write the operator
// makes, and the operator should say so by name, not just "weakened".
func TestAdmissionPoliciesCheck_PoliciesForAnotherOperatorNameBothIdentities(t *testing.T) {
	check := AdmissionPoliciesCheck(policyReader(t, installedPolicies(t)...), usernameFor("system:serviceaccount:prod:dorgu"))
	err := check.Run(context.Background())
	require.Error(t, err)
	assert.Equal(t, ReasonAdmissionPoliciesWeakened, failureReason(err, "fallback"))
	assert.Contains(t, err.Error(), TestOperatorUsername, "what the policies trust")
	assert.Contains(t, err.Error(), "system:serviceaccount:prod:dorgu", "who the operator is")
}

func TestAdmissionPoliciesCheck_AnOperatorThatCannotSayWhoItIsIsUnverified(t *testing.T) {
	for name, source := range map[string]UsernameSource{
		"error": func(context.Context) (string, error) { return "", errors.New("connection refused") },
		"empty": usernameFor(""),
		"nil":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			err := AdmissionPoliciesCheck(policyReader(t, installedPolicies(t)...), source).Run(context.Background())
			require.Error(t, err)
			assert.Equal(t, ReasonAdmissionPoliciesUnverified, failureReason(err, "fallback"))
		})
	}
}

// A policy of another release differs in content too; it is reported as outdated (what to
// do about it is helm upgrade), not as tampered.
func TestAdmissionPoliciesCheck_AnOlderReleaseIsOutdatedNotWeakened(t *testing.T) {
	objs := withPolicyEdit(t, "dorgu-approval-gate", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
		p.Annotations[AdmissionPolicyVersionAnnotation] = "0"
		p.Spec.Validations = p.Spec.Validations[:1]
	})
	err := policiesCheck(policyReader(t, objs...)).Run(context.Background())
	require.Error(t, err)
	assert.Equal(t, ReasonAdmissionPoliciesOutdated, failureReason(err, "fallback"))
}

func TestReferencePolicies_AreTheChartsThreePoliciesForTheOperator(t *testing.T) {
	policies, bindings, err := ReferencePolicies("system:serviceaccount:x:y")
	require.NoError(t, err)
	names := make([]string, 0, len(policies))
	bound := make([]string, 0, len(bindings))
	for _, p := range policies {
		names = append(names, p.Name)
		assert.Equal(t, AdmissionPolicyVersion, p.Annotations[AdmissionPolicyVersionAnnotation])
		found := false
		for _, v := range p.Spec.Variables {
			if v.Name == "operator" {
				found = true
				assert.Equal(t, "'system:serviceaccount:x:y'", v.Expression)
			}
		}
		assert.True(t, found, p.Name)
	}
	for _, b := range bindings {
		bound = append(bound, b.Name)
	}
	assert.ElementsMatch(t, AdmissionPolicyNames, names)
	assert.ElementsMatch(t, AdmissionPolicyNames, bound)
}
