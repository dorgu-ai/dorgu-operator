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
	"fmt"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The operator reads its own admission policies and bindings, by name, to be sure they
// are there. It needs get on exactly these six objects and no more.
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicies,resourceNames=dorgu-proposal-identity;dorgu-action-integrity;dorgu-approval-gate,verbs=get
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicybindings,resourceNames=dorgu-proposal-identity;dorgu-action-integrity;dorgu-approval-gate,verbs=get
// The preflight asks the API server whether the operator holds the RBAC it needs. Every
// authenticated user may; this grants it explicitly for a cluster whose roles say otherwise.
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=selfsubjectaccessreviews,verbs=create

const (
	// AdmissionPolicyVersionAnnotation is the annotation the chart puts on every policy:
	// the version of its rules. A policy from another release is outdated.
	AdmissionPolicyVersionAnnotation = "dorgu.io/admission-policy-version"

	// AdmissionPolicyVersion is the version of the policies this operator expects. Bump it,
	// and the chart's template, in the same change that changes a rule.
	AdmissionPolicyVersion = "1"

	// ReasonAdmissionPoliciesMissing: a policy or binding the chart installs is not there.
	// Without them nobody checks who approves, so the operator converts no proposal and
	// issues no ticket.
	ReasonAdmissionPoliciesMissing = "AdmissionPoliciesMissing"

	// ReasonAdmissionPoliciesWeakened: the policies are there but would not deny (they fail
	// open, a binding only warns or audits, points elsewhere, or is narrowed).
	ReasonAdmissionPoliciesWeakened = "AdmissionPoliciesWeakened"

	// ReasonAdmissionPoliciesOutdated: the policies are from another release of the chart.
	ReasonAdmissionPoliciesOutdated = "AdmissionPoliciesOutdated"

	// ReasonAdmissionPoliciesUnverified: the operator could not read the policies, so it
	// cannot tell whether they are there.
	ReasonAdmissionPoliciesUnverified = "AdmissionPoliciesUnverified"

	// ReasonOperatorRBACMissing: the operator lacks a permission it needs.
	ReasonOperatorRBACMissing = "OperatorRBACMissing"

	// ReasonOperatorRBACUnverified: the operator could not ask whether it holds its
	// permissions.
	ReasonOperatorRBACUnverified = "OperatorRBACUnverified"
)

// AdmissionPolicyNames are the three policies, in the order the chart writes them. Each
// has a binding of the same name.
var AdmissionPolicyNames = []string{"dorgu-proposal-identity", "dorgu-action-integrity", "dorgu-approval-gate"}

// ticketBlockers are the reasons that stop the operator issuing apply tickets as well as
// converting proposals: the approval it would be issuing a ticket for is only as good as
// the admission policies that checked who gave it.
var ticketBlockers = map[string]bool{
	ReasonAdmissionPoliciesMissing:    true,
	ReasonAdmissionPoliciesWeakened:   true,
	ReasonAdmissionPoliciesOutdated:   true,
	ReasonAdmissionPoliciesUnverified: true,
	ReasonOperatorRBACMissing:         true,
	ReasonOperatorRBACUnverified:      true,
}

// BlocksTickets reports whether a failure with this reason stops apply tickets as well as
// the intake.
func BlocksTickets(reason string) bool { return ticketBlockers[reason] }

// TicketsBlocked returns the first failing check that stops tickets being issued, or nil.
// Before the first run it returns the pending failure: nothing is known, so nothing is
// issued. A nil Preflight (the health pipeline is off) blocks nothing.
func (p *Preflight) TicketsBlocked() *Failure {
	if p == nil {
		return nil
	}
	for _, f := range p.Failures() {
		if f.Reason == ReasonPreflightPending || ticketBlockers[f.Reason] {
			return &f
		}
	}
	return nil
}

// UsernameSource says who the operator is, for a check that compares what the cluster holds
// with it. It is the API server's answer (a SelfSubjectReview), not a claim.
type UsernameSource func(ctx context.Context) (string, error)

