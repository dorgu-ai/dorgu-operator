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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// P2-D13: every verdict the Guardrails stage produces says where the limit it
// applied came from, so the dashboard and `dorgu report` stop inferring it.
//
// The values are the places the code actually reads a limit from:
//   - Default: a constant in the operator (the 2x cap, one in flight, the 30 minute
//     cooldown, 5 an hour, kube-system and the operator's namespace);
//   - ClusterPersona: a policy the user set there (maxRemediationsPerHour,
//     excludeNamespaces);
//   - ApplicationPersona: the baseline a change is measured against when no live
//     workload resolved;
//   - LiveWorkload: the baseline read from the live Deployment.

func ceilingOf(t *testing.T, d *Decision, guardrail string) string {
	t.Helper()
	v := verdict(d, guardrail)
	require.NotNil(t, v, "no %s verdict in %+v", guardrail, d.Verdicts)
	return v.CeilingSource
}

// assertEveryGuardrailsVerdictHasASource is the rule itself: no verdict of the
// stage leaves the field empty, and every value is one of the enum's.
func assertEveryGuardrailsVerdictHasASource(t *testing.T, d *Decision) {
	t.Helper()
	valid := []string{
		dorguv1.CeilingSourceDefault, dorguv1.CeilingSourceClusterPersona,
		dorguv1.CeilingSourceApplicationPersona, dorguv1.CeilingSourceLiveWorkload,
	}
	count := 0
	for _, v := range d.Verdicts {
		if v.Stage != StageGuardrails {
			continue
		}
		count++
		assert.Contains(t, valid, v.CeilingSource, "%s/%s (%s) has no valid ceilingSource", v.Stage, v.Guardrail, v.Verdict)
	}
	assert.Positive(t, count, "the scenario produced no Guardrails verdicts to check")
}

func TestCeilingSource_BlastRadiusFromTheLiveWorkload(t *testing.T) {
	g := standardGate(t)

	clamped := run(t, g, planCandidate(personaUpdateStep(memPatch("1Gi"))), PhaseProposal) // 4x the live 256Mi
	assert.Equal(t, "clamp", verdictKind(clamped, GuardrailBlastRadius))
	assert.Equal(t, dorguv1.CeilingSourceLiveWorkload, ceilingOf(t, clamped, GuardrailBlastRadius))
	assertEveryGuardrailsVerdictHasASource(t, clamped)

	passed := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
	assert.Equal(t, "pass", verdictKind(passed, GuardrailBlastRadius))
	assert.Equal(t, dorguv1.CeilingSourceLiveWorkload, ceilingOf(t, passed, GuardrailBlastRadius))
	assertEveryGuardrailsVerdictHasASource(t, passed)
}

func TestCeilingSource_BlastRadiusFromTheApplicationPersonaWhenNoWorkloadResolved(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"))) // no Deployment

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("1Gi"))), PhaseProposal)

	assert.Equal(t, "clamp", verdictKind(d, GuardrailBlastRadius))
	assert.Equal(t, dorguv1.CeilingSourceApplicationPersona, ceilingOf(t, d, GuardrailBlastRadius))
	assertEveryGuardrailsVerdictHasASource(t, d)
}

func TestCeilingSource_NoBaselineNamesWhereTheBaselineWasLookedFor(t *testing.T) {
	p := persona("")
	p.Spec.Resources.Limits.Memory = ""
	g := newTestGate(t, fakeClient(t, p)) // no workload, and the persona records no memory limit

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("4Gi"))), PhaseProposal)

	assert.Equal(t, "clamp", verdictKind(d, GuardrailNoBaseline))
	assert.Equal(t, dorguv1.CeilingSourceApplicationPersona, ceilingOf(t, d, GuardrailNoBaseline))
	assertEveryGuardrailsVerdictHasASource(t, d)
}

func TestCeilingSource_AbsentFieldIsTheLiveContainer(t *testing.T) {
	g := standardGate(t)
	patch := jsonOf(`{"spec":{"resources":{"limits":{"memory":"300Mi","cpu":"500m"}}}}`)

	d := run(t, g, planCandidate(personaUpdateStep(patch)), PhaseProposal)

	assert.Equal(t, "clamp", verdictKind(d, GuardrailAbsentField))
	assert.Equal(t, dorguv1.CeilingSourceLiveWorkload, ceilingOf(t, d, GuardrailAbsentField))
	assertEveryGuardrailsVerdictHasASource(t, d)
}

