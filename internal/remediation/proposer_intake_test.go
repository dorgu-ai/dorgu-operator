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
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

const operatorUser = "system:serviceaccount:dorgu-system:dorgu-operator"

// ticking stamps every created object one second after the last, as a real API
// server does: the intake orders proposals by creation time.
func ticking(base client.WithWatch) client.WithWatch {
	var clock atomic.Int64
	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetCreationTimestamp(metav1.NewTime(time.Unix(1_700_000_000+clock.Add(1), 0)))
			return c.Create(ctx, obj, opts...)
		},
	})
}

func proposerWorld(t *testing.T, mutate func(*intake.Config), personas ...string) (*Proposer, client.Client) {
	t.Helper()
	objs := []client.Object{}
	for _, name := range personas {
		objs = append(objs, newTestPersona(defaultNamespace, name))
	}
	base := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objs...).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}).Build()
	c := ticking(base)
	g := gate.New(c, testLogger())
	cfg := intake.Config{Client: c, Gate: g, OperatorUsername: operatorUser, Logger: testLogger()}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewProposer(c, g, testLogger(), WithIntake(intake.New(cfg))), c
}

func proposals(t *testing.T, c client.Client) []dorguv1alpha1.RemediationProposal {
	t.Helper()
	var list dorguv1alpha1.RemediationProposalList
	require.NoError(t, c.List(context.Background(), &list))
	return list.Items
}

// C1: Dorgu's planner writes a RemediationProposal as the operator ServiceAccount
// and the action it gets is the proposal converted, not a RemediationAction the
// Proposer created itself (P2-D1, D3).
func TestPropose_WritesAProposalAsTheOperatorAndGetsAnActionBack(t *testing.T) {
	p, c := proposerWorld(t, nil, "my-app")
	incident := newTestIncident(defaultNamespace, "oom-c1", "my-app", "OOMKilled")

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed, result.SkipReason)

	props := proposals(t, c)
	require.Len(t, props, 1, "the planner's plan is one proposal")
	prop := props[0]
	assert.Equal(t, result.Action.Name, prop.Name, "the action carries its proposal's name")
	assert.Equal(t, operatorUser, prop.Spec.Proposer.Username, "Dorgu's planner proposes as the operator ServiceAccount")
	assert.Equal(t, dorguv1.ProposalSourceDorguPlanner, prop.Spec.Source)
	assert.Equal(t, result.Action.Name, prop.Spec.IdempotencyKey)
	assert.Equal(t, incident.Name, prop.Spec.IncidentRef.Name)
	assert.Equal(t, dorguv1.PersonaReference{Kind: kindApplicationPersona, Name: "my-app", Namespace: defaultNamespace},
		dorguv1.PersonaReference{Kind: prop.Spec.Target.Kind, Name: prop.Spec.Target.Name, Namespace: prop.Spec.Target.Namespace})
	require.Len(t, prop.Spec.Steps, 1)
	assert.Equal(t, dorguv1.StepTypePersonaUpdate, prop.Spec.Steps[0].Type)

	assert.Equal(t, dorguv1alpha1.PhaseConverted, prop.Status.Phase)
	require.NotNil(t, prop.Status.RemediationActionRef)
	assert.Equal(t, result.Action.Name, prop.Status.RemediationActionRef.Name)

	var ra dorguv1.RemediationAction
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(result.Action), &ra))
	assert.Equal(t, "Pending", ra.Status.Phase)
	assert.Equal(t, &dorguv1.RemediationProvenance{
		Proposal: prop.Name,
		Proposer: dorguv1.ProposerIdentity{Username: operatorUser},
		Source:   dorguv1.ProposalSourceDorguPlanner,
		Agent:    &dorguv1.AgentInfo{Name: "dorgu-operator"},
	}, ra.Spec.Provenance)
	assert.Equal(t, operatorUser, ra.Spec.ProposerOrDefault("someone-else"))
}

