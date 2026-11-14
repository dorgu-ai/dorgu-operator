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
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

const (
	testNS       = "apps"
	testOperator = "system:serviceaccount:dorgu-system:dorgu-operator"
)

type fakeOpener struct {
	incident *dorguv1.IncidentMemory
	err      error
	calls    int
}

func (f *fakeOpener) Open(_ context.Context, _ *v1alpha1.RemediationProposal) (*dorguv1.IncidentMemory, error) {
	f.calls++
	return f.incident, f.err
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	return s
}

func persona(name string) *dorguv1.ApplicationPersona {
	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: dorguv1.ApplicationPersonaSpec{Name: name,
			Resources: &dorguv1.ResourceConstraints{Limits: &dorguv1.ResourceValues{Memory: "256Mi", CPU: "250m"}}},
	}
}

func incident(name, signal string) *dorguv1.IncidentMemory {
	return &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "web", Namespace: testNS},
			Category:   "resource", Severity: "critical",
			Detection: dorguv1.DetectionInfo{Signal: signal},
		},
	}
}

func manualStep() dorguv1.RemediationStep {
	return dorguv1.RemediationStep{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "look at the heap"}
}

func proposal(name, user, key string) *v1alpha1.RemediationProposal {
	return &v1alpha1.RemediationProposal{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: v1alpha1.RemediationProposalSpec{
			Proposer:       dorguv1.ProposerIdentity{Username: user},
			IdempotencyKey: key,
			IncidentRef:    &dorguv1.IncidentReference{Name: "inc", Namespace: testNS},
			Target:         v1alpha1.ProposalTarget{Kind: "ApplicationPersona", Namespace: testNS, Name: "web"},
			Steps:          []dorguv1.RemediationStep{manualStep()},
			Rationale:      "why",
		},
	}
}

type world struct {
	c    client.Client
	p    *Processor
	open *fakeOpener
}

// newWorld builds a Processor over a fake cluster. Objects created through it get
// a creation time one second after the last, as a real API server gives them.
func newWorld(t *testing.T, cfg Config, objs ...client.Object) *world {
	t.Helper()
	base := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}, &dorguv1.IncidentMemory{}).Build()
	var clock atomic.Int64
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetCreationTimestamp(metav1.NewTime(time.Unix(1_700_000_000+clock.Add(1), 0)))
			return cl.Create(ctx, obj, opts...)
		},
	})
	w := &world{c: c, open: &fakeOpener{incident: incident("opened", "ProposerReported")}}
	cfg.Client, cfg.Logger = c, logr.Discard()
	if cfg.Gate == nil {
		cfg.Gate = gate.New(c, logr.Discard())
	}
	if cfg.Incidents == nil {
		cfg.Incidents = w.open
	}
	w.p = New(cfg)
	return w
}

func (w *world) create(t *testing.T, obj client.Object) {
	t.Helper()
	require.NoError(t, w.c.Create(context.Background(), obj))
}

func (w *world) process(t *testing.T, name string, hint *Hint) *Outcome {
	t.Helper()
	out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, hint)
	require.NoError(t, err)
	return out
}

func (w *world) get(t *testing.T, name string) *v1alpha1.RemediationProposal {
	t.Helper()
	var p v1alpha1.RemediationProposal
	require.NoError(t, w.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &p))
	return &p
}

func baseObjects() []client.Object {
	return []client.Object{persona("web"), incident("inc", "ImagePullBackOff")}
}

func TestNew_Defaults(t *testing.T) {
	p := New(Config{Logger: logr.Discard()})
	assert.Equal(t, DefaultOperatorUsername, p.OperatorUsername())
	assert.Equal(t, DefaultRejectedTTL, p.RejectedTTL())
	assert.NotNil(t, p.Locks())
	assert.WithinDuration(t, time.Now(), p.Now(), time.Second)
	assert.Equal(t, DefaultRateLimit, p.rateLimit)
	assert.Equal(t, DefaultRateWindow, p.rateWindow)
}

