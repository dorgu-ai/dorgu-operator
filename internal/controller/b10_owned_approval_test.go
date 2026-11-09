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

package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B10 (b), D1.1 finding 5: approving a RemediationAction for a workload that Helm,
// Argo CD, Flux or kustomize owns must change nothing in the cluster: no persona
// patch, no workload patch. Before, the plan still carried an auto-executable
// persona-update step, and approving it patched the ApplicationPersona.

// ownedDeployment is the live Deployment with the evidence a given owner leaves on it.
func ownedDeployment(owner string) *appsv1.Deployment {
	d := lcDeployment(lcBaseline)
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	switch owner {
	case dorguv1.ManagedByHelm:
		d.Annotations["meta.helm.sh/release-name"] = "web"
		d.Annotations["meta.helm.sh/release-namespace"] = lcNamespace
	case dorguv1.ManagedByArgoCD:
		d.Annotations["argocd.argoproj.io/tracking-id"] = "web:apps/Deployment:shop/web"
	case dorguv1.ManagedByFlux:
		d.Labels["kustomize.toolkit.fluxcd.io/name"] = "apps"
	case dorguv1.ManagedByKustomize:
		d.Labels["app.kubernetes.io/managed-by"] = "kustomize"
	}
	return d
}

var deployerOwners = []string{
	dorguv1.ManagedByHelm, dorguv1.ManagedByArgoCD, dorguv1.ManagedByFlux, dorguv1.ManagedByKustomize,
}

// snapshot is the exact stored form of the persona and the Deployment.
func snapshot(t *testing.T, c client.Client) (persona, deployment string) {
	t.Helper()
	var p dorguv1.ApplicationPersona
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: lcPersona, Namespace: lcNamespace}, &p))
	var d appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: lcPersona, Namespace: lcNamespace}, &d))
	pj, err := json.Marshal(p)
	require.NoError(t, err)
	dj, err := json.Marshal(d)
	require.NoError(t, err)
	return string(pj), string(dj)
}

// An RA a 0.13.0 operator proposed for an owned workload, with its auto-executable
// persona-update step, is acknowledged on approval and nothing is written.
func TestOwned_ApprovingALegacyAutoStepChangesNothing(t *testing.T) {
	for _, owner := range deployerOwners {
		t.Run(owner, func(t *testing.T) {
			ra := lcRA(RemediationPhaseApproved)
			rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ownedDeployment(owner), ra.DeepCopy()}, lcOOMSignal())
			personaBefore, deploymentBefore := snapshot(t, c)

			_, got := lcReconcile(t, rc, ra)

			assert.Equal(t, RemediationPhaseAcknowledged, got.Status.Phase, "the human decision is recorded as acknowledged, as an advisory plan's is")
			applied := lcCondition(got, ConditionApplied)
			require.NotNil(t, applied)
			assert.Equal(t, dorguv1.ReasonAdvisoryOnly, applied.Reason)
			assert.Nil(t, got.Status.ApplyTicket, "no ticket: nothing is applied")
			assert.Nil(t, got.Status.AppliedAt)
			assert.Nil(t, lcCondition(got, dorguv1.ConditionGateRefused), "an acknowledgement is not a refusal")
			personaAfter, deploymentAfter := snapshot(t, c)
			assert.Equal(t, personaBefore, personaAfter, "the persona is byte-identical")
			assert.Equal(t, deploymentBefore, deploymentAfter, "the workload is byte-identical")
			assert.Equal(t, lcBaseline, lcPersonaMemory(t, c))
		})
	}
}

// The approval path itself: the gate's ApprovalRecheck leaves no persona patch to apply.
func TestOwned_ApprovalRecheckLeavesNothingToApply(t *testing.T) {
	for _, owner := range deployerOwners {
		t.Run(owner, func(t *testing.T) {
			ra := lcRA(RemediationPhaseApproved)
			_, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ownedDeployment(owner), ra.DeepCopy()}, lcOOMSignal())
			personaBefore, deploymentBefore := snapshot(t, c)

			decision, err := gate.New(c, noiseLogger()).Run(context.Background(), gate.ApprovalCandidate(ra), gate.PhaseApprovalRecheck)

			require.NoError(t, err)
			require.True(t, decision.Allowed(), "an acknowledgement is not refused: %s", decision.SkipReason())
			_, ticketErr := decision.Ticket()
			assert.Error(t, ticketErr, "no ticket can be issued for an owned workload")
			for _, step := range decision.Steps {
				assert.False(t, step.AutoExecutable, "step %d", step.Order)
				assert.Nil(t, step.Patch, "step %d", step.Order)
			}
			assert.True(t, decision.AdvisoryByOwnership())
			personaAfter, deploymentAfter := snapshot(t, c)
			assert.Equal(t, personaBefore, personaAfter)
			assert.Equal(t, deploymentBefore, deploymentAfter)
		})
	}
}

// Apply: a ticket on an owned workload (one a 0.13.0 operator issued, or a forged one) is not honoured.
func TestOwned_ApplyRefusesATicketOnAnOwnedWorkload(t *testing.T) {
	for _, owner := range deployerOwners {
		t.Run(owner, func(t *testing.T) {
			ra := lcRA(RemediationPhaseTicketed)
			rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ownedDeployment(owner), ra.DeepCopy()}, lcOOMSignal())
			personaBefore, deploymentBefore := snapshot(t, c)

			_, got := lcReconcile(t, rc, ra)

			assert.NotContains(t, []string{RemediationPhaseApplying, RemediationPhaseVerifying, RemediationPhaseCompleted}, got.Status.Phase)
			assert.Nil(t, got.Status.AppliedAt)
			personaAfter, deploymentAfter := snapshot(t, c)
			assert.Equal(t, personaBefore, personaAfter, "the persona is byte-identical")
			assert.Equal(t, deploymentBefore, deploymentAfter)
		})
	}
}

// The other half of the contract: where nothing owns the workload, an approval still applies.
func TestOwned_AnUnmanagedWorkloadStillApplies(t *testing.T) {
	ra := lcRA(RemediationPhaseApproved)
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), lcDeployment(lcBaseline), ra.DeepCopy()}, lcOOMSignal())

	_, got := lcReconcile(t, rc, ra)

	assert.NotEqual(t, RemediationPhaseAcknowledged, got.Status.Phase)
	assert.Equal(t, lcApplied, lcPersonaMemory(t, c), "an unmanaged workload's persona is still patched on approval")
}
