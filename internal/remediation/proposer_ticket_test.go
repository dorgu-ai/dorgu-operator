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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// The proposer pins the spec generation the gate cleared, so an approval can tell
// a plan the operator proposed from one that was edited afterwards.
func TestProposer_RecordsTheClearedGenerationOnStatus(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())
	incident := newTestIncident(defaultNamespace, "oom", "my-app", "OOMKilled")

	result, err := p.Propose(context.Background(), newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityWarning), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	var stored dorguv1.RemediationAction
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(result.Action), &stored))
	require.NotNil(t, stored.Status.Gate)
	assert.Equal(t, "Proposal", stored.Status.Gate.Phase)
	var proposed *metav1.Condition
	for i := range stored.Status.Conditions {
		if stored.Status.Conditions[i].Type == dorguv1.ConditionProposed {
			proposed = &stored.Status.Conditions[i]
		}
	}
	require.NotNil(t, proposed, "the Proposed condition is what an approval re-check looks for")
	assert.Equal(t, metav1.ConditionTrue, proposed.Status)
	assert.Equal(t, stored.Generation, proposed.ObservedGeneration)
}

// R11: dedup, concurrency and the rate limit read the API server, and proposals
// for one persona are serialised, so racing proposals cannot both be written. The
// cached client below never sees a RemediationAction: it stands for a cache that
// lags the operator's own writes.
func TestProposer_ConcurrentProposalsForOnePersonaCreateOne(t *testing.T) {
	truth := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	laggingCache := interceptor.NewClient(truth, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, isRAs := list.(*dorguv1.RemediationActionList); isRAs {
				return nil // the cache has not caught up with anything
			}
			return inner.List(ctx, list, opts...)
		},
	})
	// The API read is slow enough that, unserialised, every racer lists before any of
	// them creates.
	slowTruth := interceptor.NewClient(truth, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			err := inner.List(ctx, list, opts...) // the snapshot is taken now ...
			if _, isRAs := list.(*dorguv1.RemediationActionList); isRAs {
				time.Sleep(20 * time.Millisecond) // ... and is stale by the time it is acted on
			}
			return err
		},
	})
	p := NewProposer(laggingCache, gate.New(laggingCache, testLogger(), gate.WithReader(slowTruth)), testLogger())

	const racers = 8
	var wg sync.WaitGroup
	proposed := make(chan bool, racers)
	for i := range racers {
		wg.Go(func() {
			incident := newTestIncident(defaultNamespace, fmt.Sprintf("oom-%d", i), "my-app", "OOMKilled")
			result, err := p.Propose(context.Background(),
				newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityWarning), incident)
			assert.NoError(t, err)
			proposed <- err == nil && result.Proposed
		})
	}
	wg.Wait()
	close(proposed)

	created := 0
	for ok := range proposed {
		if ok {
			created++
		}
	}
	var list dorguv1.RemediationActionList
	require.NoError(t, truth.List(context.Background(), &list))
	assert.Equal(t, 1, created, "exactly one racer is told it proposed")
	assert.Len(t, list.Items, 1, "exactly one RemediationAction exists for the persona")
}

func TestProposer_RuleBasedActionIsDerivedFromItsStep(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityWarning),
		newTestIncident(defaultNamespace, "oom", "my-app", "OOMKilled"))
	require.NoError(t, err)
	require.True(t, result.Proposed)

	assertRuleBasedSingleStep(t, result.Action)
}

// --- B5 fix round: F7, F9, V2 -----------------------------------------------------