func TestOperatorUsername(t *testing.T) {
	t.Setenv(ServiceAccountEnv, "")
	assert.Equal(t, "", OperatorUsername("dorgu-system"), "no service account name: unknown")
	t.Setenv(ServiceAccountEnv, "dorgu-operator")
	assert.Equal(t, "", OperatorUsername(""), "no namespace: unknown")
	assert.Equal(t, "system:serviceaccount:dorgu-system:dorgu-operator", OperatorUsername("dorgu-system"))
}

func TestProcess_MissingAndDeletedProposalsAreNoOps(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	assert.Nil(t, w.process(t, "ghost", nil))

	gone := proposal("going", "alice", "k")
	gone.Finalizers = []string{"test/hold"}
	w.create(t, gone)
	require.NoError(t, w.c.Delete(context.Background(), gone))
	assert.Nil(t, w.process(t, "going", nil), "a proposal being deleted is not decided")
}

func TestProcess_ConvertsAndIsIdempotent(t *testing.T) {
	w := newWorld(t, Config{OperatorUsername: testOperator}, baseObjects()...)
	w.create(t, proposal("p1", "alice", "k1"))

	out := w.process(t, "p1", nil)
	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	require.NotNil(t, out.Action)
	assert.Equal(t, "Pending", out.Action.Status.Phase)
	assert.Equal(t, "why", out.Action.Spec.Explanation)
	assert.Equal(t, "0", out.Action.Spec.Confidence, "an incident with no diagnosis gives the fallback")
	assert.Equal(t, "ImagePullBackOff", out.Action.Annotations[gate.AnnotationAnswersSignal])
	assert.Equal(t, dorguv1.ProposalSourceAgent, out.Action.Spec.Provenance.Source, "the CRD default is applied when empty")

	again := w.process(t, "p1", nil)
	assert.True(t, again.Replayed)
	assert.Equal(t, v1alpha1.PhaseConverted, again.Phase)

	var actions dorguv1.RemediationActionList
	require.NoError(t, w.c.List(context.Background(), &actions))
	assert.Len(t, actions.Items, 1)
}

func TestProcess_AnActionFromAnEarlierAttemptIsAdopted(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-adopt", "alice", "k"))
	first := w.process(t, "p-adopt", nil)
	require.Equal(t, v1alpha1.PhaseConverted, first.Phase)

	// The operator died after creating the action and before recording the proposal as converted.
	prop := w.get(t, "p-adopt")
	prop.Status = v1alpha1.RemediationProposalStatus{Phase: v1alpha1.PhaseReceived}
	require.NoError(t, w.c.Status().Update(context.Background(), prop))

	retry := w.process(t, "p-adopt", nil)
	require.Equal(t, v1alpha1.PhaseConverted, retry.Phase)
	assert.Equal(t, first.Action.Name, retry.Action.Name, "the same action, not a second one")
	var actions dorguv1.RemediationActionList
	require.NoError(t, w.c.List(context.Background(), &actions))
	assert.Len(t, actions.Items, 1)
}

func TestProcess_AnActionWithNoStatusIsRemovedAndDecidedAfresh(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-unfinished", "alice", "k"))
	prop := w.get(t, "p-unfinished")
	orphan := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	orphan.Spec.Provenance = provenanceFor(prop)
	orphan.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
	w.create(t, orphan)

	out := w.process(t, "p-unfinished", nil)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Equal(t, "Pending", out.Action.Status.Phase, "the new action was proposed properly")
}

func TestProcess_NoIncidentRefOpensOne(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	p := proposal("p-open", "alice", "k")
	p.Spec.IncidentRef = nil
	w.create(t, p)

	out := w.process(t, "p-open", nil)

	assert.Equal(t, 1, w.open.calls)
	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Equal(t, "opened", w.get(t, "p-open").Status.IncidentRef.Name)
}

