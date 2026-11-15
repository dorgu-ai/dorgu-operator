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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// C6 review round: F-1 (a rollback right after an auto-rollback, and in observe mode),
// F-2 (refused, never partial) and F-6 (a retry after the action was made).

// rolledBackRA is the action the operator rolled back minutes ago: the persona is back
// at its baseline, the Deployment still carries the change.
func rolledBackRA(age time.Duration) *dorguv1.RemediationAction {
	at := metav1.NewTime(time.Now().Add(-age))
	return appliedRA(func(ra *dorguv1.RemediationAction) {
		ra.Status.Phase = "RolledBack"
		ra.Status.AppliedAt = &at
		ra.CreationTimestamp = at
	})
}

func observeCluster() *dorguv1.ClusterPersona {
	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: dorguv1.ClusterPersonaSpec{Name: "cluster", Policies: &dorguv1.ClusterPolicies{
			SelfHealing: &dorguv1.SelfHealingPolicy{Mode: dorguv1.SelfHealingModeObserve},
		}},
	}
}

func requireConvertedPending(t *testing.T, out *Outcome) {
	t.Helper()
	require.NotNil(t, out)
	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, "rejected: %+v: %s", out.Rejection, out.Message)
	require.NotNil(t, out.Action)
	assert.Equal(t, pendingPhase, out.Action.Status.Phase)
}

func TestRollback_RightAfterTheAutoRollbackSucceedsIntoPending(t *testing.T) {
	// The operator's advisory sends a person here minutes after the auto-rollback. The
	// action being undone is not a failed-cooldown sibling of its own rollback.
	w := newWorld(t, Config{}, webPersona("256Mi"), webDeployment("512Mi"), rolledBackRA(5*time.Minute))
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))

	out := w.process(t, "rb", nil)

	requireConvertedPending(t, out)
	assert.Equal(t, appliedName, out.Action.Annotations[dorguv1.AnnotationRollbackOf])
}

func TestRollback_InObserveModeAPersonsRollbackReachesPending(t *testing.T) {
	w := newWorld(t, Config{}, rollbackObjects(observeCluster())...)
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))

	out := w.process(t, "rb", nil)

	requireConvertedPending(t, out)
	assert.Equal(t, dorguv1.ProposalSourceRollback, out.Action.Spec.Provenance.Source)
	assert.Equal(t, rollbackAlice, out.Action.Spec.Provenance.RequestedBy)
}

func TestRollback_ObserveModeStillWithholdsAnOrdinaryProposal(t *testing.T) {
	// The observe exemption belongs to a rollback, not to whoever claims one.
	w := newWorld(t, Config{}, append(baseObjects(), observeCluster())...)
	w.create(t, proposal("ordinary", "alice", "k"))

	out := w.process(t, "ordinary", nil)

	require.Equal(t, v1alpha1.PhaseRejected, out.Phase)
	assert.Equal(t, gate.StageModePolicy, out.Rejection.Stage)
}

func TestRollback_AnotherFailedActionStillBlocksTheRollback(t *testing.T) {
	at := metav1.NewTime(time.Now().Add(-4 * time.Minute))
	other := appliedRA(func(ra *dorguv1.RemediationAction) {
		ra.Name = "ra-other-failed"
		ra.Status.Phase = "Failed"
		ra.Status.AppliedAt = &at
		ra.CreationTimestamp = at
	})
	w := newWorld(t, Config{}, webPersona("256Mi"), webDeployment("512Mi"), rolledBackRA(5*time.Minute), other)
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))

	out := w.process(t, "rb", nil)

	require.Equal(t, v1alpha1.PhaseRejected, out.Phase)
	assert.Equal(t, gate.StageGuardrails, out.Rejection.Stage)
	assert.Equal(t, gate.GuardrailFailedCooldown, out.Rejection.Reason)
	assert.Contains(t, out.Message, "ra-other-failed")
}

// ---- F-2: refused, never partial -------------------------------------------------

