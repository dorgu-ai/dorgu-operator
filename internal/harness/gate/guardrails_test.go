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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ------------------------------------------------------------------ deny-list

func TestDenyList_ProtectedNamespaces(t *testing.T) {
	g := newTestGate(t, fakeClient(t, clusterPersona(func(p *dorguv1.SelfHealingPolicy) {
		p.ExcludeNamespaces = []string{"staging"}
	})), WithProtectedNamespaces("dorgu-system", "payments", ""))

	tests := []struct {
		namespace string
		denied    bool
	}{
		{"kube-system", true},
		{"dorgu-system", true},
		{"payments", true},
		{"staging", true},
		{"default", false},
		{"dorgu-operator-system", false}, // the old hardcoded name is not special
		{"", true},                       // an undeterminable namespace fails closed
	}
	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			assert.Equal(t, tt.denied, g.NamespaceProtected(t.Context(), tt.namespace))
			assert.Equal(t, tt.denied, g.NamespaceDenied(t.Context(), tt.namespace) != nil,
				"the planner gate and the diagnosis gate must never disagree")
		})
	}
}

func TestDenyList_KubeSystemIsAlwaysProtected(t *testing.T) {
	assert.True(t, newTestGate(t, fakeClient(t)).NamespaceProtected(t.Context(), "kube-system"))
}

func TestDenyList_UnreadablePolicyFailsClosedWithoutLeakingTheError(t *testing.T) {
	g := newTestGate(t, failingListClient(t, isKind[*dorguv1.ClusterPersonaList]), WithProtectedNamespaces("dorgu-system"))

	assert.True(t, g.NamespaceProtected(t.Context(), "kube-system"))
	assert.True(t, g.NamespaceProtected(t.Context(), "apps"), "an unreadable deny-list must fail closed")
	v := g.NamespaceDenied(t.Context(), "apps")
	require.NotNil(t, v)
	assert.Equal(t, RuleDenyList, v.Rule)
	assert.NotContains(t, v.Message, "unavailable", "the raw API error stays in the log")
}

func TestDenyList_NilGateIsProtected(t *testing.T) {
	var g *Gate
	assert.True(t, g.NamespaceProtected(t.Context(), "apps"))
	require.NotNil(t, g.NamespaceDenied(t.Context(), "apps"))
}

func TestGuardrails_DenyListRefusesAProposalInAProtectedNamespace(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi")), WithProtectedNamespaces(testNS))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

	assert.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailDenyList))
	assert.Contains(t, d.SkipReason(), "safety check failed: [[deny-list]")
}

// -------------------------------------------------------------------- siblings

// OP-02: five RemediationActions without Dorgu's labels already target the
// persona this hour. The rate limit selects by spec, so they count.
func TestGuardrails_RateLimitCountsUnlabelledSiblings(t *testing.T) {
	objs := []clientObject{persona("256Mi")}
	for i := range DefaultMaxRemediationsPerHour {
		objs = append(objs, sibling(fmt.Sprintf("unlabelled-%d", i), "Pending"))
	}
	g := newTestGate(t, fakeClient(t, objs...))

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

	assert.Equal(t, ResultRefuse, d.Result)
	v := verdict(d, GuardrailRateLimit)
	require.NotNil(t, v)
	assert.Equal(t, "refuse", v.Verdict)
	assert.Equal(t, "5", v.Numbers["recent"])
}

func TestGuardrails_SiblingsAreSelectedBySpecTarget(t *testing.T) {
	other := sibling("other-persona", "Pending", func(ra *dorguv1.RemediationAction) { ra.Spec.PersonaRef.Name = "someone-else" })
	otherNS := sibling("other-ns", "Pending", func(ra *dorguv1.RemediationAction) { ra.Spec.PersonaRef.Namespace = "elsewhere" })
	self := sibling("ra-new", "Pending")
	mine := sibling("mine", "Pending", func(ra *dorguv1.RemediationAction) {
		ra.Namespace = "elsewhere"
		ra.Spec.PersonaRef.Namespace = "" // defaults to the action's namespace: "elsewhere"
	})
	g := newTestGate(t, fakeClient(t, persona("256Mi"), other, otherNS, self, mine))

	got, err := g.siblings(t.Context(), &Candidate{Target: target(), Namespace: testNS, Name: "ra-new"})
	require.NoError(t, err)

	assert.Empty(t, got, "another persona, another namespace, and the candidate itself are not siblings")

	// An action in another namespace whose ref names no namespace targets THAT namespace.
	got, err = g.siblings(t.Context(), &Candidate{
		Target:    dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: testPersona, Namespace: "elsewhere"},
		Namespace: "elsewhere",
	})
	require.NoError(t, err)
	names := []string{}
	for _, ra := range got {
		names = append(names, ra.Name)
	}
	assert.ElementsMatch(t, []string{"mine", "other-ns"}, names)
}