// The user sees the action the planner shaped: what a bare proposal cannot carry
// (confidence, plan source, the signal it answers) is what the planner wrote.
func TestPropose_TheActionIsWhatThePlannerShaped(t *testing.T) {
	p, _ := proposerWorld(t, nil, "my-app")
	incident := newTestIncident(defaultNamespace, "oom-shape", "my-app", "OOMKilled")
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)

	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	require.True(t, result.Proposed, result.SkipReason)

	ra := result.Action
	// 0.90 from the diagnosis, damped by the planner because a critical memory fix
	// sits at the blast-radius cap. The intake's own fallback would have said "0".
	assert.Equal(t, "0.72", ra.Spec.Confidence, "the planner's confidence reaches the action")
	assert.Equal(t, dorguv1.PlanSourceRuleBased, ra.Spec.PlanSource)
	assert.Equal(t, string(detection.SignalOOMKilled), ra.Annotations[gate.AnnotationAnswersSignal])
	assert.Equal(t, "my-app", ra.Labels["dorgu.io/persona-name"])
	assert.True(t, ra.Spec.Approval.Required)
	assert.NotEmpty(t, ra.Spec.Explanation)
	require.NotNil(t, ra.Status.Gate)
	assert.Equal(t, string(gate.PhaseProposal), ra.Status.Gate.Phase)
	assert.Equal(t, dorguv1.ActionTypeNotification, ra.Spec.Action.Type)
}

// The intake's rate limit also holds for Dorgu's own planner, and a refusal at
// Intake reports its own stage and reason (P2-D12).
func TestPropose_IntakeRateLimitRefusesThePlannersNextProposal(t *testing.T) {
	p, c := proposerWorld(t, func(cfg *intake.Config) { cfg.RateLimit = 1 }, "app-a", "app-b")

	first, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "app-a", detection.SeverityCritical),
		newTestIncident(defaultNamespace, "oom-a", "app-a", "OOMKilled"))
	require.NoError(t, err)
	require.True(t, first.Proposed, first.SkipReason)

	second, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "app-b", detection.SeverityCritical),
		newTestIncident(defaultNamespace, "oom-b", "app-b", "OOMKilled"))
	require.NoError(t, err)
	assert.False(t, second.Proposed)
	assert.Equal(t, dorguv1alpha1.StageIntake, second.Stage)
	assert.Equal(t, dorguv1alpha1.ReasonRateLimited, second.Reason)

	var rejected int
	for _, prop := range proposals(t, c) {
		if prop.Status.Phase == dorguv1alpha1.PhaseRejected {
			rejected++
			assert.Equal(t, dorguv1alpha1.ReasonRateLimited, prop.Status.Rejection.Reason)
		}
	}
	assert.Equal(t, 1, rejected, "the refusal is recorded on the proposal, where an agent would read it")
}

// A plan that cannot be recorded is not proposed, and no proposal is left behind to
// be decided later without what the planner knew (R11).
func TestPropose_AFailedActionWriteLeavesNoProposalAndNoAction(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isAction := obj.(*dorguv1.RemediationAction); isAction {
				return errors.New("etcd is on fire")
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	g := gate.New(c, testLogger())
	p := NewProposer(c, g, testLogger(), WithIntake(intake.New(intake.Config{Client: c, Gate: g, Logger: testLogger()})))

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical),
		newTestIncident(defaultNamespace, "oom-fail", "my-app", "OOMKilled"))

	require.Error(t, err)
	assert.ErrorIs(t, err, errPersistFailed)
	assert.Nil(t, result)
	assert.Empty(t, proposals(t, c), "the undecided proposal is removed")
	var actions dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &actions))
	assert.Empty(t, actions.Items)
}

// The proposal carries the planner's explanation as its rationale, bounded to what
// the API server accepts, and an empty one is given a sentence (a rationale is
// required).
func TestRationaleOf(t *testing.T) {
	assert.Equal(t, "Dorgu proposed this plan.", rationaleOf(""))
	assert.Equal(t, "memory is low", rationaleOf("memory is low"))
	long := make([]rune, dorguv1alpha1.MaxRationaleLength+500)
	for i := range long {
		long[i] = 'é'
	}
	assert.Len(t, []rune(rationaleOf(string(long))), dorguv1alpha1.MaxRationaleLength, "counted in characters, as the API server counts")
}