// ticketedAt is an action whose ticket wrote the given values over the given baselines
// (a JSON object for each), with the live workload and the persona holding the written
// memory and cpu.
func ticketedAt(baseline, wrote string) *dorguv1.RemediationAction {
	return appliedRA(func(ra *dorguv1.RemediationAction) {
		ra.Status.Gate.PrePatchState = jsonRaw(`{"spec":{"resources":{"limits":` + baseline + `}}}`)
		ra.Status.ApplyTicket.Patch = jsonRaw(`{"spec":{"resources":{"limits":` + wrote + `}}}`)
	})
}

func withCPU(memory, cpu string) []client.Object {
	d := webDeployment(memory)
	d.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse(cpu)
	p := webPersona(memory)
	p.Spec.Resources.Limits.CPU = cpu
	return []client.Object{d, p}
}

func requireRefusedAsPartial(t *testing.T, w *world, out *Outcome, before int) {
	t.Helper()
	requireRollbackRejected(t, out, v1alpha1.ReasonRollbackWouldBePartial)
	assert.Len(t, w.actions(t), before, "no action was made, so nothing half-restores")
	assert.Contains(t, out.Message, "not partially", "the refusal says what it chose instead")
}

func TestRollback_AClampedRestoreIsRefusedNotPending(t *testing.T) {
	// An out-of-contract ticket: 768Mi over a 256Mi baseline. Restoring it is a 3x
	// decrease, which the gate clamps to nothing. A Pending action that applies
	// nothing, shown as "you may approve it yourself", is the wrong answer.
	w := newWorld(t, Config{}, webPersona("768Mi"), webDeployment("768Mi"), ticketedAt(`{"memory":"256Mi"}`, `{"memory":"768Mi"}`))
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))
	before := len(w.actions(t))

	out := w.process(t, "rb", nil)

	requireRefusedAsPartial(t, w, out, before)
}

func TestRollback_ARestoreWhereOneFieldIsClampedIsRefusedNotHalfApplied(t *testing.T) {
	// Memory was written 3x (clamped on the way back), cpu 2x (restorable). Applying
	// the cpu alone leaves memory high and cpu low: a state nobody asked for.
	w := newWorld(t, Config{}, append(withCPU("768Mi", "500m"),
		ticketedAt(`{"memory":"256Mi","cpu":"250m"}`, `{"memory":"768Mi","cpu":"500m"}`))...)
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))
	before := len(w.actions(t))

	out := w.process(t, "rb", nil)

	requireRefusedAsPartial(t, w, out, before)
	assert.Contains(t, out.Message, "memory", "it names what the gate would not restore")
}

func TestRollback_ARestoreAtExactlyTwoTimesTheBaselineConvertsWhole(t *testing.T) {
	// The regression guard for F-2: a change the gate itself issued is at most 2x its
	// baseline, so restoring it is at most a 50% decrease and is never clamped.
	pairs := [][2]string{
		{"150Mi", "300Mi"}, {"333Mi", "666Mi"}, {"700Mi", "1400Mi"}, {"7Mi", "14Mi"}, {"1Gi", "2Gi"},
		{"114Mi", "228Mi"}, {"57Mi", "114Mi"}, {"1100Mi", "2200Mi"}, {"3Mi", "6Mi"}, {"90Mi", "180Mi"},
		{"250Mi", "500Mi"}, {"350Mi", "700Mi"}, {"550Mi", "1100Mi"}, {"15Mi", "30Mi"}, {"30Mi", "60Mi"}, {"110Mi", "220Mi"},
	}
	for _, p := range pairs {
		before, wrote := p[0], p[1]
		t.Run(before+"->"+wrote, func(t *testing.T) {
			ra := ticketedAt(`{"memory":"`+before+`"}`, `{"memory":"`+wrote+`"}`)
			w := newWorld(t, Config{}, webPersona(wrote), webDeployment(wrote), ra)
			w.create(t, rollbackProposal("rb", rollbackAlice, "k"))

			out := w.process(t, "rb", nil)

			requireConvertedPending(t, out)
			require.Len(t, out.Action.Spec.Steps, 1)
			var got struct {
				Spec struct {
					Resources struct {
						Limits map[string]string `json:"limits"`
					} `json:"resources"`
				} `json:"spec"`
			}
			require.NoError(t, json.Unmarshal(out.Action.Spec.Steps[0].Patch.Raw, &got))
			assert.Equal(t, before, got.Spec.Resources.Limits["memory"], "the whole baseline is restored")
		})
	}
}