func TestGuardrails_RateLimitIgnoresOldActionsAndSelf(t *testing.T) {
	old := metav1.NewTime(fixedNow.Add(-2 * time.Hour))
	objs := []clientObject{persona("256Mi"), sibling("ra-new", "Pending")}
	for i := range DefaultMaxRemediationsPerHour {
		objs = append(objs, sibling(fmt.Sprintf("old-%d", i), "Completed", func(ra *dorguv1.RemediationAction) {
			ra.CreationTimestamp = old
		}))
	}
	g := newTestGate(t, fakeClient(t, objs...))

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

	assert.Equal(t, "pass", verdictKind(d, GuardrailRateLimit))
}

func TestGuardrails_RateLimitHonoursTheClusterPersonaPolicy(t *testing.T) {
	cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.MaxRemediationsPerHour = 1 })
	g := newTestGate(t, fakeClient(t, persona("256Mi"), cp, sibling("one", "Completed")))

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

	assert.Equal(t, "refuse", verdictKind(d, GuardrailRateLimit))
	assert.Equal(t, "1", verdict(d, GuardrailRateLimit).Numbers["max"])
}

// The #51 follow-up: a ClusterPersona list error used to default to 5/h.
func TestGuardrails_RateLimitFailsClosedWhenThePolicyCannotBeRead(t *testing.T) {
	// The deny-list reads ClusterPersonas too, so make only the SECOND read fail:
	// protect nothing and let the deny-list pass first.
	calls := 0
	c := failingListClient(t, func(list clientObjectList) bool {
		if !isKind[*dorguv1.ClusterPersonaList](list) {
			return false
		}
		calls++
		return calls > 1
	}, persona("256Mi"))
	g := newTestGate(t, c)

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

	assert.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailRateLimit))
	assert.NotContains(t, verdict(d, GuardrailRateLimit).Reason, "unavailable")
}

func TestGuardrails_RateLimitIsNotAnApprovalCheck(t *testing.T) {
	objs := []clientObject{persona("256Mi"), deployment("256Mi")}
	for i := range DefaultMaxRemediationsPerHour {
		objs = append(objs, sibling(fmt.Sprintf("s-%d", i), "Completed"))
	}
	d := run(t, newTestGate(t, fakeClient(t, objs...)), planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseApprovalRecheck)
	assert.Nil(t, verdict(d, GuardrailRateLimit))
	assert.True(t, d.Allowed(), d.SkipReason())
}

func TestGuardrails_SiblingListFailureIsAnError(t *testing.T) {
	g := newTestGate(t, failingListClient(t, isKind[*dorguv1.RemediationActionList], persona("256Mi")))
	_, err := g.Run(t.Context(), planCandidate(), PhaseProposal)
	require.Error(t, err)
}

// ------------------------------------------------------------------ concurrent

func TestGuardrails_OneRemediationInFlightPerPersona(t *testing.T) {
	for _, phase := range []string{"Approved", "Applying", "Verifying"} {
		t.Run(phase, func(t *testing.T) {
			g := newTestGate(t, fakeClient(t, persona("256Mi"), sibling("in-flight", phase)))
			d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
			assert.Equal(t, "refuse", verdictKind(d, GuardrailConcurrent))
			assert.Equal(t, "in-flight", verdict(d, GuardrailConcurrent).Numbers["sibling"])
		})
	}
	for _, phase := range []string{"Pending", "Completed", "Failed", "Rejected", "Expired", "RolledBack"} {
		t.Run("not "+phase, func(t *testing.T) {
			g := newTestGate(t, fakeClient(t, persona("256Mi"), sibling("quiet", phase)))
			d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
			assert.Equal(t, "pass", verdictKind(d, GuardrailConcurrent))
		})
	}
}