// AdmissionPoliciesCheck is the preflight check that the admission policies and their
// bindings exist and say what this release of the chart says (C2 review F-3): failurePolicy
// Fail (or unset, which is Fail), every binding on its own policy with the Deny action and
// no narrowing, the policy version this operator expects, and the content itself. Each
// policy must trust the operator's verified username (operator) as "the operator", and its
// matchConstraints, matchConditions, variables and validations, and the binding's actions,
// policy, parameters and matches, must hash the same as the embedded copy of the chart's
// rendered policies for that username (ReferencePolicies). A policy that was edited,
// truncated or mis-templated is AdmissionPoliciesWeakened; the version annotation is only a
// hint. reader should be uncached.
//
// It is run every minute, and again just before every apply ticket, so policies changed
// after startup close the intake and stop tickets (P2-D8).
func AdmissionPoliciesCheck(reader client.Reader, operator UsernameSource) Check {
	return Check{Name: "admission-policies", Reason: ReasonAdmissionPoliciesMissing, Run: func(ctx context.Context) error {
		username, err := verifiedUsername(ctx, operator)
		if err != nil {
			return &reasonedError{ReasonAdmissionPoliciesUnverified, err}
		}
		referencePolicies, referenceBindings, err := ReferencePolicies(username)
		if err != nil {
			return &reasonedError{ReasonAdmissionPoliciesUnverified, err}
		}

		var missing, weak, outdated []string
		for _, name := range AdmissionPolicyNames {
			var policy admissionregistrationv1.ValidatingAdmissionPolicy
			switch err := reader.Get(ctx, client.ObjectKey{Name: name}, &policy); {
			case apierrors.IsNotFound(err):
				missing = append(missing, "policy "+name)
			case err != nil:
				return unverified(ReasonAdmissionPoliciesUnverified, "validatingadmissionpolicies", name, err)
			default:
				if policy.Spec.FailurePolicy != nil && *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
					weak = append(weak, fmt.Sprintf("policy %s has failurePolicy %s, not Fail", name, *policy.Spec.FailurePolicy))
				}
				if got := policy.Annotations[AdmissionPolicyVersionAnnotation]; got != AdmissionPolicyVersion {
					outdated = append(outdated, fmt.Sprintf("policy %s is version %q, this operator expects %q", name, got, AdmissionPolicyVersion))
				} else if ref := findPolicy(referencePolicies, name); ref != nil {
					// The annotation is a hint; the content is the proof.
					weak = append(weak, policyContentProblems(&policy, ref, username)...)
				}
			}

			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			switch err := reader.Get(ctx, client.ObjectKey{Name: name}, &binding); {
			case apierrors.IsNotFound(err):
				missing = append(missing, "binding "+name)
			case err != nil:
				return unverified(ReasonAdmissionPoliciesUnverified, "validatingadmissionpolicybindings", name, err)
			default:
				weak = append(weak, bindingProblems(name, &binding)...)
				if ref := findBinding(referenceBindings, name); ref != nil {
					weak = append(weak, bindingContentProblems(&binding, ref)...)
				}
			}
		}

		switch {
		case len(missing) > 0:
			return &reasonedError{ReasonAdmissionPoliciesMissing, fmt.Errorf(
				"the admission policies that check who approves a remediation are not all installed (missing: %s): install or upgrade the chart "+
					"(helm upgrade installs them), or for a kustomize install apply config/admission/policies.yaml. "+
					"Until then no proposal is converted and no apply ticket is issued", strings.Join(missing, ", "))}
		case len(weak) > 0:
			return &reasonedError{ReasonAdmissionPoliciesWeakened, fmt.Errorf(
				"the admission policies would not deny (%s): restore them from the chart", strings.Join(dedupe(weak), "; "))}
		case len(outdated) > 0:
			return &reasonedError{ReasonAdmissionPoliciesOutdated, fmt.Errorf(
				"the admission policies are from another release (%s): run helm upgrade, or re-apply config/admission/policies.yaml from this release",
				strings.Join(outdated, "; "))}
		}
		return nil
	}}
}

// verifiedUsername asks source who the operator is. It never guesses: no source, an error or
// an empty name is a reason the content cannot be checked.
func verifiedUsername(ctx context.Context, source UsernameSource) (string, error) {
	if source == nil {
		return "", errors.New("the operator cannot say who it is, so the admission policies cannot be compared with what it expects")
	}
	username, err := source(ctx)
	switch {
	case err != nil:
		return "", fmt.Errorf("the operator could not confirm who it is, so the admission policies cannot be compared with what it expects: %w", err)
	case username == "":
		return "", errors.New("the operator's identity is empty, so the admission policies cannot be compared with what it expects")
	}
	return username, nil
}

func findPolicy(list []admissionregistrationv1.ValidatingAdmissionPolicy, name string) *admissionregistrationv1.ValidatingAdmissionPolicy {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func findBinding(list []admissionregistrationv1.ValidatingAdmissionPolicyBinding, name string) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// dedupe drops repeated problems, keeping the order.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// bindingProblems says what keeps a binding from denying.
func bindingProblems(name string, b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) []string {
	var problems []string
	if b.Spec.PolicyName != name {
		problems = append(problems, fmt.Sprintf("binding %s points at policy %q", name, b.Spec.PolicyName))
	}
	denies := false
	for _, action := range b.Spec.ValidationActions {
		denies = denies || action == admissionregistrationv1.Deny
	}
	if !denies {
		problems = append(problems, fmt.Sprintf("binding %s does not have the Deny action", name))
	}
	if m := b.Spec.MatchResources; m != nil && (m.NamespaceSelector != nil || m.ObjectSelector != nil ||
		len(m.ResourceRules) > 0 || len(m.ExcludeResourceRules) > 0) {
		problems = append(problems, fmt.Sprintf("binding %s narrows what the policy matches", name))
	}
	return problems
}

// unverified is a check failure for an API read that failed: the operator cannot tell, so
// it says what it needed and does not guess.
func unverified(reason, resource, name string, err error) error {
	return &reasonedError{reason, fmt.Errorf("could not read %s %q (the operator needs get on it): %w", resource, name, err)}
}

// AccessReviewer asks the API server whether the operator itself may do something: a
// SelfSubjectAccessReview. It returns whether it is allowed and the API server's reason.
type AccessReviewer func(ctx context.Context, attrs authzv1.ResourceAttributes) (allowed bool, reason string, err error)

// AccessReviewerFromClientset builds an AccessReviewer from a clientset's authorization
// API.
func AccessReviewerFromClientset(cs kubernetes.Interface) AccessReviewer {
	return func(ctx context.Context, attrs authzv1.ResourceAttributes) (bool, string, error) {
		review, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authzv1.SelfSubjectAccessReview{
			Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs},
		}, metav1.CreateOptions{})
		if err != nil {
			return false, "", err
		}
		return review.Status.Allowed, review.Status.Reason, nil
	}
}

