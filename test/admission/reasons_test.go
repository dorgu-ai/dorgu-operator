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
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// ProposerMismatch: a proposal names who proposed it; it must be the requester.
func TestProposerMismatch(t *testing.T) {
	ns := namespace(t, "pm")

	t.Run("a proposal written as someone else", func(t *testing.T) {
		alice := newActor(t, ns, "", nil, proposerRules)
		err := alice.c.Create(ctxFor(t), admissionenv.NewProposal(ns, uniq("p"), "bob"))
		requireDenied(t, err, "ProposerMismatch")
	})

	t.Run("the authenticated name is accepted", func(t *testing.T) {
		alice := newActor(t, ns, "", nil, proposerRules)
		require.NoError(t, alice.c.Create(ctxFor(t), admissionenv.NewProposal(ns, uniq("p"), alice.name)))
	})

	t.Run("a service account proposes as itself", func(t *testing.T) {
		bot := newServiceAccount(t, ns, proposerRules)
		require.NoError(t, bot.c.Create(ctxFor(t), admissionenv.NewProposal(ns, uniq("p"), bot.name)))
	})

	// An identity is never rewritten: a name with an invisible character is refused even
	// when the proposal repeats it exactly, because the approver is compared as written.
	for name, unclean := range map[string]string{
		"zero width space": "ali\u200bce", "bidi override": "alice\u202e", "tag character": "alice\U000E0041",
		"variation selector": "alice\uFE0F", "word joiner": "alice\u2060", "soft hyphen": "ali\u00adce",
	} {
		t.Run("a username with a "+name, func(t *testing.T) {
			user := newActor(t, ns, unclean+uniq(""), nil, proposerRules)
			err := user.c.Create(ctxFor(t), admissionenv.NewProposal(ns, uniq("p"), user.name))
			requireDenied(t, err, "ProposerMismatch")
		})
	}
}

// TargetNamespaceMismatch: CRD validation cannot read metadata.namespace, so the policy
// does.
func TestTargetNamespaceMismatch(t *testing.T) {
	ns, other := namespace(t, "tn"), namespace(t, "tn-other")
	alice := newActor(t, ns, "", nil, proposerRules)

	prop := admissionenv.NewProposal(ns, uniq("p"), alice.name)
	prop.Spec.Target.Namespace = other
	requireDenied(t, alice.c.Create(ctxFor(t), prop), "TargetNamespaceMismatch")

	require.NoError(t, alice.c.Create(ctxFor(t), admissionenv.NewProposal(ns, uniq("p"), alice.name)),
		"a proposal in its target's namespace is accepted")
}