// OP-05: two plans approved together. Exactly one proceeds.
func TestGuardrails_TwoApprovedPlansAreOrderedByApprovalTime(t *testing.T) {
	earlier := metav1.NewTime(fixedNow.Add(-time.Minute))
	later := metav1.NewTime(fixedNow)
	first := sibling("ra-first", "Approved", func(ra *dorguv1.RemediationAction) { ra.Status.ApprovedAt = &earlier })
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), first))

	cand := planCandidate(personaUpdateStep(memPatch("300Mi")))
	cand.ApprovedAt = &later
	d := run(t, g, cand, PhaseApprovalRecheck)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailConcurrent), "the later approval defers to the earlier one")

	cand.ApprovedAt = &earlier
	cand.Name = "ra-zzz"
	d = run(t, g, cand, PhaseApprovalRecheck)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailConcurrent), "an equal time is broken by name")

	cand.Name = "ra-aaa"
	d = run(t, g, cand, PhaseApprovalRecheck)
	assert.Equal(t, "pass", verdictKind(d, GuardrailConcurrent), "the earlier approval proceeds")

	veryEarly := metav1.NewTime(fixedNow.Add(-time.Hour))
	cand.ApprovedAt = &veryEarly
	d = run(t, g, cand, PhaseApprovalRecheck)
	assert.Equal(t, "pass", verdictKind(d, GuardrailConcurrent))
}

func TestGuardrails_ApprovalWithoutTimestampsDefers(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), sibling("ra-other", "Approved")))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseApprovalRecheck)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailConcurrent), "with no way to order them, neither claims to be first")
}

func TestGuardrails_AnActivelyRunningSiblingAlwaysBlocksAnApproval(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), sibling("busy", "Applying")))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseApprovalRecheck)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailConcurrent))
}

// -------------------------------------------------------------- failed cooldown

func failed(reason string, appliedAt *metav1.Time) *dorguv1.RemediationAction {
	return sibling("failed", "Failed", func(ra *dorguv1.RemediationAction) {
		ra.Status.AppliedAt = appliedAt
		ra.Status.Conditions = []metav1.Condition{{
			Type: conditionApplied, Status: metav1.ConditionFalse, Reason: reason,
			Message: "nothing was applied", LastTransitionTime: metav1.NewTime(fixedNow.Add(-10 * time.Minute)),
		}}
	})
}

// F-03: approving the remediation Dorgu itself suggested produced a precondition
// failure and then a 30-minute blackout, for a remediation that never touched
// the cluster.
func TestGuardrails_FailedCooldown(t *testing.T) {
	recent := metav1.NewTime(fixedNow.Add(-5 * time.Minute))
	stale := metav1.NewTime(fixedNow.Add(-2 * time.Hour))

	tests := []struct {
		name   string
		action *dorguv1.RemediationAction
		want   string
	}{
		{"a genuine apply failure cools down", failed("Failed", nil), "refuse"},
		{"applied then failed cools down", failed(dorguv1.ReasonPreconditionRejected, &recent), "refuse"},
		{"a precondition rejection never touched the cluster", failed(dorguv1.ReasonPreconditionRejected, nil), "pass"},
		{"a failure long ago has cooled", failed("Failed", &stale), "pass"},
		{"an acknowledged advisory is not a failure", sibling("f", "Acknowledged"), "pass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newTestGate(t, fakeClient(t, persona("256Mi"), tt.action))
			d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
			assert.Equal(t, tt.want, verdictKind(d, GuardrailFailedCooldown))
		})
	}
}

// ---------------------------------------------------------------- blast radius

func TestGuardrails_BlastRadiusIsMeasuredAgainstTheLiveValue(t *testing.T) {
	// The persona says 512Mi (months stale); the container actually has 64Mi.
	g := newTestGate(t, fakeClient(t, persona("512Mi"), deployment("64Mi")))

	d := run(t, g, planCandidate(personaUpdateStep(memPatch("256Mi"))), PhaseProposal)

	require.True(t, d.Allowed(), d.SkipReason())
	assert.Equal(t, ResultClamp, d.Result)
	assert.Equal(t, "clamp", verdictKind(d, GuardrailBlastRadius))
	v := verdict(d, GuardrailBlastRadius)
	assert.Equal(t, "64Mi", v.Numbers["baseline"], "the live value, not the persona's 512Mi")
	assert.Equal(t, "256Mi", v.Numbers["requested"])
	assert.Equal(t, "4.00", v.Numbers["ratio"])
	assert.Nil(t, d.Steps[0].Patch, "the refused field is gone from the plan")
	assert.False(t, d.Steps[0].AutoExecutable)
	require.Len(t, d.Steps[0].Safety, 1)
	assert.Equal(t, dorguv1.SafetyVerdictRejected, d.Steps[0].Safety[0].Verdict)
	assert.Equal(t, "64Mi", d.Steps[0].Safety[0].Baseline)
}