// Access is one thing the operator must be allowed to do, cluster-wide: every verb on
// the resource (and subresource), on each named object when Names is set.
type Access struct {
	Group, Resource, Subresource string
	Names                        []string
	Verbs                        []string
}

var dorguGroup = "dorgu.io"

// OperatorAccess is what the intake and the approval path need the operator to hold. It is
// not the whole ClusterRole (the controllers' other needs are theirs to report); it is what
// the proposal intake, the admission-policy check and ticket issue cannot work without, so
// that a partial hand-written role closes the intake with a message instead of a cache
// that never syncs.
func OperatorAccess() []Access {
	var needs []Access
	add := func(a Access) { needs = append(needs, a) }
	add(Access{Group: dorguGroup, Resource: "remediationproposals", Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}})
	add(Access{Group: dorguGroup, Resource: "remediationproposals", Subresource: "status", Verbs: []string{"get", "update", "patch"}})
	add(Access{Group: dorguGroup, Resource: "remediationactions", Verbs: []string{"get", "list", "watch", "create", "delete"}})
	add(Access{Group: dorguGroup, Resource: "remediationactions", Subresource: "status", Verbs: []string{"get", "update", "patch"}})
	add(Access{Group: dorguGroup, Resource: "incidentmemories", Verbs: []string{"get", "list", "watch", "create", "update", "patch"}})
	add(Access{Group: dorguGroup, Resource: "incidentmemories", Subresource: "status", Verbs: []string{"get", "update", "patch"}})
	add(Access{Group: "authentication.k8s.io", Resource: "selfsubjectreviews", Verbs: []string{"create"}})
	add(Access{Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies", Names: AdmissionPolicyNames, Verbs: []string{"get"}})
	add(Access{Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicybindings", Names: AdmissionPolicyNames, Verbs: []string{"get"}})
	return needs
}

// maxGapsListed bounds how many missing permissions a failure message names.
const maxGapsListed = 8

// OperatorRBACCheck is the preflight check that the operator holds the permissions the
// intake needs, asked of the API server itself (SelfSubjectAccessReview): C1 re-check N-4.
// A permission it lacks closes the intake with a message naming it; a review that cannot
// be made is a failure too, never a pass.
func OperatorRBACCheck(review AccessReviewer) Check {
	return Check{Name: "operator-rbac", Reason: ReasonOperatorRBACMissing, PeriodicOnly: true, Run: func(ctx context.Context) error {
		var gaps []string
		for _, need := range OperatorAccess() {
			names := need.Names
			if len(names) == 0 {
				names = []string{""}
			}
			for _, name := range names {
				for _, verb := range need.Verbs {
					attrs := authzv1.ResourceAttributes{
						Verb: verb, Group: need.Group, Resource: need.Resource, Subresource: need.Subresource, Name: name,
					}
					allowed, _, err := review(ctx, attrs)
					if err != nil {
						return &reasonedError{ReasonOperatorRBACUnverified, fmt.Errorf(
							"could not check the operator's own permissions with a SelfSubjectAccessReview: %w", err)}
					}
					if !allowed {
						gaps = append(gaps, describe(attrs))
					}
				}
			}
		}
		if len(gaps) == 0 {
			return nil
		}
		shown := gaps
		more := ""
		if len(shown) > maxGapsListed {
			shown, more = shown[:maxGapsListed], fmt.Sprintf(" and %d more", len(gaps)-maxGapsListed)
		}
		return &reasonedError{ReasonOperatorRBACMissing, errors.New(
			"the operator is missing permissions it needs (" + strings.Join(shown, ", ") + more + "): " +
				"apply the ClusterRole from this release; the chart and config/rbac grant them")}
	}}
}

// describe renders an access as "verb resource.group/subresource" (or "verb resource.group
// NAME" for one named object).
func describe(a authzv1.ResourceAttributes) string {
	out := a.Verb + " " + a.Resource
	if a.Group != "" {
		out += "." + a.Group
	}
	if a.Subresource != "" {
		out += "/" + a.Subresource
	}
	if a.Name != "" {
		out += " " + a.Name
	}
	return out
}