// OperatorOnly: creating an action, and writing what only the operator owns.
func TestOperatorOnly(t *testing.T) {
	ns := namespace(t, "oo")

	t.Run("a non-operator creates a RemediationAction", func(t *testing.T) {
		agent := newActor(t, ns, "", nil, writerRules)
		requireDenied(t, agent.c.Create(ctxFor(t), admissionenv.NewAction(ns, uniq("ra"), "")), "OperatorOnly")
		requireDenied(t, env.Admin.Create(ctxFor(t), admissionenv.NewAction(ns, uniq("ra"), "")), "OperatorOnly")
	})

	t.Run("the operator creates one", func(t *testing.T) {
		require.NoError(t, env.Operator.Create(ctxFor(t), admissionenv.NewAction(ns, uniq("ra"), "alice")))
	})

	t.Run("a dorgu.io annotation or label on an action", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		user := newActor(t, ns, "", nil, writerRules)
		kinds := map[string]string{
			"dorgu.io/answers-signal": "annotation", "dorgu.io/escalated-at": "annotation", "dorgu.io/phase": "label",
		}
		for key, kind := range kinds {
			fresh := &dorguv1.RemediationAction{}
			require.NoError(t, user.c.Get(ctxFor(t), client.ObjectKeyFromObject(ra), fresh))
			if kind == "annotation" {
				fresh.Annotations = map[string]string{key: "now"}
			} else {
				fresh.Labels = map[string]string{key: "x"}
			}
			requireDenied(t, user.c.Update(ctxFor(t), fresh), "OperatorOnly")
		}
		// Another annotation is the user's own business.
		fresh := &dorguv1.RemediationAction{}
		require.NoError(t, user.c.Get(ctxFor(t), client.ObjectKeyFromObject(ra), fresh))
		fresh.Annotations = map[string]string{"example.com/note": "fine"}
		require.NoError(t, user.c.Update(ctxFor(t), fresh))
	})

	t.Run("removing an operator annotation is a write too", func(t *testing.T) {
		ra := admissionenv.NewAction(ns, uniq("ra"), "alice")
		ra.Annotations = map[string]string{"dorgu.io/proposal-uid": "u-1"}
		require.NoError(t, env.Operator.Create(ctxFor(t), ra))
		user := newActor(t, ns, "", nil, writerRules)
		fresh := &dorguv1.RemediationAction{}
		require.NoError(t, user.c.Get(ctxFor(t), client.ObjectKeyFromObject(ra), fresh))
		fresh.Annotations = nil
		requireDenied(t, user.c.Update(ctxFor(t), fresh), "OperatorOnly")
	})

	t.Run("the operator writes dorgu.io annotations", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		fresh := &dorguv1.RemediationAction{}
		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(ra), fresh))
		fresh.Annotations = map[string]string{"dorgu.io/escalated-at": "now"}
		require.NoError(t, env.Operator.Update(ctxFor(t), fresh))
	})

	t.Run("the gate record and the apply ticket on an action", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		approver := newActor(t, ns, "", nil, approverRules)
		forged := map[string]any{"applyTicket": map[string]any{
			"specHash": "00", "approvedBy": approver.name, "expiresAt": "2099-01-01T00:00:00Z",
			"patch": map[string]any{},
		}}
		requireDenied(t, admissionenv.PatchStatus(ctxFor(t), approver.c, ra, forged), "OperatorOnly")
		forgedGate := map[string]any{"gate": map[string]any{
			"phase": "Proposal", "result": "Pass", "evaluatedAt": "2099-01-01T00:00:00Z",
		}}
		requireDenied(t, admissionenv.PatchStatus(ctxFor(t), approver.c, ra, forgedGate), "OperatorOnly")
		requireDenied(t, admissionenv.PatchStatus(ctxFor(t), approver.c, ra,
			map[string]any{"appliedAt": "2099-01-01T00:00:00Z"}), "OperatorOnly")
		requireDenied(t, admissionenv.PatchStatus(ctxFor(t), approver.c, ra,
			map[string]any{"verificationResult": "Healthy"}), "OperatorOnly")
		requireDenied(t, admissionenv.PatchStatus(ctxFor(t), approver.c, ra,
			map[string]any{"currentStep": 3}), "OperatorOnly")
	})

	t.Run("an operator-owned condition on an action", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		approver := newActor(t, ns, "", nil, approverRules)
		forged := map[string]any{"conditions": []map[string]any{{
			"type": "Ticketed", "status": "True", "reason": "TicketIssued", "message": "forged",
			"lastTransitionTime": "2099-01-01T00:00:00Z",
		}}}
		requireDenied(t, admissionenv.PatchStatus(ctxFor(t), approver.c, ra, forged), "OperatorOnly")
	})

	t.Run("an incident memory's status, annotations and labels", func(t *testing.T) {
		im := newIncident(ns)
		user := newActor(t, ns, "", nil, writerRules)

		labelled := im.DeepCopy()
		labelled.Labels = map[string]string{"dorgu.io/persona-name": "web"}
		requireDenied(t, user.c.Create(ctxFor(t), labelled), "OperatorOnly")
		annotated := im.DeepCopy()
		annotated.Annotations = map[string]string{"dorgu.io/answers-signal": "OOMKilled"}
		requireDenied(t, user.c.Create(ctxFor(t), annotated), "OperatorOnly")

		require.NoError(t, env.Operator.Create(ctxFor(t), im), "the operator writes both")
		stored := &dorguv1.IncidentMemory{}
		require.NoError(t, user.c.Get(ctxFor(t), client.ObjectKeyFromObject(im), stored))
		stored.Status.Phase = "Resolved"
		requireDenied(t, user.c.Status().Update(ctxFor(t), stored), "OperatorOnly")

		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(im), stored))
		stored.Status.Phase = "Resolved"
		require.NoError(t, env.Operator.Status().Update(ctxFor(t), stored))

		// A plain incident from a user is theirs to write: nothing here is operator state.
		plain := newIncident(ns)
		require.NoError(t, user.c.Create(ctxFor(t), plain))
	})

	// C2 review F-5: a proposer with no delete verb could still erase its own proposals by
	// naming a missing owner (the garbage collector deletes what has none), wedge them with a
	// finalizer, or write operator state under dorgu.io/. The operator is exempt: it owns the
	// proposals it converts.
	t.Run("ownerReferences, finalizers and dorgu.io metadata on a proposal", func(t *testing.T) {
		alice := newActor(t, ns, "", nil, append(append([]rbacv1.PolicyRule{}, proposerRules...), writerRules...))
		ghost := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "ghost", UID: "11111111-2222-3333-4444-555555555555"}
		mutations := map[string]func(*dorguv1alpha1.RemediationProposal){
			"an owner reference to something that does not exist": func(p *dorguv1alpha1.RemediationProposal) { p.OwnerReferences = []metav1.OwnerReference{ghost} },
			"a finalizer nobody removes":                          func(p *dorguv1alpha1.RemediationProposal) { p.Finalizers = []string{"example.com/stuck"} },
			"a dorgu.io annotation": func(p *dorguv1alpha1.RemediationProposal) {
				p.Annotations = map[string]string{"dorgu.io/answers-signal": "x"}
			},
			"a dorgu.io label": func(p *dorguv1alpha1.RemediationProposal) {
				p.Labels = map[string]string{"dorgu.io/persona-name": "web"}
			},
			"all of them at once (the review's probe)": func(p *dorguv1alpha1.RemediationProposal) {
				p.OwnerReferences = []metav1.OwnerReference{ghost}
				p.Finalizers = []string{"example.com/stuck"}
				p.Annotations = map[string]string{"dorgu.io/a": "1"}
				p.Labels = map[string]string{"dorgu.io/l": "1"}
			},
		}
		for name, mutate := range mutations {
			t.Run("create with "+name, func(t *testing.T) {
				prop := admissionenv.NewProposal(ns, uniq("p"), alice.name)
				mutate(prop)
				requireDenied(t, alice.c.Create(ctxFor(t), prop), "OperatorOnly")
			})
			t.Run("update adding "+name, func(t *testing.T) {
				prop := admissionenv.NewProposal(ns, uniq("p"), alice.name)
				require.NoError(t, alice.c.Create(ctxFor(t), prop))
				fresh := &dorguv1alpha1.RemediationProposal{}
				require.NoError(t, alice.c.Get(ctxFor(t), client.ObjectKeyFromObject(prop), fresh))
				mutate(fresh)
				requireDenied(t, alice.c.Update(ctxFor(t), fresh), "OperatorOnly")
			})
		}

		t.Run("other annotations, labels and no owner are the proposer's own business", func(t *testing.T) {
			prop := admissionenv.NewProposal(ns, uniq("p"), alice.name)
			prop.Annotations = map[string]string{"example.com/note": "fine"}
			prop.Labels = map[string]string{"team": "sre"}
			require.NoError(t, alice.c.Create(ctxFor(t), prop))
		})

		t.Run("the operator sets them on a proposal it converts, and a later user update leaves them alone", func(t *testing.T) {
			prop := admissionenv.NewProposal(ns, uniq("p"), alice.name)
			require.NoError(t, alice.c.Create(ctxFor(t), prop))

			owned := &dorguv1alpha1.RemediationProposal{}
			require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(prop), owned))
			owned.OwnerReferences = []metav1.OwnerReference{ghost}
			owned.Finalizers = []string{"dorgu.io/proposal"}
			owned.Annotations = map[string]string{"dorgu.io/converted-to": "ra-1"}
			owned.Labels = map[string]string{"dorgu.io/state": "converted"}
			require.NoError(t, env.Operator.Update(ctxFor(t), owned), "the operator is exempt")

			// A user's update that touches none of them is fine; changing one is not.
			fresh := &dorguv1alpha1.RemediationProposal{}
			require.NoError(t, alice.c.Get(ctxFor(t), client.ObjectKeyFromObject(prop), fresh))
			fresh.Annotations["example.com/note"] = "still fine"
			require.NoError(t, alice.c.Update(ctxFor(t), fresh))

			require.NoError(t, alice.c.Get(ctxFor(t), client.ObjectKeyFromObject(prop), fresh))
			fresh.OwnerReferences = nil
			requireDenied(t, alice.c.Update(ctxFor(t), fresh), "OperatorOnly")
			require.NoError(t, alice.c.Get(ctxFor(t), client.ObjectKeyFromObject(prop), fresh))
			fresh.Finalizers = nil
			requireDenied(t, alice.c.Update(ctxFor(t), fresh), "OperatorOnly")
		})
	})

	t.Run("a proposal's status", func(t *testing.T) {
		alice := newActor(t, ns, "", nil, append(append([]rbacv1.PolicyRule{}, proposerRules...), writerRules...))
		prop := admissionenv.NewProposal(ns, uniq("p"), alice.name)
		require.NoError(t, alice.c.Create(ctxFor(t), prop))
		prop.Status.Phase = "Converted"
		requireDenied(t, alice.c.Status().Update(ctxFor(t), prop), "OperatorOnly")

		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(prop), prop))
		prop.Status.Phase = "Received"
		require.NoError(t, env.Operator.Status().Update(ctxFor(t), prop), "the operator answers")
	})
}