// F7: spec.action is a notification only. A pre-0.14 CLI applies the patch it finds
// in spec.action without a ticket; there must be none to find.
func TestF7_TheProposedSpecActionCarriesNoPatch(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityWarning),
		newTestIncident(defaultNamespace, "oom", "my-app", "OOMKilled"))
	require.NoError(t, err)
	require.True(t, result.Proposed)

	var stored dorguv1.RemediationAction
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(result.Action), &stored))
	assert.Equal(t, dorguv1.ActionTypeNotification, stored.Spec.Action.Type)
	assert.Nil(t, stored.Spec.Action.Patch, "no patch for an old CLI to apply")
	assert.Nil(t, stored.Spec.Action.PrePatchState)
	require.Len(t, stored.Spec.Steps, 1)
	require.NotNil(t, stored.Spec.Steps[0].Patch, "steps[] is the only plan and still carries the change")
	assert.True(t, stored.HasAutoApplicableChange())
}

func TestF7_AnAIPlansSpecActionCarriesNoPatchEither(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(&stubPlanner{plan: threeStepPlan()}))

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical),
		newTestIncident(defaultNamespace, "oom", "my-app", "OOMKilled"))
	require.NoError(t, err)
	require.True(t, result.Proposed)

	var stored dorguv1.RemediationAction
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(result.Action), &stored))
	assert.Equal(t, dorguv1.ActionTypeNotification, stored.Spec.Action.Type)
	assert.Nil(t, stored.Spec.Action.Patch)
	assert.True(t, stored.Spec.Steps[0].AutoExecutable)
}

// F9: the Proposed condition pins the generation Create returned. An edit that
// lands between Create and the status write must not be baked into the pin.
func TestF9_TheClearedGenerationIsTheOneCreateReturned(t *testing.T) {
	truth := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	c := interceptor.NewClient(truth, interceptor.Funcs{
		Create: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := inner.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*dorguv1.RemediationAction); ok {
				obj.SetGeneration(1) // what the apiserver answers a Create with
			}
			return nil
		},
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := inner.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*dorguv1.RemediationAction); ok {
				obj.SetGeneration(2) // somebody edited the spec after Create returned
			}
			return nil
		},
	})
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())

	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityWarning),
		newTestIncident(defaultNamespace, "oom", "my-app", "OOMKilled"))
	require.NoError(t, err)
	require.True(t, result.Proposed)

	var stored dorguv1.RemediationAction
	require.NoError(t, truth.Get(context.Background(), client.ObjectKeyFromObject(result.Action), &stored))
	var proposed *metav1.Condition
	for i := range stored.Status.Conditions {
		if stored.Status.Conditions[i].Type == dorguv1.ConditionProposed {
			proposed = &stored.Status.Conditions[i]
		}
	}
	require.NotNil(t, proposed)
	assert.Equal(t, int64(1), proposed.ObservedGeneration, "the pin is what Create returned, not what a later Get saw")
}

// V2 (B4 review): the rules say "fix the deployment" for an image-pull failure and
// the model answers with a memory persona-update. The gate's action-class check
// refuses it; nothing is proposed and no model-chosen persona change is applied.
func TestV2_AModelPersonaUpdateForAnImagePullDiagnosisIsRefused(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	memoryPlan := &planner.RemediationPlan{
		RootCause: "image cannot be pulled; raise memory", Confidence: 0.9,
		Steps: []planner.PlannedStep{{
			Order: 1, Type: dorguv1.StepTypePersonaUpdate, Description: "raise memory", Rationale: "r", Risk: "low",
			Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`),
		}},
	}
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(&stubPlanner{plan: memoryPlan}))
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalImagePullBackOff
	diag.SuggestedAction = "deployment-fix" // what the rule engine says for image-pull

	result, err := p.Propose(context.Background(), diag,
		newTestIncident(defaultNamespace, "imagepull", "my-app", "ImagePullBackOff"))
	require.NoError(t, err)

	assert.False(t, result.Proposed, "the plan's class (persona-update) is not the rules' (deployment-fix)")
	require.NotNil(t, result.Decision)
	_, guardrail := result.Decision.RefusedBy()
	assert.Equal(t, gate.GuardrailActionClass, guardrail)
	var list dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Empty(t, list.Items, "nothing was written")
}
