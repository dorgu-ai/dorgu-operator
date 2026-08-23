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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// oomSignal is one OOMKilled pod, optionally attributed to a persona.
func oomSignal(namespace, pod, persona string) Signal {
	sig := Signal{
		Type:     SignalOOMKilled,
		Severity: SeverityCritical,
		Category: CategoryResource,
		Source:   podCollectorName,
		Resource: dorguv1.ResourceReference{Kind: "Pod", Name: pod, Namespace: namespace},
	}
	if persona != "" {
		sig.PersonaRef = &dorguv1.PersonaReference{
			Kind:      "ApplicationPersona",
			Name:      persona,
			Namespace: namespace,
		}
	}
	return sig
}

// TestGroupSignals_F02_NeverMixesApplications reproduces F-02. The clean-room
// run had four unrelated apps OOM-killing at once across three namespaces, and
// every one of their pods landed in a single IncidentMemory. Grouping is the
// boundary that makes that impossible.
func TestGroupSignals_F02_NeverMixesApplications(t *testing.T) {
	signals := []Signal{
		oomSignal("apps", "report-worker-788c95d9bc-p9pkz", "report-worker"),
		oomSignal("apps", "frontend-podinfo-b4867d9b4-cddt2", "frontend-podinfo"),
		oomSignal("web", "edge-nginx-7b98cd89d4-rk8hf", "edge-nginx"),
		oomSignal("platform", "checkout-57c95bf9b8-47vp9", "checkout"),
	}

	groups := GroupSignals(signals)

	require.Len(t, groups, 4, "four applications must produce four groups, not one bundle")
	for _, g := range groups {
		assert.Equal(t, ScopePersona, g.Scope)
		require.Len(t, g.Signals, 1)
		assert.Equal(t, g.PersonaRef.Namespace, g.Signals[0].Resource.Namespace,
			"a group must never span namespaces")
		assert.True(t, NameClaimedByPersona(g.Signals[0].Resource.Name, g.PersonaRef.Name),
			"pod %s must not be filed under persona %s",
			g.Signals[0].Resource.Name, g.PersonaRef.Name)
	}
}

// TestGroupSignals_SamePersonaNameInTwoNamespaces pins the namespace half of
// the rule: two apps that happen to share a name are still two apps.
func TestGroupSignals_SamePersonaNameInTwoNamespaces(t *testing.T) {
	groups := GroupSignals([]Signal{
		oomSignal("staging", "api-1", "api"),
		oomSignal("prod", "api-1", "api"),
	})

	require.Len(t, groups, 2)
	namespaces := []string{groups[0].Namespace, groups[1].Namespace}
	assert.ElementsMatch(t, []string{"prod", "staging"}, namespaces)
}

// TestGroupSignals_UnattributedStaysItsOwnGroup covers the "prefer unattributed
// over wrong" rule: an unclaimed pod gets its own group instead of being folded
// into the app next to it.
func TestGroupSignals_UnattributedStaysItsOwnGroup(t *testing.T) {
	orphan := oomSignal("apps", "mystery-7c9-abc", "")
	orphan.Metadata = map[string]string{MetadataKeyDeployment: "mystery"}

	groups := GroupSignals([]Signal{
		oomSignal("apps", "report-worker-788c95d9bc-p9pkz", "report-worker"),
		orphan,
	})

	require.Len(t, groups, 2)

	byScope := map[SignalScope]SignalGroup{}
	for _, g := range groups {
		byScope[g.Scope] = g
	}

	unattributed, ok := byScope[ScopeUnattributed]
	require.True(t, ok, "an unclaimed signal must produce an unattributed group")
	assert.Equal(t, "apps", unattributed.Namespace)
	assert.Equal(t, "mystery", unattributed.Workload, "grouping keys off the owning Deployment")
	assert.Nil(t, unattributed.PersonaRef)

	persona, ok := byScope[ScopePersona]
	require.True(t, ok)
	require.Len(t, persona.Signals, 1, "the unclaimed pod must not join the app's group")
}

// TestGroupSignals_ReplicasOfOneWorkloadShareAGroup keeps the unattributed path
// from filing one incident per pod: three replicas of one broken Deployment are
// one broken application.
func TestGroupSignals_ReplicasOfOneWorkloadShareAGroup(t *testing.T) {
	pods := []string{"edge-1", "edge-2", "edge-3"}
	signals := make([]Signal, 0, len(pods))
	for _, pod := range pods {
		sig := oomSignal("web", pod, "")
		sig.Metadata = map[string]string{MetadataKeyDeployment: "edge"}
		signals = append(signals, sig)
	}

	groups := GroupSignals(signals)

	require.Len(t, groups, 1)
	assert.Equal(t, ScopeUnattributed, groups[0].Scope)
	assert.Len(t, groups[0].Signals, 3)
}

// TestGroupSignals_ClusterScopedSignalsGroupTogether keeps node and control
// plane findings out of every application's group.
func TestGroupSignals_ClusterScopedSignalsGroupTogether(t *testing.T) {
	groups := GroupSignals([]Signal{
		{
			Type:     SignalNodeMemoryPressure,
			Severity: SeverityWarning,
			Category: CategoryNode,
			Resource: dorguv1.ResourceReference{Kind: "Node", Name: "ip-10-0-1-5"},
		},
		oomSignal("apps", "report-worker-1", "report-worker"),
	})

	require.Len(t, groups, 2)

	byScope := map[SignalScope]SignalGroup{}
	for _, g := range groups {
		byScope[g.Scope] = g
	}
	require.Contains(t, byScope, ScopeCluster)
	assert.Empty(t, byScope[ScopeCluster].Namespace)
	assert.Len(t, byScope[ScopePersona].Signals, 1)
}

// TestGroupSignals_IsDeterministic keeps incident identity stable across
// cycles: the same signals in a different collection order must group the same
// way, in the same order.
func TestGroupSignals_IsDeterministic(t *testing.T) {
	a := oomSignal("apps", "alpha-1", "alpha")
	b := oomSignal("web", "beta-1", "beta")

	first := GroupSignals([]Signal{a, b})
	second := GroupSignals([]Signal{b, a})

	require.Len(t, first, 2)
	require.Len(t, second, 2)
	assert.Equal(t, first[0].Key, second[0].Key)
	assert.Equal(t, first[1].Key, second[1].Key)
}

func TestGroupSignals_Empty(t *testing.T) {
	assert.Empty(t, GroupSignals(nil))
}

func TestWorkloadName(t *testing.T) {
	tests := []struct {
		name   string
		signal Signal
		want   string
	}{
		{
			name: "the owning Deployment wins",
			signal: Signal{
				Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "edge-7b9-x"},
				Metadata: map[string]string{MetadataKeyDeployment: "edge"},
			},
			want: "edge",
		},
		{
			name:   "no owner recorded falls back to the resource",
			signal: Signal{Resource: dorguv1.ResourceReference{Kind: "Pod", Name: "standalone"}},
			want:   "standalone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, WorkloadName(&tt.signal))
		})
	}
}