func TestRollbackIsWhole_TheGateDecisionMustBeAPassWithUnchangedSteps(t *testing.T) {
	derived := &gate.RollbackDerivation{Steps: []dorguv1.RemediationStep{{
		Order: 1, ID: "rollback-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
		Patch: jsonRaw(`{"spec":{"resources":{"limits":{"cpu":"250m","memory":"256Mi"}}}}`),
	}}}
	same := func() *gate.Decision {
		return &gate.Decision{Result: gate.ResultPass, Steps: []dorguv1.RemediationStep{{
			Order: 1, ID: "rollback-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
			// Reordered keys and spacing: the same patch.
			Patch: jsonRaw(`{ "spec": {"resources": {"limits": {"memory":"256Mi", "cpu":"250m"}}} }`),
		}}}
	}
	require.Empty(t, partialRollbackReason(same(), derived), "an identical plan is whole")

	cases := map[string]func(*gate.Decision){
		"a clamp":           func(d *gate.Decision) { d.Result = gate.ResultClamp },
		"a refusal":         func(d *gate.Decision) { d.Result = gate.ResultRefuse },
		"an unknown result": func(d *gate.Decision) { d.Result = "" },
		"a step dropped":    func(d *gate.Decision) { d.Steps = nil },
		"a step added":      func(d *gate.Decision) { d.Steps = append(d.Steps, d.Steps[0]) },
		"a field stripped":  func(d *gate.Decision) { d.Steps[0].Patch = jsonRaw(`{"spec":{"resources":{"limits":{"cpu":"250m"}}}}`) },
		"a value changed": func(d *gate.Decision) {
			d.Steps[0].Patch = jsonRaw(`{"spec":{"resources":{"limits":{"cpu":"250m","memory":"300Mi"}}}}`)
		},
		"a patch removed":          func(d *gate.Decision) { d.Steps[0].Patch = nil },
		"made non-executable":      func(d *gate.Decision) { d.Steps[0].AutoExecutable = false },
		"another step type":        func(d *gate.Decision) { d.Steps[0].Type = dorguv1.StepTypeManual },
		"a patch that is not JSON": func(d *gate.Decision) { d.Steps[0].Patch = jsonRaw(`{`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dec := same()
			mutate(dec)
			assert.NotEmpty(t, partialRollbackReason(dec, derived))
		})
	}
}

// ---- F-6: a retry finds the action it made ---------------------------------------

func TestRollback_ARetryAfterTheActionCompletedStillFindsIt(t *testing.T) {
	w := newWorld(t, Config{}, rollbackObjects()...)
	w.create(t, rollbackProposal("rb", rollbackAlice, "k"))
	first := w.process(t, "rb", nil)
	requireConvertedPending(t, first)

	// The operator died after making the action and before recording the proposal. In
	// the minutes before the retry the action was approved, applied and completed, so the
	// workload is back at its baseline and a fresh derivation would say "nothing to restore".
	ctx := context.Background()
	var rb dorguv1.RemediationAction
	require.NoError(t, w.c.Get(ctx, client.ObjectKeyFromObject(first.Action), &rb))
	rb.Status.Phase = "Completed"
	require.NoError(t, w.c.Status().Update(ctx, &rb))
	var dep appsv1.Deployment
	require.NoError(t, w.c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "web"}, &dep))
	dep.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("256Mi")
	require.NoError(t, w.c.Update(ctx, &dep))
	var pers dorguv1.ApplicationPersona
	require.NoError(t, w.c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "web"}, &pers))
	pers.Spec.Resources.Limits.Memory = "256Mi"
	require.NoError(t, w.c.Update(ctx, &pers))
	prop := w.get(t, "rb")
	prop.Status = v1alpha1.RemediationProposalStatus{Phase: v1alpha1.PhaseReceived}
	require.NoError(t, w.c.Status().Update(ctx, prop))

	retry := w.process(t, "rb", nil)

	require.Equal(t, v1alpha1.PhaseConverted, retry.Phase, "rejected: %+v: %s", retry.Rejection, retry.Message)
	assert.Equal(t, first.Action.Name, retry.Action.Name)
	assert.Len(t, w.actions(t), 2, "the original and its rollback, and no third")
}
