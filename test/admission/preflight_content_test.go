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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dorgu-ai/dorgu-operator/internal/intake"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// C2 review F-3 on a real API server: the operator's content check agrees with what the API
// server stores for the chart's rendered policies (after its own defaulting), and notices
// each tampering the review tried.

func operatorIs(username string) intake.UsernameSource {
	return func(context.Context) (string, error) { return username, nil }
}

func contentCheck() intake.Check {
	return intake.AdmissionPoliciesCheck(env.Admin, operatorIs(admissionenv.OperatorUsername))
}

func TestPreflightContent_TheStoredPoliciesPassTheOperatorsOwnCheck(t *testing.T) {
	require.NoError(t, contentCheck().Run(ctxFor(t)),
		"the embedded reference and what the API server stores (defaults filled in) must agree")
}

func TestPreflightContent_AnotherOperatorNameIsRefused(t *testing.T) {
	err := intake.AdmissionPoliciesCheck(env.Admin, operatorIs("system:serviceaccount:prod:dorgu")).Run(ctxFor(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trusts")
}

func TestPreflightContent_EachTamperingOnTheRealServerIsWeakened(t *testing.T) {
	policy := func(name string, edit func(*admissionregistrationv1.ValidatingAdmissionPolicy)) func() client.Object {
		return func() client.Object {
			var p admissionregistrationv1.ValidatingAdmissionPolicy
			require.NoError(t, env.Admin.Get(context.Background(), client.ObjectKey{Name: name}, &p))
			edit(&p)
			return &p
		}
	}
	bind := func(name string, edit func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding)) func() client.Object {
		return func() client.Object {
			var b admissionregistrationv1.ValidatingAdmissionPolicyBinding
			require.NoError(t, env.Admin.Get(context.Background(), client.ObjectKey{Name: name}, &b))
			edit(&b)
			return &b
		}
	}
	cases := map[string]func() client.Object{
		"matchConditions false": policy("dorgu-approval-gate", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.MatchConditions = []admissionregistrationv1.MatchCondition{{Name: "never", Expression: "false"}}
		}),
		"a validation replaced by true": policy("dorgu-approval-gate", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			p.Spec.Validations[0].Expression = "true"
		}),
		"another operator identity": policy("dorgu-action-integrity", func(p *admissionregistrationv1.ValidatingAdmissionPolicy) {
			for i := range p.Spec.Variables {
				if p.Spec.Variables[i].Name == "operator" {
					p.Spec.Variables[i].Expression = "'system:serviceaccount:other:other'"
				}
			}
		}),
		"binding set to warn and audit": bind("dorgu-approval-gate", func(b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.ValidationActions = []admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn, admissionregistrationv1.Audit}
		}),
	}
	for name, tampered := range cases {
		t.Run(name, func(t *testing.T) {
			obj := tampered()

			// Save the shipped object, change it, and put it back however the case ends.
			shipped := shippedCopy(t, obj)
			require.NoError(t, env.Admin.Update(context.Background(), obj))
			t.Cleanup(func() {
				var live client.Object
				switch shipped.(type) {
				case *admissionregistrationv1.ValidatingAdmissionPolicy:
					live = &admissionregistrationv1.ValidatingAdmissionPolicy{}
				default:
					live = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
				}
				require.NoError(t, env.Admin.Get(context.Background(), client.ObjectKeyFromObject(shipped), live))
				switch s := shipped.(type) {
				case *admissionregistrationv1.ValidatingAdmissionPolicy:
					live.(*admissionregistrationv1.ValidatingAdmissionPolicy).Spec = s.Spec
				case *admissionregistrationv1.ValidatingAdmissionPolicyBinding:
					live.(*admissionregistrationv1.ValidatingAdmissionPolicyBinding).Spec = s.Spec
				}
				require.NoError(t, env.Admin.Update(context.Background(), live))
				// The API server notices a changed policy a moment later: do not hand the next
				// test a policy that is still the tampered one.
				require.NoError(t, env.WaitEnforcing(context.Background()))
			})

			err := contentCheck().Run(ctxFor(t))
			require.Error(t, err, "a tampered policy must close the intake")
			assert.Contains(t, err.Error(), "would not deny")
		})
	}
	require.NoError(t, contentCheck().Run(ctxFor(t)), "everything restored")
}

// shippedCopy re-reads the object as the API server holds it before the case changes it.
func shippedCopy(t *testing.T, like client.Object) client.Object {
	t.Helper()
	var out client.Object
	switch like.(type) {
	case *admissionregistrationv1.ValidatingAdmissionPolicy:
		out = &admissionregistrationv1.ValidatingAdmissionPolicy{}
	default:
		out = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	}
	require.NoError(t, env.Admin.Get(context.Background(), client.ObjectKey{Name: like.GetName()}, out))
	return out
}