// SpecImmutable: a plan is what was approved.
func TestSpecImmutable(t *testing.T) {
	ns := namespace(t, "si")
	ra := pendingAction(t, ns, "alice")

	for name, c := range map[string]client.Client{"an administrator": env.Admin, "the operator": env.Operator} {
		t.Run(name+" edits an action's spec", func(t *testing.T) {
			fresh := &dorguv1.RemediationAction{}
			require.NoError(t, c.Get(ctxFor(t), client.ObjectKeyFromObject(ra), fresh))
			fresh.Spec.Explanation = "something else"
			requireDenied(t, c.Update(ctxFor(t), fresh), "SpecImmutable")
		})
	}

	t.Run("a status write leaves the spec alone and is not a spec edit", func(t *testing.T) {
		fresh := &dorguv1.RemediationAction{}
		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(ra), fresh))
		fresh.Status.CurrentStep = 0
		require.NoError(t, env.Operator.Status().Update(ctxFor(t), fresh))
	})

	t.Run("a proposal's spec", func(t *testing.T) {
		alice := newActor(t, ns, "", nil, append(append([]rbacv1.PolicyRule{}, proposerRules...), writerRules...))
		prop := admissionenv.NewProposal(ns, uniq("p"), alice.name)
		require.NoError(t, alice.c.Create(ctxFor(t), prop))
		prop.Spec.Rationale = "now it says something else"
		// The CRD's own immutability rule answers first, so the policy's is a second wall;
		// either way the edit never lands.
		require.Error(t, alice.c.Update(ctxFor(t), prop))
	})
}

