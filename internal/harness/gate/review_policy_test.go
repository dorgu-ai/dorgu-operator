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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// --- R6: unknown or ambiguous policy fails closed; most restrictive wins ----------

func modePersona(name, mode string, perHour int32) *dorguv1.ClusterPersona {
	cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) {
		p.Mode = mode
		p.MaxRemediationsPerHour = perHour
	})
	cp.Name = name
	return cp
}

func TestR6_MostRestrictiveModeWinsWhateverTheListOrder(t *testing.T) {
	for _, order := range [][]client.Object{
		{modePersona("a-propose", dorguv1.SelfHealingModePropose, 0), modePersona("b-observe", dorguv1.SelfHealingModeObserve, 0)},
		{modePersona("a-observe", dorguv1.SelfHealingModeObserve, 0), modePersona("b-propose", dorguv1.SelfHealingModePropose, 0)},
		{modePersona("a-auto", dorguv1.SelfHealingModeAutoApprove, 0), modePersona("b-observe", dorguv1.SelfHealingModeObserve, 0)},
	} {
		objs := append([]client.Object{persona("256Mi"), deployment("256Mi")}, order...)
		g := newTestGate(t, fakeClient(t, objs...))

		d := run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

		stage, guardrail := refusedBy(t, d)
		assert.Equal(t, StageModePolicy, stage)
		assert.Equal(t, GuardrailSelfHealingMode, guardrail)
	}
}

func TestR6_ModeListErrorFailsClosed(t *testing.T) {
	// The mode stage on its own, because the deny-list and rate limit would refuse
	// first on a full run and hide what ModePolicy does with an unreadable policy.
	g := newTestGate(t, failingListClient(t, isKind[*dorguv1.ClusterPersonaList]))
	cand := planCandidate(personaUpdateStep(memPatch("384Mi")))
	d := newDecision(&cand, PhaseProposal, metav1Now())

	require.NoError(t, modePolicyStage{g}.Run(context.Background(), &cand, d))

	stage, guardrail := refusedBy(t, d)
	assert.Equal(t, StageModePolicy, stage)
	assert.Equal(t, GuardrailPolicyUnreadable, guardrail)
}

func TestR6_UnrecognizedModeFailsClosed(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"),
		modePersona("odd", "yolo", 0)))

	_, guardrail := refusedBy(t, run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal))

	assert.Equal(t, GuardrailUnknownMode, guardrail)
}

func TestR6_LowestRateLimitWins(t *testing.T) {
	one := sibling("ra-prev", "Completed")
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), one,
		modePersona("a-loose", dorguv1.SelfHealingModePropose, 10),
		modePersona("b-tight", dorguv1.SelfHealingModePropose, 1)))

	_, guardrail := refusedBy(t, run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal))

	assert.Equal(t, GuardrailRateLimit, guardrail)
}

// --- R5: excludeNamespaces is matched on normalized names -------------------------

func TestR5_ExcludeNamespacesIsNormalizedAtMatchTime(t *testing.T) {
	for _, entry := range []string{" shop", "Shop", "SHOP ", "\tshop\n"} {
		cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.ExcludeNamespaces = []string{entry} })
		g := newTestGate(t, fakeClient(t, cp))

		assert.True(t, g.NamespaceProtected(context.Background(), "shop"),
			"excludeNamespaces entry %q protects nothing", entry)
	}
}

func TestR5_QueriedNamespaceIsNormalizedToo(t *testing.T) {
	cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.ExcludeNamespaces = []string{"shop"} })
	g := newTestGate(t, fakeClient(t, cp), WithProtectedNamespaces("Payments "))

	assert.True(t, g.NamespaceProtected(context.Background(), " Shop"))
	assert.True(t, g.NamespaceProtected(context.Background(), "payments"))
	assert.True(t, g.NamespaceProtected(context.Background(), "KUBE-SYSTEM"))
	assert.False(t, g.NamespaceProtected(context.Background(), "other"))
}

// --- R12: an unknown operator namespace blocks rule-based proposals too -----------

func TestR12_UnknownOperatorNamespaceRefusesEveryProposal(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")), WithOperatorNamespaceUnknown())

	for name, cand := range map[string]Candidate{
		"rule-based action": legacyCandidate(memPatch("384Mi")),
		"ai plan":           planCandidate(personaUpdateStep(memPatch("384Mi"))),
		"preflight intent":  {Target: target(), Namespace: testNS},
	} {
		stage, guardrail := refusedBy(t, run(t, g, cand, PhaseProposal))
		assert.Equal(t, StageGuardrails, stage, name)
		assert.Equal(t, GuardrailOperatorNamespace, guardrail, name)
	}
	assert.True(t, g.NamespaceProtected(context.Background(), testNS), "the model must not see any namespace either")
}

func TestR12_KnownOperatorNamespaceIsUnaffected(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")), WithProtectedNamespaces("dorgu-system"))

	assert.True(t, run(t, g, legacyCandidate(memPatch("384Mi")), PhaseProposal).Allowed())
}
