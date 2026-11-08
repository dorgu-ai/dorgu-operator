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

package diagnosis

import (
	"context"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// NamespaceGate decides whether a namespace is protected, meaning nothing about
// it may be sent to a model. It is the deny-list the remediation safety checker
// already enforces, passed in rather than copied so there is one decision. The
// interface lives here because remediation imports this package.
type NamespaceGate interface {
	// NamespaceProtected reports whether namespace is protected. It must fail
	// closed: an empty namespace, or a deny-list that cannot be read, is protected.
	NamespaceProtected(ctx context.Context, namespace string) bool
}

// ProtectedNamespace reports whether d may not be sent to a model, and the
// namespace that decided it. It is the one enumeration of the namespaces a
// diagnosis touches, used by every path that sends a diagnosis out (the AI
// diagnosis provider and the AI remediation planner): its persona, its affected
// resources, and the resource behind every contributing signal. A diagnosis
// merges signals across pods, so one protected namespace anywhere withholds the
// whole diagnosis.
//
// It fails closed: a nil gate, a persona or namespaced resource with no
// namespace, or a gate that says so, all withhold the diagnosis. Only the
// cluster-scoped kinds (detection.IsClusterScopedKind) may have an empty namespace.
func ProtectedNamespace(ctx context.Context, gate NamespaceGate, d Diagnosis) (string, bool) {
	if gate == nil {
		return "", true
	}

	namespaces := make([]string, 0, len(d.AffectedResources)+len(d.Contributing)+1)
	if d.PersonaRef != nil {
		namespaces = append(namespaces, d.PersonaRef.Namespace)
	}
	refs := make([]dorguv1.ResourceReference, 0, len(d.AffectedResources)+len(d.Contributing))
	refs = append(refs, d.AffectedResources...)
	for _, cs := range d.Contributing {
		refs = append(refs, cs.Signal.Resource)
	}
	for _, ref := range refs {
		if ref.Namespace == "" && detection.IsClusterScopedKind(ref.Kind) {
			continue
		}
		namespaces = append(namespaces, ref.Namespace)
	}

	checked := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		if checked[ns] {
			continue
		}
		checked[ns] = true
		if ns == "" || gate.NamespaceProtected(ctx, ns) {
			return ns, true
		}
	}
	return "", false
}