// ServiceAccountCannotApprove: approving is a human decision.
func TestServiceAccountCannotApprove(t *testing.T) {
	ns := namespace(t, "sa")

	t.Run("a service account that holds the approve verb and the status patch", func(t *testing.T) {
		ra := pendingAction(t, ns, "someone-else")
		bot := newServiceAccount(t, ns, approverRules)
		requireDenied(t, admissionenv.Approve(ctxFor(t), bot.c, ra, bot.name), "ServiceAccountCannotApprove")
		requireDenied(t, admissionenv.Reject(ctxFor(t), bot.c, ra, bot.name), "ServiceAccountCannotApprove")
	})

	t.Run("not even with a cluster admin's rights", func(t *testing.T) {
		ra := pendingAction(t, ns, "someone-else")
		name := "system:serviceaccount:" + ns + ":" + uniq("root")
		root := newActor(t, ns, name, []string{"system:masters"}, nil)
		requireDenied(t, admissionenv.Approve(ctxFor(t), root.c, ra, name), "ServiceAccountCannotApprove")
	})

	t.Run("the operator itself never approves", func(t *testing.T) {
		ra := pendingAction(t, ns, "someone-else")
		err := admissionenv.Approve(ctxFor(t), env.Operator, ra, admissionenv.OperatorUsername)
		requireDenied(t, err, "ServiceAccountCannotApprove")
	})
}

