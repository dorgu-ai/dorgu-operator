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

package gate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// NormalizeNamespace is the form namespaces are compared in: trimmed and
// lowercased. A namespace name is a DNS label, so " Shop" can only have meant
// "shop"; matching byte for byte made an entry with a stray space or a capital
// protect nothing while looking as if it did (R5).
func NormalizeNamespace(namespace string) string {
	return strings.ToLower(strings.TrimSpace(namespace))
}

// NamespaceProtected is NamespaceDenied as a yes/no, for callers that only need
// the verdict. It satisfies diagnosis.NamespaceGate, so the diagnosis provider
// asks this same deny list before any model call.
func (g *Gate) NamespaceProtected(ctx context.Context, namespace string) bool {
	return g.NamespaceDenied(ctx, namespace) != nil
}

// NamespaceDenied applies the deny list to a namespace on its own. It is the one
// place the deny list is decided: the Guardrails stage asks it, and the diagnosis
// provider and planner ask it, through NamespaceProtected, before any model call.
//
// It fails closed. A namespace that cannot be determined (empty), or an
// excludeNamespaces policy that cannot be read, is refused: guessing "allowed"
// would send a protected namespace's context to a model, or plan against it.
func (g *Gate) NamespaceDenied(ctx context.Context, namespace string) *Violation {
	// A nil gate (a typed nil inside an interface) has no deny list to consult,
	// which is the same as one that cannot be read.
	if g == nil {
		return &Violation{
			Rule:    RuleDenyList,
			Message: "no deny list is configured, so the namespace cannot be cleared",
		}
	}

	namespace = NormalizeNamespace(namespace)
	if namespace == "" {
		return &Violation{
			Rule:    RuleDenyList,
			Message: "namespace could not be determined, so the deny list cannot clear it",
		}
	}
	// The operator could not learn its own namespace, so the deny-list cannot say
	// which namespace is Dorgu's. Nothing is cleared until it can (R12).
	if g.operatorNamespaceUnknown {
		return &Violation{
			Rule:    RuleOperatorNamespace,
			Message: "the operator's own namespace is unknown, so no namespace can be cleared",
		}
	}

	denied, why, err := g.listed(ctx, namespace)
	if err != nil {
		// Logged loudly: while this persists (lost RBAC, missing CRD) every AI call
		// and proposal is refused, and that must not be silent.
		g.logUnreadable("excludeNamespaces", err, "cannot read ClusterPersona excludeNamespaces; refusing the namespace until the deny list is readable",
			"namespace", namespace)
		return &Violation{
			Rule: RuleDenyList,
			// Fixed text: the message becomes a SkipReason that may be persisted or
			// shown, and the raw API error stays in the log above.
			Message: fmt.Sprintf("namespace %s: deny-list could not be read, treating namespace as protected", namespace),
		}
	}
	if denied {
		return &Violation{Rule: RuleDenyList, Message: why}
	}
	return nil
}

// NamespaceExcluded asks the same deny list a different question: should
// detection leave this namespace alone? It is the list alone (kube-system, the
// operator's namespace, installer-protected namespaces and every ClusterPersona
// excludeNamespaces entry, all normalized the same way), read from the one place
// NamespaceDenied reads it.
//
// It differs from NamespaceDenied in two deliberate ways. An empty namespace is
// not excluded: cluster-scoped signals have none, and detection is not the place
// to refuse them. And an operator that does not know its own namespace does not
// blind detection: the gate still refuses every action in that state, so seeing
// an incident costs nothing, while hiding one costs the user the report.
//
// An unreadable policy is an error, not a verdict. Detection that cannot tell
// what it may skip must say so rather than guess either way; a collector that
// fails is what the verifier and the resolver already treat as "do not conclude".
func (g *Gate) NamespaceExcluded(ctx context.Context, namespace string) (bool, error) {
	if g == nil {
		return false, fmt.Errorf("no deny list is configured, so the namespace cannot be cleared")
	}
	namespace = NormalizeNamespace(namespace)
	if namespace == "" {
		return false, nil
	}
	denied, _, err := g.listed(ctx, namespace)
	if err != nil {
		g.logUnreadable("excludeNamespaces", err, "cannot read ClusterPersona excludeNamespaces; detection cannot tell which namespaces to skip",
			"namespace", namespace)
		return false, err
	}
	return denied, nil
}

// listed reports whether a normalized, non-empty namespace is on the deny list,
// and why. It is the one reading of the list: the protected set first, then the
// ClusterPersona policy.
func (g *Gate) listed(ctx context.Context, namespace string) (denied bool, why string, err error) {
	// Always deny kube-system, the operator's own namespace and any namespace the
	// installer protected.
	if slices.Contains(g.protected, namespace) {
		return true, fmt.Sprintf("namespace %s is in the protected deny list", namespace), nil
	}
	excluded, err := g.excludedNamespaces(ctx)
	if err != nil {
		return false, "", err
	}
	if slices.Contains(excluded, namespace) {
		return true, fmt.Sprintf("namespace %s is excluded by ClusterPersona policy", namespace), nil
	}
	return false, "", nil
}

// excludedNamespaces reads excluded namespaces from all ClusterPersonas.
func (g *Gate) excludedNamespaces(ctx context.Context) ([]string, error) {
	var clusterPersonas dorguv1.ClusterPersonaList
	if err := g.client.List(ctx, &clusterPersonas); err != nil {
		return nil, fmt.Errorf("listing ClusterPersonas: %w", err)
	}

	var excluded []string
	for i := range clusterPersonas.Items {
		cp := &clusterPersonas.Items[i]
		if cp.Spec.Policies != nil && cp.Spec.Policies.SelfHealing != nil {
			for _, ns := range cp.Spec.Policies.SelfHealing.ExcludeNamespaces {
				excluded = append(excluded, NormalizeNamespace(ns))
			}
		}
	}
	return excluded, nil
}