func TestCeilingSource_RateLimit(t *testing.T) {
	t.Run("the built-in default", func(t *testing.T) {
		g := standardGate(t)
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assert.Equal(t, dorguv1.CeilingSourceDefault, ceilingOf(t, d, GuardrailRateLimit))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("a ClusterPersona policy", func(t *testing.T) {
		cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.MaxRemediationsPerHour = 1 })
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), cp, sibling("one", "Completed")))
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailRateLimit))
		assert.Equal(t, dorguv1.CeilingSourceClusterPersona, ceilingOf(t, d, GuardrailRateLimit))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("a ClusterPersona that sets no limit leaves the default", func(t *testing.T) {
		cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.ExcludeNamespaces = []string{"staging"} })
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), cp))
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assert.Equal(t, dorguv1.CeilingSourceDefault, ceilingOf(t, d, GuardrailRateLimit))
	})
	t.Run("an unreadable policy", func(t *testing.T) {
		g := newTestGate(t, failingListClient(t, isKind[*dorguv1.ClusterPersonaList]), WithProtectedNamespaces("dorgu-system"))
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
}

func TestCeilingSource_DenyList(t *testing.T) {
	t.Run("built in: kube-system", func(t *testing.T) {
		g := standardGate(t)
		cand := planCandidate(personaUpdateStep(memPatch("300Mi")))
		cand.Target.Namespace = "kube-system"
		cand.Namespace = "kube-system"
		d := run(t, g, cand, PhaseProposal)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailDenyList))
		assert.Equal(t, dorguv1.CeilingSourceDefault, ceilingOf(t, d, GuardrailDenyList))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("a namespace the ClusterPersona excludes", func(t *testing.T) {
		cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.ExcludeNamespaces = []string{testNS} })
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), cp))
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailDenyList))
		assert.Equal(t, dorguv1.CeilingSourceClusterPersona, ceilingOf(t, d, GuardrailDenyList))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("a namespace nothing lists passes under the default", func(t *testing.T) {
		g := standardGate(t)
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assert.Equal(t, "pass", verdictKind(d, GuardrailDenyList))
		assert.Equal(t, dorguv1.CeilingSourceDefault, ceilingOf(t, d, GuardrailDenyList))
	})
	t.Run("an operator that does not know its own namespace", func(t *testing.T) {
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")), WithOperatorNamespaceUnknown())
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assert.Equal(t, dorguv1.CeilingSourceDefault, ceilingOf(t, d, GuardrailOperatorNamespace))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
}

func TestCeilingSource_ConcurrencyAndCooldownAreOperatorDefaults(t *testing.T) {
	t.Run("one in flight", func(t *testing.T) {
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), sibling("in-flight", "Applying")))
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailConcurrent))
		assert.Equal(t, dorguv1.CeilingSourceDefault, ceilingOf(t, d, GuardrailConcurrent))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("none in flight", func(t *testing.T) {
		d := run(t, standardGate(t), planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assert.Equal(t, dorguv1.CeilingSourceDefault, ceilingOf(t, d, GuardrailConcurrent))
	})
	t.Run("failed cooldown", func(t *testing.T) {
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), failed("Failed", nil)))
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailFailedCooldown))
		assert.Equal(t, dorguv1.CeilingSourceDefault, ceilingOf(t, d, GuardrailFailedCooldown))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
}

func TestCeilingSource_TheApprovalRecheckFillsItToo(t *testing.T) {
	g := standardGate(t)

	d := approvalRun(t, g, proposedAction())

	require.True(t, d.Allowed(), d.SkipReason())
	assert.Equal(t, dorguv1.CeilingSourceLiveWorkload, ceilingOf(t, d, GuardrailBlastRadius))
	assertEveryGuardrailsVerdictHasASource(t, d)
}