// ApprovedByMismatch: the record names the authenticated user, on a rejection as well.
func TestApprovedByMismatch(t *testing.T) {
	ns := namespace(t, "ab")
	alice := newActor(t, ns, "", nil, approverRules)

	t.Run("approve as someone else", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		requireDenied(t, admissionenv.Approve(ctxFor(t), alice.c, ra, "cli-user"), "ApprovedByMismatch")
	})
	t.Run("reject as someone else", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		requireDenied(t, admissionenv.Reject(ctxFor(t), alice.c, ra, "cli-user"), "ApprovedByMismatch")
	})
	t.Run("approve with no approver at all", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		err := admissionenv.PatchStatus(ctxFor(t), alice.c, ra, map[string]any{"phase": "Approved"})
		requireDenied(t, err, "ApprovedByMismatch")
	})
	t.Run("an approver written with surrounding space", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		requireDenied(t, admissionenv.Approve(ctxFor(t), alice.c, ra, " "+alice.name+" "), "ApprovedByMismatch")
	})
	t.Run("a user whose name has an invisible character cannot approve at all", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		ghost := newActor(t, ns, "gho\u200bst-"+uniq(""), nil, approverRules)
		requireDenied(t, admissionenv.Approve(ctxFor(t), ghost.c, ra, ghost.name), "ApprovedByMismatch")
	})
}

// ApproverIsProposer: whoever proposed a remediation does not decide it.
func TestApproverIsProposer(t *testing.T) {
	ns := namespace(t, "ap")
	agent := newActor(t, ns, "", nil, approverRules)

	t.Run("the recorded proposer approves its own proposal", func(t *testing.T) {
		ra := pendingAction(t, ns, agent.name)
		requireDenied(t, admissionenv.Approve(ctxFor(t), agent.c, ra, agent.name), "ApproverIsProposer")
	})
	t.Run("and may not reject it either", func(t *testing.T) {
		ra := pendingAction(t, ns, agent.name)
		requireDenied(t, admissionenv.Reject(ctxFor(t), agent.c, ra, agent.name), "ApproverIsProposer")
	})
	t.Run("someone else may", func(t *testing.T) {
		ra := pendingAction(t, ns, agent.name)
		bob := newActor(t, ns, "", nil, approverRules)
		require.NoError(t, admissionenv.Approve(ctxFor(t), bob.c, ra, bob.name))
	})
	t.Run("an action from before provenance counts as proposed by the operator", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		// A solo team's person approves Dorgu's own proposal (P2-D3).
		require.NoError(t, admissionenv.Approve(ctxFor(t), agent.c, ra, agent.name))
	})
}

// MissingApproveVerb: the status-patch right alone is not the right to decide.
func TestMissingApproveVerb(t *testing.T) {
	ns := namespace(t, "mv")

	t.Run("approve without the approve verb", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		bob := newActor(t, ns, "", nil, statusOnlyRules)
		requireDenied(t, admissionenv.Approve(ctxFor(t), bob.c, ra, bob.name), "MissingApproveVerb")
	})
	t.Run("reject needs the reject verb, not the approve verb", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		bob := newActor(t, ns, "", nil, approveOnlyRules)
		requireDenied(t, admissionenv.Reject(ctxFor(t), bob.c, ra, bob.name), "MissingApproveVerb")
		require.NoError(t, admissionenv.Approve(ctxFor(t), bob.c, ra, bob.name), "and the approve verb approves")
	})
	t.Run("the verb in another namespace does not count", func(t *testing.T) {
		other := namespace(t, "mv-other")
		ra := pendingAction(t, ns, "")
		bob := newActor(t, other, "", nil, approverRules)
		// bob can patch no status here at all: the API server refuses before the policy.
		err := admissionenv.Approve(ctxFor(t), bob.c, ra, bob.name)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "dorgu: ", "refused by RBAC before any policy runs")
	})
}