func TestGuardrails_BlastRadiusBounds(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		want      string
	}{
		{"1.5x is allowed", "384Mi", "pass"},
		{"exactly 2x is allowed", "512Mi", "pass"},
		{"3x is clamped", "768Mi", "clamp"},
		{"a 60% decrease is clamped", "100Mi", "clamp"},
		{"a 50% decrease is allowed", "128Mi", "pass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
			d := run(t, g, planCandidate(personaUpdateStep(memPatch(tt.requested))), PhaseProposal)
			assert.Equal(t, tt.want, verdictKind(d, GuardrailBlastRadius))
		})
	}
}

func TestGuardrails_BlastRadiusReportsEveryBreachInAStableOrder(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Resources.Limits["cpu"] = mustQuantity("100m")
	})))
	patch := jsonOf(`{"spec":{"resources":{"limits":{"memory":"4Gi","cpu":"2"}}}}`)

	d := run(t, g, planCandidate(personaUpdateStep(patch)), PhaseProposal)

	var fields []string
	for _, v := range d.Verdicts {
		if v.Guardrail == GuardrailBlastRadius && v.Verdict == "clamp" {
			fields = append(fields, v.Numbers["field"])
		}
	}
	assert.Equal(t, []string{cpuLimit, memLimit}, fields, "every breach is named, in field order")
}

func TestGuardrails_LegacyActionBreachIsRefusedWhole(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	d := run(t, g, legacyCandidate(memPatch("4Gi")), PhaseProposal)
	assert.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailBlastRadius))
	assert.Contains(t, d.SkipReason(), "safety check failed: [[blast-radius]")
}

func TestGuardrails_NoBaselineIsClampedNotPassed(t *testing.T) {
	// No live workload, and the persona records no memory limit.
	p := persona("")
	p.Spec.Resources.Limits.Memory = ""
	g := newTestGate(t, fakeClient(t, p))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("4Gi"))), PhaseProposal)
	// A value nothing was measured against is not within any cap (R3).
	assert.Equal(t, "clamp", verdictKind(d, GuardrailNoBaseline))
	assert.Equal(t, "absent", verdictKind(d, GuardrailBlastRadius))
	for _, step := range d.Steps {
		assert.Empty(t, stepPatchValue(t, step))
	}
}

func TestGuardrails_AbsentFieldIsDroppedNotIntroduced(t *testing.T) {
	// The container sets a memory limit but no CPU limit.
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	patch := jsonOf(`{"spec":{"resources":{"limits":{"memory":"300Mi","cpu":"500m"}}}}`)

	d := run(t, g, planCandidate(personaUpdateStep(patch)), PhaseProposal)

	assert.Equal(t, "clamp", verdictKind(d, GuardrailAbsentField))
	assert.Equal(t, []string{memLimit}, PatchLeafPaths(d.Steps[0].Patch.Raw))
	require.Len(t, d.Steps[0].Safety, 1)
	assert.Equal(t, dorguv1.SafetyRuleAbsentField, d.Steps[0].Safety[0].Rule)
	assert.Contains(t, d.Steps[0].Rationale, "does not set")
}

func TestGuardrails_LegacyActionAbsentField(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))

	d := run(t, g, legacyCandidate(jsonOf(`{"spec":{"resources":{"limits":{"memory":"300Mi","cpu":"500m"}}}}`)), PhaseProposal)
	assert.Equal(t, "clamp", verdictKind(d, GuardrailAbsentField))
	assert.Equal(t, []string{memLimit}, PatchLeafPaths(d.Action.Patch.Raw))

	d = run(t, g, legacyCandidate(jsonOf(`{"spec":{"resources":{"limits":{"cpu":"500m"}}}}`)), PhaseProposal)
	assert.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailAbsentField))
}