func TestCeilingSource_NothingLeftAtApprovalIsMeasuredAgainstTheLiveWorkload(t *testing.T) {
	// The live limit was cut to 64Mi after the proposal: 384Mi is 6x live.
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("64Mi")))

	d := approvalRun(t, g, proposedAction())

	assert.False(t, d.Allowed())
	assert.Equal(t, dorguv1.CeilingSourceLiveWorkload, ceilingOf(t, d, GuardrailNothingLeft))
	assertEveryGuardrailsVerdictHasASource(t, d)
}

func TestCeilingSource_CompletenessAndLegacyActionVerdicts(t *testing.T) {
	t.Run("nothing-applies on a plan that applies the diagnosed change", func(t *testing.T) {
		cand := planCandidate(personaUpdateStep(memPatch("300Mi")))
		cand.Final = true
		cand.Facts.TargetPath = memLimit
		d := run(t, standardGate(t), cand, PhaseProposal)
		assert.Equal(t, "pass", verdictKind(d, GuardrailNothingApplies))
		assert.Equal(t, dorguv1.CeilingSourceDefault, ceilingOf(t, d, GuardrailNothingApplies))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("nothing-applies refusing a plan that applies nothing", func(t *testing.T) {
		cand := planCandidate(dorguv1.RemediationStep{Order: 1, ID: "m", Type: dorguv1.StepTypeManual, Description: "look"})
		cand.Final = true
		cand.Facts.TargetPath = memLimit
		d := run(t, standardGate(t), cand, PhaseProposal)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailNothingApplies))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("a legacy action refused whole", func(t *testing.T) {
		d := run(t, standardGate(t), legacyCandidate(memPatch("4Gi")), PhaseProposal)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailBlastRadius))
		assert.Equal(t, dorguv1.CeilingSourceLiveWorkload, ceilingOf(t, d, GuardrailBlastRadius))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("a legacy action with an absent field", func(t *testing.T) {
		patch := jsonOf(`{"spec":{"resources":{"limits":{"cpu":"500m"}}}}`)
		d := run(t, standardGate(t), legacyCandidate(patch), PhaseProposal)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailAbsentField))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("a legacy action with no baseline", func(t *testing.T) {
		p := persona("")
		p.Spec.Resources.Limits.Memory = ""
		g := newTestGate(t, fakeClient(t, p))
		d := run(t, g, legacyCandidate(memPatch("4Gi")), PhaseProposal)
		assert.Equal(t, "refuse", verdictKind(d, GuardrailNoBaseline))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
	t.Run("a deployer-owned workload", func(t *testing.T) {
		g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("512Mi", func(d *appsv1.Deployment) {
			d.Labels["app.kubernetes.io/managed-by"] = "Helm"
			d.Annotations = map[string]string{"meta.helm.sh/release-name": "web"}
		})))
		cand := planCandidate(dorguv1.RemediationStep{Order: 1, ID: "m", Type: dorguv1.StepTypeManual, Description: "advice"})
		cand.Final = true
		cand.Facts.TargetPath = memLimit
		d := run(t, g, cand, PhaseProposal)
		assert.Equal(t, "pass", verdictKind(d, GuardrailNothingApplies))
		assertEveryGuardrailsVerdictHasASource(t, d)
	})
}

// A verdict that is not the Guardrails stage's is not required to carry a source,
// and the field is optional on the CRD: a stored v0.13 verdict has none.
func TestCeilingSource_OtherStagesNeedNone(t *testing.T) {
	d := run(t, standardGate(t), planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
	for _, v := range d.Verdicts {
		if v.Stage != StageGuardrails {
			assert.Empty(t, v.CeilingSource, "%s/%s", v.Stage, v.Guardrail)
		}
	}
}

// The ticket copies the approval re-check's verdicts, and with them their sources.
func TestCeilingSource_TheTicketCarriesTheSources(t *testing.T) {
	ticket := issuedTicket(t, standardGate(t), proposedAction())

	found := false
	for _, v := range ticket.Verdicts {
		if v.Guardrail == GuardrailBlastRadius {
			found = true
			assert.Equal(t, dorguv1.CeilingSourceLiveWorkload, v.CeilingSource)
		}
	}
	assert.True(t, found, "no blast-radius verdict on the ticket")
}
