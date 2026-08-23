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

package detection

import (
	"fmt"
	"sort"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// MetadataKeyDeployment is the metadata key under which pod signals record the
// Deployment that owns the pod.
const MetadataKeyDeployment = "deployment"

// clusterGroupKey is the group key for signals that belong to no namespace.
const clusterGroupKey = "cluster"

// SignalScope names the kind of subject a group of signals is about.
type SignalScope string

const (
	// ScopePersona means every signal in the group was attributed to the same
	// persona in the same namespace.
	ScopePersona SignalScope = "persona"

	// ScopeUnattributed means the signals come from one workload in one
	// namespace that no single persona claims. The workload is real and its
	// signals are real; only the owner is unknown.
	ScopeUnattributed SignalScope = "unattributed"

	// ScopeCluster means the signals are cluster-scoped (nodes, control plane)
	// and belong to no namespace or workload.
	ScopeCluster SignalScope = "cluster"
)

// SignalGroup is the set of signals that may be reasoned about as one finding.
//
// Nothing outside a group is visible to a diagnosis of it. That boundary is the
// whole point: it is what stops a rule from describing four applications as one
// incident.
type SignalGroup struct {
	// Scope names what the group is about.
	Scope SignalScope

	// Key identifies the group, and orders groups deterministically.
	Key string

	// Namespace is the single namespace the group belongs to, empty for
	// ScopeCluster.
	Namespace string

	// PersonaRef is the persona every signal in the group is attributed to.
	// Set only for ScopePersona.
	PersonaRef *dorguv1.PersonaReference

	// Workload names the workload the signals came from. Set only for
	// ScopeUnattributed.
	Workload string

	// Signals are the group's signals, in the order they were collected.
	Signals []Signal
}

// GroupSignals partitions signals so that no group ever spans two applications
// or two namespaces.
//
// Diagnosis used to run once over every signal in the cluster. Its rules take
// "all the OOMKilled signals" as a single finding, name the first persona they
// happen to see as the owner, and list every OOM-killed pod as affected. On a
// cluster where four unrelated apps were failing at once, that produced one
// IncidentMemory holding pods from three namespaces, and the planner, handed a
// bundle spanning three namespaces, concluded the nodes were under memory
// pressure. The nodes were at 23% (F-02).
//
// Grouping first is what makes an incident about one application: a rule can
// only see one application's signals, so it can only describe one application.
// A signal no single persona claims lands in its own unattributed group rather
// than joining a neighbour's, because an honest "something here is broken and I
// do not know whose it is" beats a confident attribution to the wrong app.
func GroupSignals(signals []Signal) []SignalGroup {
	groups := make(map[string]*SignalGroup, len(signals))
	keys := make([]string, 0, len(signals))

	for i := range signals {
		sig := signals[i]
		group := groupFor(&sig)

		existing, ok := groups[group.Key]
		if !ok {
			keys = append(keys, group.Key)
			groups[group.Key] = &group
			existing = &group
		}
		existing.Signals = append(existing.Signals, sig)
	}

	sort.Strings(keys)

	result := make([]SignalGroup, 0, len(keys))
	for _, key := range keys {
		result = append(result, *groups[key])
	}
	return result
}

// groupFor decides which group a single signal belongs to.
func groupFor(sig *Signal) SignalGroup {
	if sig.PersonaRef != nil {
		ref := *sig.PersonaRef
		return SignalGroup{
			Scope:      ScopePersona,
			Key:        fmt.Sprintf("persona/%s/%s/%s", ref.Kind, ref.Namespace, ref.Name),
			Namespace:  ref.Namespace,
			PersonaRef: &ref,
		}
	}

	namespace := sig.Resource.Namespace
	if namespace == "" {
		return SignalGroup{Scope: ScopeCluster, Key: clusterGroupKey}
	}

	workload := WorkloadName(sig)
	return SignalGroup{
		Scope:     ScopeUnattributed,
		Key:       fmt.Sprintf("unattributed/%s/%s", namespace, workload),
		Namespace: namespace,
		Workload:  workload,
	}
}

// WorkloadName names the workload a signal came from: the owning Deployment
// when the detector recorded one, and otherwise the resource itself. Pod names
// carry a per-replica suffix, so grouping on them alone would file every
// restart of the same app as a separate workload.
func WorkloadName(sig *Signal) string {
	if deployment := sig.Metadata[MetadataKeyDeployment]; deployment != "" {
		return deployment
	}
	return sig.Resource.Name
}