func TestProcess_IncidentFailuresRetryOrReject(t *testing.T) {
	t.Run("no opener configured is an error to retry", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		w.p.incidents = nil
		p := proposal("p", "alice", "k")
		p.Spec.IncidentRef = nil
		w.create(t, p)
		_, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"}, nil)
		require.Error(t, err)
		assert.Equal(t, v1alpha1.PhaseReceived, w.get(t, "p").Status.Phase, "not decided: it will be retried")
	})
	t.Run("opener error is an error to retry", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		w.open.err = errors.New("apiserver down")
		p := proposal("p", "alice", "k")
		p.Spec.IncidentRef = nil
		w.create(t, p)
		_, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"}, nil)
		require.ErrorContains(t, err, "apiserver down")
	})
	t.Run("an incident in another namespace reads as absent", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		p := proposal("p", "alice", "k")
		p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "inc", Namespace: "other"}
		w.create(t, p)
		out := w.process(t, "p", nil)
		assert.Equal(t, v1alpha1.ReasonIncidentNotFound, out.Rejection.Reason)
	})
	t.Run("a read error is an error to retry", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		w.create(t, proposal("p", "alice", "k"))
		failing := interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*dorguv1.IncidentMemory); ok {
					return errors.New("boom")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		})
		w.p.reader = failing
		_, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"}, nil)
		require.ErrorContains(t, err, "boom")
	})
}

