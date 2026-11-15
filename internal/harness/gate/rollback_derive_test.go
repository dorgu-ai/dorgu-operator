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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// C6 (P2-D10): the operator derives a ticketed rollback's steps from what it recorded
// before it applied an action: status.gate.prePatchState (the live values the gate
// measured at approval) and status.applyTicket (the fields and values it wrote). Never
// from the spec, never from the proposer.

const (
	wroteMemory  = "512Mi"
	beforeMemory = "256Mi"
)

// appliedAction is a Completed action whose ticket raised memory from 256Mi to 512Mi.
func appliedAction(mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: "ra-applied", Namespace: testNS, UID: "ra-uid"},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef: target(),
			Provenance: &dorguv1.RemediationProvenance{
				Proposal: "p", Proposer: dorguv1.ProposerIdentity{Username: "system:serviceaccount:dorgu-system:dorgu-operator"},
				Source: dorguv1.ProposalSourceDorguPlanner,
			},
		},
		Status: dorguv1.RemediationActionStatus{
			Phase: "Completed",
			Gate: &dorguv1.GateStatus{
				Phase: "ApprovalRecheck", Result: "Pass",
				PrePatchState: memPatchBody(beforeMemory),
			},
			ApplyTicket: &dorguv1.ApplyTicket{
				WorkloadUID: "uid-1", Container: testPersona, Patch: memPatch(wroteMemory),
			},
		},
	}
	for _, m := range mutate {
		m(ra)
	}
	return ra
}

// memPatchBody is the baseline shape: the same as a patch.
func memPatchBody(v string) *apiextensionsv1.JSON { return memPatch(v) }

func rollbackGate(t *testing.T, objs ...client.Object) *Gate {
	t.Helper()
	return newTestGate(t, fakeClient(t, objs...))
}

func derive(t *testing.T, g *Gate, ra *dorguv1.RemediationAction) (*RollbackDerivation, *RollbackProblem) {
	t.Helper()
	d, problem, err := g.DeriveRollback(context.Background(), ra)
	require.NoError(t, err)
	return d, problem
}

func requireRefused(t *testing.T, problem *RollbackProblem, reason string) {
	t.Helper()
	require.NotNil(t, problem, "the rollback must be refused with %s", reason)
	assert.Equal(t, reason, problem.Reason, problem.Message)
	assert.NotEmpty(t, problem.Message)
}

func TestDeriveRollback_RestoresTheRecordedValuesOnACompletedAction(t *testing.T) {
	g := rollbackGate(t, deployment(wroteMemory), persona(wroteMemory))

	d, problem := derive(t, g, appliedAction())

	require.Nil(t, problem)
	require.Len(t, d.Steps, 1)
	step := d.Steps[0]
	assert.Equal(t, dorguv1.StepTypePersonaUpdate, step.Type)
	assert.True(t, step.AutoExecutable)
	assert.Equal(t, beforeMemory, stepPatchValue(t, step), "the patch restores what the gate recorded before the change")
	assert.Nil(t, step.PrePatchState, "the gate computes its own baseline")
	assert.Empty(t, step.Safety)
	assert.Equal(t, []string{memLimit}, d.Restored)
	assert.Equal(t, int32(1), step.Order)
	assert.NotEmpty(t, step.ID)
	assert.NotEmpty(t, step.Description)
}

func TestDeriveRollback_AlsoServesAnActionTheAutoRollbackLeftOnTheWorkload(t *testing.T) {
	// The operator's own rollback restores the persona, never the Deployment: the
	// workload still carries the change, and that is what a ticketed rollback finishes.
	g := rollbackGate(t, deployment(wroteMemory), persona(beforeMemory))

	d, problem := derive(t, g, appliedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Phase = "RolledBack" }))

	require.Nil(t, problem)
	assert.Equal(t, beforeMemory, stepPatchValue(t, d.Steps[0]))
}

func TestDeriveRollback_OnlyACompletedOrRolledBackActionApplied(t *testing.T) {
	g := rollbackGate(t, deployment(wroteMemory), persona(wroteMemory))
	for _, phase := range []string{
		"", "Pending", "Approved", "Ticketed", "Applying", "Verifying", "Acknowledged", "Failed", "Rejected", "Expired",
	} {
		t.Run("phase "+phase, func(t *testing.T) {
			_, problem := derive(t, g, appliedAction(func(ra *dorguv1.RemediationAction) { ra.Status.Phase = phase }))
			requireRefused(t, problem, RollbackReasonNotApplied)
		})
	}
}