// InvalidTransition: a person makes one transition.
func TestInvalidTransition(t *testing.T) {
	ns := namespace(t, "it")
	alice := newActor(t, ns, "", nil, approverRules)

	t.Run("Pending to Completed", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		requireDenied(t, setPhase(alice, ra, "Completed"), "InvalidTransition")
	})
	t.Run("Pending to Ticketed", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		requireDenied(t, setPhase(alice, ra, "Ticketed"), "InvalidTransition")
	})
	t.Run("Approved to Rejected", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))
		bob := newActor(t, ns, "", nil, approverRules)
		requireDenied(t, admissionenv.Reject(ctxFor(t), bob.c, ra, bob.name), "InvalidTransition")
	})
	t.Run("Approved back to Pending", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))
		requireDenied(t, setPhase(alice, ra, "Pending"), "InvalidTransition")
	})
	t.Run("a second approval over the first", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))
		bob := newActor(t, ns, "", nil, approverRules)
		requireDenied(t, admissionenv.Approve(ctxFor(t), bob.c, ra, bob.name), "InvalidTransition")
	})
	t.Run("rewriting who approved without changing the phase", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))
		bob := newActor(t, ns, "", nil, approverRules)
		err := admissionenv.PatchStatus(ctxFor(t), bob.c, ra, map[string]any{"approvedBy": bob.name})
		requireDenied(t, err, "InvalidTransition")
	})
	t.Run("an action with no phase yet is not decided by a person", func(t *testing.T) {
		ra := admissionenv.NewAction(ns, uniq("ra"), "")
		require.NoError(t, env.Operator.Create(ctxFor(t), ra))
		requireDenied(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name), "InvalidTransition")
	})
}

// What a person may do is allowed: the CLI's approve, reject and heal record.
func TestTheCLIsWritesAreAllowed(t *testing.T) {
	ns := namespace(t, "ok")
	alice := newActor(t, ns, "", nil, approverRules)

	t.Run("approve, then the operator moves it on", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))

		fresh := &dorguv1.RemediationAction{}
		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(ra), fresh))
		assert.Equal(t, alice.name, fresh.Status.ApprovedBy)
		fresh.Status.Phase = phaseTicketed
		require.NoError(t, env.Operator.Status().Update(ctxFor(t), fresh),
			"the operator's own transitions are not the human's")
	})

	t.Run("reject resending every condition as read, and adding only its own", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		stored := &dorguv1.RemediationAction{}
		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(ra), stored))
		conditions := append(conditionMaps(stored.Status.Conditions), map[string]any{
			"type": "RejectionReason", "status": "True", "reason": "NotNeeded", "message": "not needed",
			"lastTransitionTime": nowText(),
		})
		require.NoError(t, admissionenv.PatchStatus(ctxFor(t), alice.c, ra, map[string]any{
			"phase": "Rejected", "approvedBy": alice.name, "conditions": conditions,
		}))
	})

	t.Run("the heal record on an approved action touches only its condition", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))
		stored := &dorguv1.RemediationAction{}
		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(ra), stored))
		conditions := append(conditionMaps(stored.Status.Conditions), map[string]any{
			"type": "WorkloadPatched", "status": "True", "reason": "WorkloadPatched", "message": "patched",
			"lastTransitionTime": nowText(),
		})
		require.NoError(t, admissionenv.PatchStatus(ctxFor(t), alice.c, ra, map[string]any{"conditions": conditions}))
	})

	t.Run("the heal record on a Pending action is the implicit approval", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		stored := &dorguv1.RemediationAction{}
		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(ra), stored))
		conditions := append(conditionMaps(stored.Status.Conditions), map[string]any{
			"type": "WorkloadPatched", "status": "True", "reason": "WorkloadPatched", "message": "patched",
			"lastTransitionTime": nowText(),
		})
		require.NoError(t, admissionenv.PatchStatus(ctxFor(t), alice.c, ra, map[string]any{
			"phase": "Approved", "approvedBy": alice.name, "approvedAt": nowText(),
			"conditions": conditions,
		}))
	})

	// The operator's records are nested objects (a gate decision with verdicts and a schemaless
	// pre-patch state, a ticket with a patch, per-step progress). A person's write that
	// resends them as they were must compare equal, or the heal record would be refused.
	t.Run("a heal record on an action with a populated operator status", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))

		stored := &dorguv1.RemediationAction{}
		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(ra), stored))
		now := metav1.Now()
		stored.Status.Phase = phaseTicketed
		stored.Status.CurrentStep = 1
		stored.Status.StepStatuses = []dorguv1.StepStatus{{Order: 1, Phase: "Pending"}}
		stored.Status.Gate = &dorguv1.GateStatus{
			Phase: "ApprovalRecheck", Result: "Pass", EvaluatedAt: now,
			Verdicts: []dorguv1.GateVerdict{{
				Stage: "Guardrails", Guardrail: "blast-radius", Verdict: "pass", Reason: "within the cap",
				Numbers: map[string]string{"ratio": "2"},
			}},
			PrePatchState: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)},
		}
		stored.Status.ApplyTicket = &dorguv1.ApplyTicket{
			Patch:    &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
			SpecHash: "ab", ApprovedBy: alice.name, IssuedAt: &now, ExpiresAt: metav1.NewTime(now.Add(15 * 60 * 1e9)),
		}
		require.NoError(t, env.Operator.Status().Update(ctxFor(t), stored))

		require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(ra), stored))
		conditions := append(conditionMaps(stored.Status.Conditions), map[string]any{
			"type": "WorkloadPatched", "status": "True", "reason": "WorkloadPatched", "message": "patched",
			"lastTransitionTime": nowText(),
		})
		require.NoError(t, admissionenv.PatchStatus(ctxFor(t), alice.c, ra, map[string]any{"conditions": conditions}))

		// And a change to the same records, however small, is not.
		requireDenied(t, admissionenv.PatchStatus(ctxFor(t), alice.c, ra, map[string]any{"gate": map[string]any{
			"verdicts": []map[string]any{{
				"stage": "Guardrails", "guardrail": "blast-radius", "verdict": "pass", "reason": "forged",
			}},
		}}), "OperatorOnly")
	})

	t.Run("a status write that changes nothing a person owns", func(t *testing.T) {
		ra := pendingAction(t, ns, "")
		require.NoError(t, admissionenv.PatchStatus(ctxFor(t), alice.c, ra, map[string]any{"phase": "Pending"}))
	})
}

