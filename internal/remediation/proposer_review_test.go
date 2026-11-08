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

package remediation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// Proposer-level tests for R7 and R11 of the #54 adversarial review.

func verdictSignature(verdicts []dorguv1.GateVerdict) []string {
	out := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		out = append(out, fmt.Sprintf("%s:%s:%s", v.Stage, v.Guardrail, v.Verdict))
	}
	return out
}

// R7: status.gate is the SEAL's decision, so it describes the plan the spec holds.
// An AI plan that is clamped by the first run and shaped afterwards used to be
// stored with the first run's verdicts, which describe a different plan.
func TestR7_StatusGateIsTheSealDecisionOfTheStoredPlan(t *testing.T) {
	p, incident := memhogWorld(t)
	p.planner = &stubPlanner{plan: &planner.RemediationPlan{
		RootCause: "memory limit too low", Confidence: 0.9,
		Steps: []planner.PlannedStep{{
			Order: 1, Type: dorguv1.StepTypePersonaUpdate, Description: "Raise memory",
			Patch: json.RawMessage(`{"spec":{"policies":{"security":{"runAsNonRoot":false}},` +
				`"resources":{"limits":{"memory":"96Mi","cpu":null}}}}`),
		}},
	}}
	diag := newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical)

	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	var stored dorguv1.RemediationAction
	require.NoError(t, p.client.Get(context.Background(), client.ObjectKeyFromObject(result.Action), &stored))
	require.NotNil(t, stored.Status.Gate)

	// An independent final run over exactly what was stored.
	again, err := p.gate.Run(context.Background(), candidateFor(&stored, diag, incident, true), gate.PhaseProposal)
	require.NoError(t, err)

	assert.Equal(t, string(again.Result), stored.Status.Gate.Result,
		"status.gate must be the decision on the stored plan, not on an earlier draft of it")
	assert.Equal(t, verdictSignature(again.Verdicts), verdictSignature(stored.Status.Gate.Verdicts))
	assert.Equal(t, string(result.Decision.Result), stored.Status.Gate.Result, "ProposalResult.Decision is the persisted one")
}

// R11: a failed status write must not report Proposed, and must not leave a
// phase-less RemediationAction behind.
func failingStatusClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	inner := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).WithObjects(objs...).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	return interceptor.NewClient(inner, interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return errors.New("the API server is unavailable")
		},
	})
}

func TestR11_FailedStatusPersistIsNotProposedAndLeavesNoPhaselessObject(t *testing.T) {
	persona := personaWithLimits(groundedPersona, "200m", "64Mi")
	deploy := liveDeployment(groundedPersona,
		map[corev1.ResourceName]string{corev1.ResourceMemory: "64Mi", corev1.ResourceCPU: "200m"},
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi", corev1.ResourceCPU: "50m"})
	incident := newTestIncident(defaultNamespace, "oom", groundedPersona, "OOMKilled")
	c := failingStatusClient(t, persona, deploy)
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.Error(t, err, "the proposal was not recorded, so it must not read as proposed")
	assert.True(t, result == nil || !result.Proposed)

	var list dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &list))
	for _, ra := range list.Items {
		assert.NotEmpty(t, ra.Status.Phase, "RemediationAction %s was left with no phase", ra.Name)
	}
	assert.Empty(t, list.Items, "an action that could not be given a status is removed")
}

func TestR11_TransientStatusFailureIsRetried(t *testing.T) {
	persona := personaWithLimits(groundedPersona, "200m", "64Mi")
	deploy := liveDeployment(groundedPersona,
		map[corev1.ResourceName]string{corev1.ResourceMemory: "64Mi", corev1.ResourceCPU: "200m"},
		map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi", corev1.ResourceCPU: "50m"})
	incident := newTestIncident(defaultNamespace, "oom", groundedPersona, "OOMKilled")
	inner := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).WithObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	failures := 0
	c := interceptor.NewClient(inner, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if failures < 1 {
				failures++
				return apierrors.NewConflict(dorguv1.GroupVersion.WithResource("remediationactions").GroupResource(), obj.GetName(), errors.New("conflict"))
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)

	require.NoError(t, err)
	require.True(t, result.Proposed)
	var stored dorguv1.RemediationAction
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(result.Action), &stored))
	assert.Equal(t, "Pending", stored.Status.Phase)
	assert.NotNil(t, stored.Status.Gate)
}