func TestDeriveRollback_AnActionWithNoTicketNeverAppliedAnything(t *testing.T) {
	g := rollbackGate(t, deployment(wroteMemory), persona(wroteMemory))

	_, problem := derive(t, g, appliedAction(func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket = nil }))

	requireRefused(t, problem, RollbackReasonNotApplied)
}

func TestDeriveRollback_ARollbackIsNotRolledBack(t *testing.T) {
	g := rollbackGate(t, deployment(beforeMemory), persona(beforeMemory))

	_, problem := derive(t, g, appliedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Provenance.Source = dorguv1.ProposalSourceRollback
	}))

	requireRefused(t, problem, RollbackReasonOfRollback)
}

func TestDeriveRollback_RefusesWhenTheOperatorRecordedNothingToRestore(t *testing.T) {
	g := rollbackGate(t, deployment(wroteMemory), persona(wroteMemory))
	cases := map[string]func(*dorguv1.RemediationAction){
		"no gate record": func(ra *dorguv1.RemediationAction) { ra.Status.Gate = nil },
		"no baseline":    func(ra *dorguv1.RemediationAction) { ra.Status.Gate.PrePatchState = nil },
		"an empty baseline": func(ra *dorguv1.RemediationAction) {
			ra.Status.Gate.PrePatchState = jsonOf(`{}`)
		},
		"a baseline for another field": func(ra *dorguv1.RemediationAction) {
			ra.Status.Gate.PrePatchState = jsonOf(`{"spec":{"resources":{"limits":{"cpu":"250m"}}}}`)
		},
		"a baseline outside the allowlist": func(ra *dorguv1.RemediationAction) {
			ra.Status.Gate.PrePatchState = jsonOf(`{"spec":{"policies":{"security":{"runAsNonRoot":false}}}}`)
		},
		"a ticket that changed no resource field": func(ra *dorguv1.RemediationAction) {
			ra.Status.ApplyTicket.Patch = jsonOf(`{"spec":{"tier":"critical"}}`)
		},
		"a step prePatchState is not the record": func(ra *dorguv1.RemediationAction) {
			ra.Status.Gate = nil
			ra.Spec.Steps = []dorguv1.RemediationStep{{
				Order: 1, ID: "s", Type: dorguv1.StepTypePersonaUpdate, PrePatchState: memPatch("128Mi"), Patch: memPatch(wroteMemory),
			}}
			ra.Spec.Action = dorguv1.RemediationActionDetail{
				Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch(wroteMemory), PrePatchState: memPatch("128Mi"),
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, problem := derive(t, g, appliedAction(mutate))
			requireRefused(t, problem, RollbackReasonNoRecordedState)
		})
	}
}

func TestDeriveRollback_TheSpecsOwnPrePatchStateIsNeverTheBaseline(t *testing.T) {
	g := rollbackGate(t, deployment(wroteMemory), persona(wroteMemory))

	d, problem := derive(t, g, appliedAction(func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action = dorguv1.RemediationActionDetail{
			Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch(wroteMemory), PrePatchState: memPatch("1Mi"),
		}
		ra.Spec.Steps = []dorguv1.RemediationStep{{
			Order: 1, ID: "s", Type: dorguv1.StepTypePersonaUpdate, Patch: memPatch(wroteMemory), PrePatchState: memPatch("1Mi"),
		}}
	}))

	require.Nil(t, problem)
	assert.Equal(t, beforeMemory, stepPatchValue(t, d.Steps[0]), "the restore comes from status.gate, which only the operator writes")
}

func TestDeriveRollback_RefusesWhenTheWorkloadDrifted(t *testing.T) {
	two := func(name string) *appsv1.Deployment {
		d := deployment(wroteMemory)
		d.Name = name
		d.UID = "uid-other"
		return d
	}
	cases := map[string]struct {
		objs []client.Object
	}{
		"a person raised memory further":   {[]client.Object{deployment("768Mi"), persona(wroteMemory)}},
		"a person lowered memory":          {[]client.Object{deployment("300Mi"), persona(wroteMemory)}},
		"the Deployment was replaced":      {[]client.Object{deployment(wroteMemory, func(d *appsv1.Deployment) { d.UID = "uid-2" }), persona(wroteMemory)}},
		"the Deployment is gone":           {[]client.Object{persona(wroteMemory)}},
		"two Deployments now answer":       {[]client.Object{deployment(wroteMemory), two("web-canary"), persona(wroteMemory)}},
		"the persona was edited":           {[]client.Object{deployment(wroteMemory), persona("1Gi")}},
		"the persona is gone":              {[]client.Object{deployment(wroteMemory)}},
		"the persona holds no memory":      {[]client.Object{deployment(wroteMemory), persona("")}},
		"the container is not the tickets": {[]client.Object{deployment(wroteMemory, renameContainer("sidecar")), persona(wroteMemory)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, problem := derive(t, rollbackGate(t, tc.objs...), appliedAction())
			requireRefused(t, problem, RollbackReasonWorkloadDrifted)
		})
	}
}