// The largest plan the CRD allows is updated by the operator and read by the policies
// within the API server's CEL cost budget: a policy that cannot evaluate denies (Fail).
func TestAMaximumSizePlanStillPassesThePolicies(t *testing.T) {
	ns := namespace(t, "big")
	ra := admissionenv.NewAction(ns, uniq("ra"), "alice")
	ra.Spec.Steps = nil
	for i := range 64 {
		ra.Spec.Steps = append(ra.Spec.Steps, dorguv1.RemediationStep{
			Order: int32(i + 1), ID: "step-" + uniq("s"), Type: dorguv1.StepTypeManual,
			Description: "a step that says a good deal about what to look at, repeated to make the object large",
			Rationale:   "because the object is large",
		})
	}
	require.NoError(t, env.MakePending(ctxFor(t), ra))
	alice := newActor(t, ns, "", nil, approverRules)
	require.NoError(t, admissionenv.Approve(ctxFor(t), alice.c, ra, alice.name))
	stored := &dorguv1.RemediationAction{}
	require.NoError(t, env.Operator.Get(ctxFor(t), client.ObjectKeyFromObject(ra), stored))
	stored.Status.Phase = phaseTicketed
	require.NoError(t, env.Operator.Status().Update(ctxFor(t), stored))
}

// A request in a namespace outside Dorgu's concern is none of the policies' business.
func TestOtherResourcesAreUntouched(t *testing.T) {
	ns := namespace(t, "other")
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "x", Namespace: ns, Annotations: map[string]string{"dorgu.io/answers-signal": "x"},
	}}
	require.NoError(t, env.Admin.Create(ctxFor(t), cm))
}

func setPhase(a actor, ra *dorguv1.RemediationAction, phase string) error {
	return admissionenv.PatchStatus(context.Background(), a.c, ra, map[string]any{"phase": phase})
}

func nowText() string { return metav1.Now().UTC().Format("2006-01-02T15:04:05Z") }

func newIncident(ns string) *dorguv1.IncidentMemory {
	return &dorguv1.IncidentMemory{ObjectMeta: metav1.ObjectMeta{Name: uniq("im"), Namespace: ns}, Spec: incidentSpec(ns)}
}

const phaseTicketed = "Ticketed"

// conditionMaps renders conditions as a status patch would carry them back, as read.
func conditionMaps(in []metav1.Condition) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, c := range in {
		m := map[string]any{
			"type": c.Type, "status": string(c.Status), "reason": c.Reason,
			"lastTransitionTime": c.LastTransitionTime.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if c.Message != "" {
			m["message"] = c.Message
		} else {
			m["message"] = ""
		}
		if c.ObservedGeneration != 0 {
			m["observedGeneration"] = c.ObservedGeneration
		}
		out = append(out, m)
	}
	return out
}