func TestProcess_GateRefusalIsRecordedWithItsStageAndReason(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	p := proposal("p-refused", "alice", "k")
	p.Spec.Steps = []dorguv1.RemediationStep{{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
		Patch: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`)},
	}}
	w.create(t, p)

	out := w.process(t, "p-refused", nil)

	require.Equal(t, v1alpha1.PhaseRejected, out.Phase)
	assert.Equal(t, gate.StageValidate, out.Rejection.Stage)
	assert.Equal(t, gate.GuardrailActionClass, out.Rejection.Reason, "ImagePullBackOff is not a resource problem")
	assert.NotNil(t, out.Decision)
	got := w.get(t, "p-refused")
	assert.Equal(t, v1alpha1.PhaseRejected, got.Status.Phase)
	require.Len(t, got.Status.Conditions, 1)
	assert.Equal(t, "ActionClass", got.Status.Conditions[0].Reason, "a guardrail token is a valid CamelCase condition reason")
}

func TestProcess_AMismatchedHintIsIgnored(t *testing.T) {
	w := newWorld(t, Config{OperatorUsername: testOperator}, baseObjects()...)
	w.create(t, proposal("p-hint", "alice", "k"))
	prop := w.get(t, "p-hint")

	draft := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	draft.Spec.Confidence = "0.99"
	draft.Spec.Steps = []dorguv1.RemediationStep{{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "something else"}}

	out := w.process(t, "p-hint", &Hint{Draft: draft, Facts: gate.Facts{Incident: incident("inc", "ImagePullBackOff")}})

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Equal(t, "0", out.Action.Spec.Confidence, "a draft that is not this proposal's plan is not believed")
	assert.Equal(t, "look at the heap", out.Action.Spec.Steps[0].Description)
}

func TestProcess_AMatchingHintKeepsThePlannersShape(t *testing.T) {
	w := newWorld(t, Config{OperatorUsername: testOperator}, baseObjects()...)
	w.create(t, proposal("p-hint", "alice", "k"))
	prop := w.get(t, "p-hint")
	draft := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	draft.Name = "ra-planner-named"
	draft.Spec.Confidence = "0.77"
	draft.Spec.PlanSource = dorguv1.PlanSourceRuleBased
	release := w.p.Claim(types.NamespacedName{Namespace: testNS, Name: "p-hint"})
	assert.True(t, w.p.Claimed(types.NamespacedName{Namespace: testNS, Name: "p-hint"}))
	defer release()

	// The planner's record of its own decision: what status.gate keeps when the intake's run agrees.
	planners, err := w.p.gate.Run(context.Background(), candidateFor(draft, gate.Facts{}), gate.PhaseProposal)
	require.NoError(t, err)

	out := w.process(t, "p-hint", &Hint{Draft: draft, Decision: planners,
		Facts: gate.Facts{Incident: incident("not-stored", "ImagePullBackOff")}})

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Equal(t, "ra-planner-named", out.Action.Name)
	assert.Equal(t, "0.77", out.Action.Spec.Confidence)
	assert.Same(t, planners, out.Decision, "the planner's own decision is the record")
	assert.Equal(t, "not-stored", w.get(t, "p-hint").Status.IncidentRef.Name, "an incident the planner passes in need not be re-read")
}

func TestClaim_ReleaseLetsGo(t *testing.T) {
	p := New(Config{Logger: logr.Discard()})
	key := types.NamespacedName{Namespace: testNS, Name: "x"}
	release := p.Claim(key)
	assert.True(t, p.Claimed(key))
	release()
	assert.False(t, p.Claimed(key))
}

func TestIntakeChecks(t *testing.T) {
	t.Run("rollbackOf is not yet supported", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		p := proposal("p", "alice", "k")
		p.Spec.RollbackOf = &v1alpha1.ActionRef{Name: "ra-1"}
		w.create(t, p)
		out := w.process(t, "p", nil)
		assert.Equal(t, v1alpha1.ReasonNotYetSupported, out.Rejection.Reason)
		assert.Equal(t, v1alpha1.StageIntake, out.Rejection.Stage)
	})
	t.Run("the namespace must be the target's", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		p := proposal("p", "alice", "k")
		p.Spec.Target.Namespace = "elsewhere"
		w.create(t, p)
		assert.Equal(t, v1alpha1.ReasonNamespaceMismatch, w.process(t, "p", nil).Rejection.Reason)
	})
	t.Run("a duplicate key from the same proposer is refused, from another is not", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		w.create(t, proposal("a-first", "alice", "same"))
		w.create(t, proposal("b-second", "alice", "same"))
		w.create(t, proposal("c-other", "bob", "same"))
		assert.Equal(t, v1alpha1.PhaseConverted, w.process(t, "a-first", nil).Phase)
		dup := w.process(t, "b-second", nil)
		assert.Equal(t, v1alpha1.ReasonDuplicate, dup.Rejection.Reason)
		assert.Contains(t, dup.Message, "a-first")
		assert.NotEqual(t, v1alpha1.ReasonDuplicate, reasonOf(w.process(t, "c-other", nil)))
	})
	t.Run("the earlier proposal is never the duplicate, whatever order they are decided in", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		w.create(t, proposal("z-first", "alice", "same"))
		w.create(t, proposal("a-second", "alice", "same"))
		assert.Equal(t, v1alpha1.ReasonDuplicate, w.process(t, "a-second", nil).Rejection.Reason)
		assert.Equal(t, v1alpha1.PhaseConverted, w.process(t, "z-first", nil).Phase)
	})
	t.Run("the rate limit counts a proposer's earlier proposals within the window", func(t *testing.T) {
		w := newWorld(t, Config{RateLimit: 2, RateWindow: 10 * time.Second}, baseObjects()...)
		for i := 1; i <= 4; i++ {
			w.create(t, proposal(fmt.Sprintf("r-%d", i), "alice", fmt.Sprintf("k%d", i)))
		}
		w.create(t, proposal("other", "bob", "k"))
		assert.NotEqual(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "r-1", nil)))
		assert.NotEqual(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "r-2", nil)))
		assert.Equal(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "r-3", nil)))
		assert.Equal(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "r-4", nil)))
		assert.NotEqual(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "other", nil)), "another proposer has its own allowance")
	})
	t.Run("proposals older than the window do not count", func(t *testing.T) {
		w := newWorld(t, Config{RateLimit: 1, RateWindow: time.Second / 2}, baseObjects()...)
		w.create(t, proposal("old", "alice", "k1"))
		w.create(t, proposal("new", "alice", "k2"))
		assert.NotEqual(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "new", nil)), "created a second apart: outside a half-second window")
	})
}

func reasonOf(out *Outcome) string {
	if out == nil || out.Rejection == nil {
		return ""
	}
	return out.Rejection.Reason
}

func TestProcess_StatusWriteFailureRemovesTheActionAndErrors(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p", "alice", "k"))
	failing := interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*dorguv1.RemediationAction); ok {
				return apierrors.NewBadRequest("no")
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	w.p.client, w.p.reader = failing, failing

	_, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"}, nil)

	require.Error(t, err)
	var actions dorguv1.RemediationActionList
	require.NoError(t, w.c.List(context.Background(), &actions))
	assert.Empty(t, actions.Items, "an action with no status is not proposed")
}

func TestFactsFromIncident(t *testing.T) {
	withOOMContribution := incident("i", "CrashLoopBackOff")
	withOOMContribution.Spec.RootCause = &dorguv1.RootCauseInfo{
		Contributing: []dorguv1.ContributingSignal{{Signal: "OOMKilled"}}}
	plainCrashLoop := incident("i", "CrashLoopBackOff")
	plainCrashLoop.Spec.RootCause = &dorguv1.RootCauseInfo{Contributing: []dorguv1.ContributingSignal{{Signal: "Other"}}}

	tests := []struct {
		name       string
		in         *dorguv1.IncidentMemory
		wantAction string
		wantPath   string
	}{
		{"nil", nil, "", ""},
		{"oom", incident("i", "OOMKilled"), gate.ClassResourceAdjustment, gate.ResourcePathMemory},
		{"memory saturation", incident("i", "MemorySaturationHigh"), gate.ClassResourceAdjustment, gate.ResourcePathMemory},
		{"cpu saturation", incident("i", "CPUSaturationCritical"), gate.ClassResourceAdjustment, gate.ResourcePathCPU},
		{"crash loop with oom", withOOMContribution, gate.ClassResourceAdjustment, gate.ResourcePathMemory},
		{"crash loop", plainCrashLoop, "", ""},
		{"crash loop with no diagnosis", incident("i", "CrashLoopBackOff"), "", ""},
		{"image pull", incident("i", "ImagePullBackOff"), "", ""},
		{"a signal a proposer made up", incident("i", "DefinitelyAResourceProblem"), "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := factsFromIncident(tt.in)
			assert.Equal(t, tt.wantAction, f.SuggestedAction)
			assert.Equal(t, tt.wantPath, f.TargetPath)
			assert.False(t, f.AdvisoryAllowed, "an external plan never lifts the completeness check")
		})
	}
}

func TestConditionReason(t *testing.T) {
	assert.Equal(t, "DenyList", conditionReason("deny-list"))
	assert.Equal(t, "ActionClass", conditionReason("action-class"))
	assert.Equal(t, "RateLimited", conditionReason("RateLimited"))
	assert.Equal(t, "Rejected", conditionReason(""))
	assert.Equal(t, "Rejected", conditionReason("--"))
}

func TestActionName(t *testing.T) {
	p := proposal("p", "alice", "k")
	p.UID = "uid-1"
	name := actionName(p)
	assert.True(t, strings.HasPrefix(name, "ra-web-"))
	assert.Equal(t, name, actionName(p), "derived, so a retry finds the same name")
	p.UID = "uid-2"
	assert.NotEqual(t, name, actionName(p), "a recreated proposal does not collide with its predecessor's action")

	p.Spec.Target.Name = strings.Repeat("a", 300)
	assert.LessOrEqual(t, len(actionName(p)), 253)
}

func TestLocks_SerialisePerPersona(t *testing.T) {
	l := NewLocks()
	ref := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "web", Namespace: testNS}
	unlock := l.Lock(ref)

	got := make(chan struct{})
	go func() {
		l.Lock(ref)()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("a second holder got the lock while the first held it")
	case <-time.After(50 * time.Millisecond):
	}
	other := l.Lock(&dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "db", Namespace: testNS})
	other()
	unlock()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the second holder never got the lock")
	}
}

func TestStepsEqual_IgnoresJSONFormatting(t *testing.T) {
	a := []dorguv1.RemediationStep{{Order: 1, ID: "s", Type: dorguv1.StepTypePersonaUpdate,
		Patch: &apiextensionsv1.JSON{Raw: []byte(`{"a":1,"b":2}`)}}}
	b := []dorguv1.RemediationStep{{Order: 1, ID: "s", Type: dorguv1.StepTypePersonaUpdate,
		Patch: &apiextensionsv1.JSON{Raw: []byte("{ \"b\": 2,\n \"a\": 1 }")}}}
	assert.True(t, stepsEqual(a, b), "the API server re-serialises embedded JSON")
	b[0].Patch.Raw = []byte(`{"a":1,"b":3}`)
	assert.False(t, stepsEqual(a, b))
	assert.False(t, stepsEqual(a, nil))
	assert.False(t, jsonEqual(a, make(chan int)), "unmarshalable input is never equal")
}
