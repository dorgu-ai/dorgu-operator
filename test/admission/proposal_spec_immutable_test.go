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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// C2 review F-10: for a proposal the CRD's own immutability rule (`self == oldSelf` on spec)
// answers an edit first, so a proposer never sees the SpecImmutable token and nothing else
// exercised the policy's rule behind it. To test that rule on its own, this takes the CRD's
// rule off for the length of the case (the shipped CRD keeps it: this edits the envtest
// server's copy and puts it back), then edits a proposal's spec and asserts the policy's
// exact token. If the policy's rule were removed or broken, the edit would now land.
func TestSpecImmutable_TheProposalPolicyRuleStandsOnItsOwn(t *testing.T) {
	ctx := context.Background()
	ns := namespace(t, "si-vap")
	alice := newActor(t, ns, "", nil, append(append([]rbacv1.PolicyRule{}, proposerRules...), writerRules...))
	prop := admissionenv.NewProposal(ns, uniq("p"), alice.name)
	require.NoError(t, alice.c.Create(ctx, prop))

	const crdName = "remediationproposals.dorgu.io"
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, env.Admin.Get(ctx, client.ObjectKey{Name: crdName}, &crd))
	shipped := crd.Spec.DeepCopy()

	t.Cleanup(func() {
		var live apiextensionsv1.CustomResourceDefinition
		require.NoError(t, env.Admin.Get(context.Background(), client.ObjectKey{Name: crdName}, &live))
		live.Spec = *shipped
		require.NoError(t, env.Admin.Update(context.Background(), &live))
		// Wait for the rule to bite again, so the next test sees the shipped CRD.
		require.Eventually(t, func() bool {
			err := editRationale(ctx, alice.c, prop, "put back "+uniq("x"))
			return err != nil && strings.Contains(err.Error(), "spec is immutable") && !strings.Contains(err.Error(), "dorgu: ")
		}, 30*time.Second, 200*time.Millisecond, "the CRD's own rule must be back")
	})

	removed := 0
	for i := range crd.Spec.Versions {
		schema := crd.Spec.Versions[i].Schema.OpenAPIV3Schema
		spec := schema.Properties["spec"]
		kept := spec.XValidations[:0]
		for _, rule := range spec.XValidations {
			if strings.Contains(rule.Rule, "oldSelf") {
				removed++
				continue
			}
			kept = append(kept, rule)
		}
		spec.XValidations = kept
		schema.Properties["spec"] = spec
	}
	require.Equal(t, 1, removed, "the CRD has exactly the one immutability rule this case takes off")
	require.NoError(t, env.Admin.Update(ctx, &crd))

	var last error
	require.Eventually(t, func() bool {
		last = editRationale(ctx, alice.c, prop, "edited "+uniq("x"))
		return last != nil && admissionenv.IsDorguDenial(last, "SpecImmutable")
	}, 30*time.Second, 200*time.Millisecond,
		"with the CRD's rule off, the policy's SpecImmutable must be what refuses the edit (last answer: %v)", last)
	requireDenied(t, last, "SpecImmutable")

	// And the same edit through the status subresource is the operator's alone, whatever
	// the spec says: unchanged specs are fine for it.
	var fresh dorguv1alpha1.RemediationProposal
	require.NoError(t, env.Operator.Get(ctx, client.ObjectKeyFromObject(prop), &fresh))
	fresh.Status.Phase = "Received"
	require.NoError(t, env.Operator.Status().Update(ctx, &fresh), "a status write is not a spec edit")
}

func editRationale(
	ctx context.Context, c client.Client, prop *dorguv1alpha1.RemediationProposal, rationale string,
) error {
	var fresh dorguv1alpha1.RemediationProposal
	if err := c.Get(ctx, client.ObjectKeyFromObject(prop), &fresh); err != nil {
		return err
	}
	fresh.Spec.Rationale = rationale
	return c.Update(ctx, &fresh)
}