// OP-04: approved after the live limit was cut 4x. The cap is re-measured on live
// values, so the approval does not carry the stale proposal onto the cluster.
func TestGuardrails_ApprovalRemeasuresAgainstTheLiveValue(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("64Mi")))
	// Proposed as 256Mi -> 512Mi; the live limit is now 64Mi.
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("512Mi"))), PhaseApprovalRecheck)

	assert.Equal(t, ResultRefuse, d.Result, "a plan clamped to nothing has nothing left to approve")
	assert.Equal(t, "clamp", verdictKind(d, GuardrailBlastRadius))
	assert.Equal(t, "refuse", verdictKind(d, GuardrailNothingLeft))
}

// OP-01: a hand-written plan with an empty prePatchState still cannot move a
// value 16x, because the baseline is the cluster's, not the plan's.
func TestGuardrails_AnEmptyPrePatchStateDoesNotDisableTheCap(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	step := personaUpdateStep(memPatch("4Gi"))
	step.PrePatchState = jsonOf(`{}`)

	d := run(t, g, planCandidate(step), PhaseApprovalRecheck)

	assert.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, "clamp", verdictKind(d, GuardrailBlastRadius))
}

func TestGuardrails_ApprovalOfACleanPlanPasses(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseApprovalRecheck)
	assert.Equal(t, ResultPass, d.Result, d.SkipReason())
}

// CR-01: no plan is written that applies nothing when a resource change was diagnosed.
func TestGuardrails_FinalPlanMustApplyTheDiagnosedChange(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	advisory := dorguv1.RemediationStep{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "look"}

	cand := planCandidate(advisory)
	cand.Final = true
	cand.Facts.TargetPath = ResourcePathMemory

	d := run(t, g, cand, PhaseProposal)
	assert.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailNothingApplies))

	cand.Facts.AdvisoryAllowed = true
	assert.True(t, run(t, g, cand, PhaseProposal).Allowed(), "the rule engine itself declined: advice is the honest outcome")

	cand.Facts.AdvisoryAllowed = false
	cand.Facts.TargetPath = ""
	assert.True(t, run(t, g, cand, PhaseProposal).Allowed(), "no resource change was diagnosed")

	applying := planCandidate(personaUpdateStep(memPatch("300Mi")))
	applying.Final = true
	applying.Facts.TargetPath = ResourcePathMemory
	assert.Equal(t, "pass", verdictKind(run(t, g, applying, PhaseProposal), GuardrailNothingApplies))

	wrongField := planCandidate(personaUpdateStep(jsonOf(`{"spec":{"resources":{"requests":{"memory":"300Mi"}}}}`)))
	wrongField.Final = true
	wrongField.Facts.TargetPath = ResourcePathMemory
	assert.Equal(t, ResultRefuse, run(t, g, wrongField, PhaseProposal).Result,
		"raising the request does not fix an OOM on the limit")
}

func TestAppliesDiagnosedChange(t *testing.T) {
	steps := []dorguv1.RemediationStep{personaUpdateStep(memPatch("300Mi"))}
	legacy := dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch("300Mi")}

	assert.True(t, AppliesDiagnosedChange(steps, dorguv1.RemediationActionDetail{}, ResourcePathMemory))
	assert.False(t, AppliesDiagnosedChange(steps, dorguv1.RemediationActionDetail{}, ResourcePathCPU))
	assert.True(t, AppliesDiagnosedChange(nil, legacy, ResourcePathMemory))
	assert.False(t, AppliesDiagnosedChange(nil, legacy, ResourcePathCPU))
	assert.False(t, AppliesDiagnosedChange(nil, dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}, ResourcePathMemory))
	steps[0].AutoExecutable = false
	assert.False(t, AppliesDiagnosedChange(steps, legacy, ResourcePathMemory), "steps are the plan of record when present")
}

func TestApprovedBefore(t *testing.T) {
	t1 := metav1.NewTime(fixedNow)
	t2 := metav1.NewTime(fixedNow.Add(time.Second))
	sib := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Status: dorguv1.RemediationActionStatus{ApprovedAt: &t1}}

	assert.True(t, approvedBefore(sib, &Candidate{ApprovedAt: &t2}))
	assert.False(t, approvedBefore(sib, &Candidate{ApprovedAt: &t1, Name: "c"}) == false)
	assert.False(t, approvedBefore(&dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Status: dorguv1.RemediationActionStatus{ApprovedAt: &t2}}, &Candidate{ApprovedAt: &t1}))
}