func renameContainer(name string) func(*appsv1.Deployment) {
	return func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers = append([]corev1.Container{{Name: name}}, d.Spec.Template.Spec.Containers[0])
		d.Spec.Template.Spec.Containers[1].Name = "other"
	}
}

func TestDeriveRollback_NothingToRestoreWhenTheWorkloadAlreadyHoldsTheBaseline(t *testing.T) {
	g := rollbackGate(t, deployment(beforeMemory), persona(beforeMemory))

	_, problem := derive(t, g, appliedAction())

	requireRefused(t, problem, RollbackReasonNothingToRestore)
}

func TestDeriveRollback_ComparesQuantitiesNotStrings(t *testing.T) {
	g := rollbackGate(t, deployment(wroteMemory), persona("0.5Gi"))

	d, problem := derive(t, g, appliedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.Patch = memPatch("0.5Gi")
	}))

	require.Nil(t, problem)
	assert.Equal(t, beforeMemory, stepPatchValue(t, d.Steps[0]))
}

func TestDeriveRollback_RestoresOnlyTheFieldsStillHoldingTheChange(t *testing.T) {
	// Memory and CPU were changed; the CPU limit was already put back by hand.
	dep := deployment(wroteMemory, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("250m")
	})
	pers := persona(wroteMemory)
	pers.Spec.Resources.Limits.CPU = "250m"
	g := rollbackGate(t, dep, pers)

	d, problem := derive(t, g, appliedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.Patch = jsonOf(`{"spec":{"resources":{"limits":{"memory":"512Mi","cpu":"500m"}}}}`)
		ra.Status.Gate.PrePatchState = jsonOf(`{"spec":{"resources":{"limits":{"memory":"256Mi","cpu":"250m"}}}}`)
	}))

	require.Nil(t, problem, "a field already at its baseline is not drift")
	assert.Equal(t, []string{memLimit}, d.Restored)
	assert.NotContains(t, string(d.Steps[0].Patch.Raw), "cpu")
}

func TestDeriveRollback_AnUnreadableClusterIsAnErrorNotAVerdict(t *testing.T) {
	failing := interceptor.NewClient(fakeClient(t, deployment(wroteMemory), persona(wroteMemory)), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("the API server is unavailable")
		},
	})
	g := newTestGate(t, failing)

	d, problem, err := g.DeriveRollback(context.Background(), appliedAction())

	require.Error(t, err)
	assert.Nil(t, d)
	assert.Nil(t, problem, "a transient error must be retried, not recorded as a refusal")
}

func TestDeriveRollback_AnUnreadablePersonaIsAnErrorNotAVerdict(t *testing.T) {
	failing := interceptor.NewClient(fakeClient(t, deployment(wroteMemory), persona(wroteMemory)), interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*dorguv1.ApplicationPersona); ok {
				return errors.New("the API server is unavailable")
			}
			return inner.Get(ctx, key, obj, opts...)
		},
	})
	g := newTestGate(t, failing)

	d, problem, err := g.DeriveRollback(context.Background(), appliedAction())

	require.Error(t, err)
	assert.Nil(t, d)
	assert.Nil(t, problem, "a transient error must be retried, not recorded as a refusal")
}

func TestDeriveRollback_TheDerivedPlanClearsTheGate(t *testing.T) {
	g := rollbackGate(t, deployment(wroteMemory), persona(wroteMemory), clusterPersona(nil))
	d, problem := derive(t, g, appliedAction())
	require.Nil(t, problem)

	cand := planCandidate(d.Steps...)
	cand.Facts = Facts{SuggestedAction: ClassResourceAdjustment}
	dec := run(t, g, cand, PhaseProposal)

	require.True(t, dec.Allowed(), dec.SkipReason())
	require.Len(t, dec.Steps, 1)
	assert.Equal(t, beforeMemory, stepPatchValue(t, dec.Steps[0]))
	assert.True(t, dec.Steps[0].AutoExecutable)
}
