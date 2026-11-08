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

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

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

	if namespace == "" {
		return &Violation{
			Rule:    RuleDenyList,
			Message: "namespace could not be determined, so the deny list cannot clear it",
		}
	}

	// Always deny kube-system, the operator's own namespace and any namespace the
	// installer protected.
	if slices.Contains(g.protected, namespace) {
		return &Violation{
			Rule:    RuleDenyList,
			Message: fmt.Sprintf("namespace %s is in the protected deny list", namespace),
		}
	}

	excluded, err := g.excludedNamespaces(ctx)
	if err != nil {
		// Logged loudly: while this persists (lost RBAC, missing CRD) every AI call
		// and proposal is refused, and that must not be silent.
		g.logger.Error(err, "cannot read ClusterPersona excludeNamespaces; refusing the namespace until the deny list is readable",
			"namespace", namespace)
		return &Violation{
			Rule: RuleDenyList,
			// Fixed text: the message becomes a SkipReason that may be persisted or
			// shown, and the raw API error stays in the log above.
			Message: fmt.Sprintf("namespace %s: deny-list could not be read, treating namespace as protected", namespace),
		}
	}

	if slices.Contains(excluded, namespace) {
		return &Violation{
			Rule:    RuleDenyList,
			Message: fmt.Sprintf("namespace %s is excluded by ClusterPersona policy", namespace),
		}
	}
	return nil
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
			excluded = append(excluded, cp.Spec.Policies.SelfHealing.ExcludeNamespaces...)
		}
	}
	return excluded, nil
}
