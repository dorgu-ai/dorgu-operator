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

// Names tests reuse.
const (
	mallory   = "mallory"
	victim    = "victim"
	otherName = "other"
)

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
			n := clock.Add(1)
			obj.SetCreationTimestamp(metav1.NewTime(time.Unix(1_700_000_000+n, 0)))
			obj.SetUID(types.UID(fmt.Sprintf("uid-%d", n)))
			return cl.Create(ctx, obj, opts...)
		},
	})
	w := &world{c: c, open: &fakeOpener{incident: incident("opened", "ProposerReported")}}
	cfg.Client, cfg.Logger = c, logr.Discard()
	if cfg.OperatorUsername == "" {
		cfg.OperatorUsername = TestOperatorUsername
	}
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
	assert.Empty(t, p.OperatorUsername(), "an unknown username is never defaulted")
	assert.NotNil(t, p.Blocked(), "and the intake is closed")
	assert.Equal(t, DefaultRejectedTTL, p.RejectedTTL())
	assert.NotNil(t, p.Locks())
	assert.WithinDuration(t, time.Now(), p.Now(), time.Second)
	assert.Equal(t, DefaultRateLimit, p.rateLimit)
	assert.Equal(t, DefaultRateWindow, p.rateWindow)
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
	orphan.Spec.Provenance = provenanceFor(prop, false)
	orphan.Annotations[AnnotationProposalUID] = string(prop.UID)
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
		p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "inc", Namespace: otherName}
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

// plannerProposal is a proposal as Dorgu's planner writes it.
func plannerProposal(name string) *v1alpha1.RemediationProposal {
	p := proposal(name, testOperator, name)
	p.Spec.Source = dorguv1.ProposalSourceDorguPlanner
	p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "inc", Namespace: testNS}
	return p
}

// plannerHint is the hint the planner hands over for the stored proposal.
func plannerHint(prop *v1alpha1.RemediationProposal) *Hint {
	draft := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	draft.Name = prop.Name
	draft.Spec.Confidence = "0.77"
	draft.Spec.PlanSource = dorguv1.PlanSourceRuleBased
	return &Hint{
		ProposalUID: prop.UID, Draft: draft, PersonaLocked: true,
		Facts: gate.Facts{Incident: incident("not-stored", "ImagePullBackOff")},
	}
}

func TestProcess_AMatchingHintKeepsThePlannersShape(t *testing.T) {
	w := newWorld(t, Config{OperatorUsername: testOperator}, baseObjects()...)
	w.create(t, plannerProposal("p-hint"))
	prop := w.get(t, "p-hint")
	hint := plannerHint(prop)
	key := types.NamespacedName{Namespace: testNS, Name: "p-hint"}
	release := w.p.Claim(key)
	assert.True(t, w.p.Claimed(key))
	defer release()
	ref := personaRefOf(prop)
	defer w.p.Locks().Lock(&ref)()

	out := w.process(t, "p-hint", hint)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Equal(t, "p-hint", out.Action.Name)
	assert.Equal(t, "0.77", out.Action.Spec.Confidence)
	assert.Equal(t, "not-stored", w.get(t, "p-hint").Status.IncidentRef.Name, "an incident the planner passes in need not be re-read")
}

// F-12: status.gate is the record to trust, so it is the intake's own run.
func TestProcess_StatusGateRecordsTheIntakesOwnRun(t *testing.T) {
	w := newWorld(t, Config{OperatorUsername: testOperator}, baseObjects()...)
	w.create(t, plannerProposal("p-hint"))
	prop := w.get(t, "p-hint")
	hint := plannerHint(prop)
	ref := personaRefOf(prop)
	defer w.p.Locks().Lock(&ref)()

	out := w.process(t, "p-hint", hint)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	require.NotNil(t, out.Decision)
	var stored dorguv1.RemediationAction
	require.NoError(t, w.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p-hint"}, &stored))
	assert.Equal(t, out.Decision.Status().EvaluatedAt.Unix(), stored.Status.Gate.EvaluatedAt.Unix(), "the record is the run that authorised creation")
	assert.Equal(t, out.Decision.Status().Verdicts, stored.Status.Gate.Verdicts)
}

// F-12: the hint is bound to the proposal it was issued for, and to the planner.
func TestHintMatches_IsBoundToTheProposalAndThePlanner(t *testing.T) {
	prop := plannerProposal("p")
	prop.UID = "uid-1"
	good := func() *Hint { h := plannerHint(prop); return h }
	require.True(t, hintMatches(good(), prop, testOperator))

	tests := []struct {
		name   string
		mutate func(*Hint, *v1alpha1.RemediationProposal) string // returns the operator username to test with
	}{
		{"nil hint", func(h *Hint, _ *v1alpha1.RemediationProposal) string { *h = Hint{}; return testOperator }},
		{"another proposal under the same name (UID)", func(h *Hint, _ *v1alpha1.RemediationProposal) string {
			h.ProposalUID = "uid-other"
			return testOperator
		}},
		{"not the operator's username", func(_ *Hint, p *v1alpha1.RemediationProposal) string {
			p.Spec.Proposer.Username = mallory
			return testOperator
		}},
		{"operator username unknown", func(*Hint, *v1alpha1.RemediationProposal) string { return "" }},
		{"not the planner's source", func(_ *Hint, p *v1alpha1.RemediationProposal) string {
			p.Spec.Source = dorguv1.ProposalSourceAgent
			return testOperator
		}},
		{"idempotency key is not the proposal name", func(_ *Hint, p *v1alpha1.RemediationProposal) string {
			p.Spec.IdempotencyKey = otherName
			return testOperator
		}},
		{"draft has another name", func(h *Hint, _ *v1alpha1.RemediationProposal) string {
			h.Draft.Name = otherName
			return testOperator
		}},
		{"different incident", func(_ *Hint, p *v1alpha1.RemediationProposal) string {
			p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "elsewhere", Namespace: testNS}
			return testOperator
		}},
		{"no incident", func(_ *Hint, p *v1alpha1.RemediationProposal) string {
			p.Spec.IncidentRef = nil
			return testOperator
		}},
		{"different target", func(_ *Hint, p *v1alpha1.RemediationProposal) string {
			p.Spec.Target.Name = "db"
			return testOperator
		}},
		{"different steps", func(h *Hint, _ *v1alpha1.RemediationProposal) string {
			h.Draft.Spec.Steps[0].Description = otherName
			return testOperator
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := prop.DeepCopy()
			h := good()
			user := tt.mutate(h, p)
			assert.False(t, hintMatches(h, p, user))
		})
	}
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
		w.create(t, proposal(otherName, "bob", "k"))
		assert.NotEqual(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "r-1", nil)))
		assert.NotEqual(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "r-2", nil)))
		assert.Equal(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "r-3", nil)))
		assert.Equal(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "r-4", nil)))
		assert.NotEqual(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, otherName, nil)), "another proposer has its own allowance")
	})
	t.Run("proposals older than the window do not count", func(t *testing.T) {
		w := newWorld(t, Config{RateLimit: 1, RateWindow: time.Second / 2}, baseObjects()...)
		w.create(t, proposal("old", "alice", "k1"))
		w.create(t, proposal("new", "alice", "k2"))
		assert.NotEqual(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "new", nil)), "created a second apart: outside a half-second window")
	})
}

// F-2: only a matched in-process hint exempts a proposal from the rate limit. A
// proposer that names the operator's username is a claim, not a proof.
func TestIntakeChecks_OnlyAMatchedHintIsExemptFromTheRateLimit(t *testing.T) {
	newLimited := func(t *testing.T) *world {
		w := newWorld(t, Config{OperatorUsername: testOperator, RateLimit: 1}, baseObjects()...)
		w.create(t, plannerProposal("earlier"))
		return w
	}

	t.Run("the planner's matched hint is exempt at limit+1", func(t *testing.T) {
		w := newLimited(t)
		w.create(t, plannerProposal("planner-next"))
		prop := w.get(t, "planner-next")
		ref := personaRefOf(prop)
		defer w.p.Locks().Lock(&ref)()
		out := w.process(t, "planner-next", plannerHint(prop))
		assert.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	})
	t.Run("a proposer naming the operator username, with no hint, is limited", func(t *testing.T) {
		w := newLimited(t)
		w.create(t, plannerProposal("impostor"))
		out := w.process(t, "impostor", nil)
		assert.Equal(t, v1alpha1.ReasonRateLimited, reasonOf(out))
	})
	t.Run("a hint for another UID does not exempt", func(t *testing.T) {
		w := newLimited(t)
		w.create(t, plannerProposal("replaced"))
		prop := w.get(t, "replaced")
		hint := plannerHint(prop)
		hint.ProposalUID = "uid-of-a-deleted-proposal"
		ref := personaRefOf(prop)
		defer w.p.Locks().Lock(&ref)()
		out := w.process(t, "replaced", hint)
		assert.Equal(t, v1alpha1.ReasonRateLimited, reasonOf(out))
	})
	t.Run("a hint against a proposal from someone else does not exempt", func(t *testing.T) {
		w := newLimited(t)
		other := plannerProposal("mallory-prop")
		other.Spec.Proposer.Username = mallory
		w.create(t, other)
		w.create(t, plannerProposal("mallory-prop-2"))
		w.create(t, func() *v1alpha1.RemediationProposal {
			p := plannerProposal("mallory-3")
			p.Spec.Proposer.Username = mallory
			return p
		}())
		w.process(t, "mallory-prop", nil)
		prop := w.get(t, "mallory-3")
		hint := plannerHint(prop)
		ref := personaRefOf(prop)
		defer w.p.Locks().Lock(&ref)()
		out := w.process(t, "mallory-3", hint)
		assert.Equal(t, v1alpha1.ReasonRateLimited, reasonOf(out))
	})
}

func memoryRaise() []dorguv1.RemediationStep {
	return []dorguv1.RemediationStep{{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
		Patch: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`)},
	}}
}

func incidentOn(name string) *dorguv1.IncidentMemory {
	im := incident(name, "OOMKilled")
	im.Spec.PersonaRef.Name = "web"
	return im
}

// F-3: an incident on persona A cannot unlock a resource change on persona B.
func TestProcess_AnIncidentOnAnotherPersonaCannotUnlockAChange(t *testing.T) {
	objs := append(baseObjects(), persona(victim), incidentOn("oom-of-web"))

	t.Run("a named incident for a different persona is refused at Intake", func(t *testing.T) {
		w := newWorld(t, Config{}, objs...)
		p := proposal("mallory-1", mallory, "k")
		p.Spec.Target.Name = victim
		p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "oom-of-web", Namespace: testNS}
		p.Spec.Steps = memoryRaise()
		w.create(t, p)

		out := w.process(t, "mallory-1", nil)

		require.Equal(t, v1alpha1.PhaseRejected, out.Phase, out.Message)
		assert.Equal(t, v1alpha1.StageIntake, out.Rejection.Stage)
		assert.Equal(t, v1alpha1.ReasonIncidentTargetMismatch, out.Rejection.Reason)
		var actions dorguv1.RemediationActionList
		require.NoError(t, w.c.List(context.Background(), &actions))
		assert.Empty(t, actions.Items, "no action for the victim persona")
	})
	t.Run("the incident's own persona is not refused for the target", func(t *testing.T) {
		w := newWorld(t, Config{}, objs...)
		p := proposal("alice-1", "alice", "k")
		p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "oom-of-web", Namespace: testNS}
		p.Spec.Steps = memoryRaise()
		w.create(t, p)
		assert.NotEqual(t, v1alpha1.ReasonIncidentTargetMismatch, reasonOf(w.process(t, "alice-1", nil)))
	})
	t.Run("a persona kind or namespace mismatch counts too", func(t *testing.T) {
		for name, mutate := range map[string]func(*dorguv1.IncidentMemory){
			"kind":      func(im *dorguv1.IncidentMemory) { im.Spec.PersonaRef.Kind = "ClusterPersona" },
			"namespace": func(im *dorguv1.IncidentMemory) { im.Spec.PersonaRef.Namespace = "elsewhere" },
		} {
			im := incidentOn("odd")
			mutate(im)
			w := newWorld(t, Config{}, append(baseObjects(), im)...)
			p := proposal("p", "alice", "k")
			p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "odd", Namespace: testNS}
			w.create(t, p)
			assert.Equal(t, v1alpha1.ReasonIncidentTargetMismatch, reasonOf(w.process(t, "p", nil)), name)
		}
	})
	t.Run("an incident that omits its persona namespace means its own", func(t *testing.T) {
		im := incidentOn("bare")
		im.Spec.PersonaRef.Namespace = ""
		w := newWorld(t, Config{}, append(baseObjects(), im)...)
		p := proposal("p", "alice", "k")
		p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "bare", Namespace: testNS}
		w.create(t, p)
		assert.NotEqual(t, v1alpha1.ReasonIncidentTargetMismatch, reasonOf(w.process(t, "p", nil)))
	})
	t.Run("an incident the hint carries is held to the same rule", func(t *testing.T) {
		w := newWorld(t, Config{OperatorUsername: testOperator}, append(baseObjects(), persona(victim))...)
		prop := plannerProposal("planner-p")
		prop.Spec.Target.Name = victim
		w.create(t, prop)
		stored := w.get(t, "planner-p")
		hint := plannerHint(stored)
		hint.Draft.Spec.PersonaRef.Name = victim
		hint.Facts.Incident = incidentOn("oom-of-web")
		ref := personaRefOf(stored)
		defer w.p.Locks().Lock(&ref)()

		out := w.process(t, "planner-p", hint)

		assert.Equal(t, v1alpha1.ReasonIncidentTargetMismatch, reasonOf(out))
	})
}

// F-3: the planner never proposes against a resolved incident, so an outside
// proposal may not cite one to unlock a change either.
func TestProcess_AResolvedIncidentCannotBeCited(t *testing.T) {
	resolved := incidentOn("old-oom")
	resolved.Status.Phase = IncidentPhaseResolved
	w := newWorld(t, Config{}, append(baseObjects(), resolved)...)
	p := proposal("p", "alice", "k")
	p.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "old-oom", Namespace: testNS}
	w.create(t, p)

	out := w.process(t, "p", nil)

	require.Equal(t, v1alpha1.PhaseRejected, out.Phase, out.Message)
	assert.Equal(t, v1alpha1.StageIntake, out.Rejection.Stage)
	assert.Equal(t, v1alpha1.ReasonIncidentResolved, out.Rejection.Reason)
}

// F-6: a Rejected proposal releases its idempotency key, so the retry an
// idempotency key exists to make safe is not refused as its own duplicate. Only a
// proposal that is Received (undecided) or Converted holds the key.
func TestIntakeChecks_ARejectedProposalReleasesItsIdempotencyKey(t *testing.T) {
	t.Run("a retry after a gate refusal", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		refused := proposal("a-first", "alice", "retry-key")
		refused.Spec.Steps = memoryRaise() // ImagePullBackOff is not a resource problem
		w.create(t, refused)
		first := w.process(t, "a-first", nil)
		require.Equal(t, v1alpha1.PhaseRejected, first.Phase)
		require.Equal(t, gate.StageValidate, first.Rejection.Stage)

		w.create(t, proposal("b-retry", "alice", "retry-key"))
		retry := w.process(t, "b-retry", nil)

		assert.Equal(t, v1alpha1.PhaseConverted, retry.Phase, "the corrected retry is decided on its merits: %s", retry.Message)
	})
	t.Run("a retry after RateLimited", func(t *testing.T) {
		w := newWorld(t, Config{RateLimit: 1, RateWindow: 10 * time.Second}, baseObjects()...)
		w.create(t, proposal("old", "alice", otherName))
		w.create(t, proposal("limited", "alice", "retry-key"))
		w.create(t, proposal("retry", "alice", "retry-key"))
		require.Equal(t, v1alpha1.ReasonRateLimited, reasonOf(w.process(t, "limited", nil)))

		out := w.process(t, "retry", nil)

		// Still inside the window, so still limited, but the key was not used up: the
		// answer is the limit, not "duplicate of a proposal that never happened".
		assert.Equal(t, v1alpha1.ReasonRateLimited, reasonOf(out))
	})
	t.Run("a retry after IncidentNotFound", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		gone := proposal("a-first", "alice", "retry-key")
		gone.Spec.IncidentRef = &dorguv1.IncidentReference{Name: "not-yet", Namespace: testNS}
		w.create(t, gone)
		require.Equal(t, v1alpha1.ReasonIncidentNotFound, reasonOf(w.process(t, "a-first", nil)))

		w.create(t, proposal("b-retry", "alice", "retry-key"))
		assert.Equal(t, v1alpha1.PhaseConverted, w.process(t, "b-retry", nil).Phase)
	})
	t.Run("a Converted proposal still holds its key", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		w.create(t, proposal("a-first", "alice", "same"))
		require.Equal(t, v1alpha1.PhaseConverted, w.process(t, "a-first", nil).Phase)
		w.create(t, proposal("b-second", "alice", "same"))
		assert.Equal(t, v1alpha1.ReasonDuplicate, reasonOf(w.process(t, "b-second", nil)))
	})
	t.Run("a Received proposal still holds its key", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		w.create(t, proposal("a-first", "alice", "same"))
		w.create(t, proposal("b-second", "alice", "same"))
		first := w.get(t, "a-first")
		first.Status.Phase = v1alpha1.PhaseReceived
		require.NoError(t, w.c.Status().Update(context.Background(), first))
		assert.Equal(t, v1alpha1.ReasonDuplicate, reasonOf(w.process(t, "b-second", nil)))
	})
}

// F-4: a proposal's text is untrusted. It is stripped of control characters before
// it is stored anywhere the operator writes it.
func TestProcess_UntrustedTextIsStrippedBeforeItIsStored(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	hostile := "\x1b[2J\x1b[31mAPPROVE ME\x1b[0m\nline two\ttabbed " + string(rune(0x202E)) + "gnp.exe"
	p := proposal("hostile", mallory, "k")
	p.Spec.Rationale = hostile
	p.Spec.Steps = []dorguv1.RemediationStep{{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual,
		Description: "look\x1b[2Jat it" + string(rune(0x2028)) + "now", Rationale: "because\x07 bells",
	}}
	p.Spec.Agent = &dorguv1.AgentInfo{Name: "evil\x1b[1m", Version: "1\x00", RunID: "r\n1"}
	w.create(t, p)

	out := w.process(t, "hostile", nil)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	ra := out.Action
	assert.Equal(t, "[2J[31mAPPROVE ME[0m\nline two\ttabbed gnp.exe", ra.Spec.Explanation, "controls gone, newline and tab kept")
	assert.Equal(t, "look[2Jat itnow", ra.Spec.Steps[0].Description)
	assert.Equal(t, "because bells", ra.Spec.Steps[0].Rationale)
	assert.Equal(t, "evil[1m", ra.Spec.Provenance.Agent.Name)
	assert.Equal(t, "1", ra.Spec.Provenance.Agent.Version)
	assert.Equal(t, "r1", ra.Spec.Provenance.Agent.RunID)
}

// F-4 and F-7: only a proposal decided with a matched in-process planner hint is
// badged as the planner's. A proposal that says it is the planner, and is not, is
// an agent's.
func TestProcess_OnlyAMatchedPlannerRunIsBadgedAsThePlanner(t *testing.T) {
	t.Run("a claim of source dorgu-planner with no hint is recorded as agent", func(t *testing.T) {
		w := newWorld(t, Config{OperatorUsername: testOperator}, baseObjects()...)
		w.create(t, plannerProposal("claims-planner"))
		out := w.process(t, "claims-planner", nil)
		require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
		assert.Equal(t, dorguv1.ProposalSourceAgent, out.Action.Spec.Provenance.Source)
	})
	t.Run("the matched planner run keeps source dorgu-planner", func(t *testing.T) {
		w := newWorld(t, Config{OperatorUsername: testOperator}, baseObjects()...)
		w.create(t, plannerProposal("real-planner"))
		prop := w.get(t, "real-planner")
		ref := personaRefOf(prop)
		defer w.p.Locks().Lock(&ref)()
		out := w.process(t, "real-planner", plannerHint(prop))
		require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
		assert.Equal(t, dorguv1.ProposalSourceDorguPlanner, out.Action.Spec.Provenance.Source)
	})
	t.Run("the reserved source rollback cannot be claimed either", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		p := proposal("claims-rollback", mallory, "k")
		p.Spec.Source = dorguv1.ProposalSourceRollback
		w.create(t, p)
		out := w.process(t, "claims-rollback", nil)
		require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
		assert.Equal(t, dorguv1.ProposalSourceAgent, out.Action.Spec.Provenance.Source)
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

// F-11: conversion is idempotent end to end. The action is created and its status
// written, then recording the proposal as Converted fails: the retry finds the action
// it already made and finishes, with no second action.
func TestProcess_AFailedConvertedWriteIsFinishedByTheRetry(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p", "alice", "k"))
	failConverted := true
	failing := interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if prop, ok := obj.(*v1alpha1.RemediationProposal); ok && failConverted && prop.Status.Phase == v1alpha1.PhaseConverted {
				return apierrors.NewBadRequest("status write refused")
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	w.p.client, w.p.reader = failing, failing

	_, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"}, nil)
	require.Error(t, err)
	assert.NotEqual(t, v1alpha1.PhaseConverted, w.get(t, "p").Status.Phase)
	var actions dorguv1.RemediationActionList
	require.NoError(t, w.c.List(context.Background(), &actions))
	require.Len(t, actions.Items, 1, "the action exists")
	first := actions.Items[0].Name
	found, err := w.p.FindAction(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"})
	require.NoError(t, err)
	require.NotNil(t, found, "the planner can see it exists")
	assert.Equal(t, first, found.Name)

	failConverted = false
	out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"}, nil)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.PhaseConverted, out.Phase)
	assert.Equal(t, first, out.Action.Name, "the retry adopted the action, it did not make another")
	require.NoError(t, w.c.List(context.Background(), &actions))
	assert.Len(t, actions.Items, 1)
	assert.Equal(t, first, w.get(t, "p").Status.RemediationActionRef.Name)
}

// F-11: the action is matched by the proposal's UID, so a proposal recreated under
// the same name does not adopt the action its predecessor left.
func TestFindAction_IsBoundToTheProposalsUID(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p", "alice", "k"))
	prop := w.get(t, "p")
	key := types.NamespacedName{Namespace: testNS, Name: "p"}

	stale := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	stale.Name = "ra-of-the-predecessor"
	stale.Spec.Provenance = provenanceFor(prop, false)
	stale.Annotations[AnnotationProposalUID] = "uid-of-a-deleted-proposal"
	w.create(t, stale)
	require.NoError(t, w.c.Get(context.Background(), client.ObjectKeyFromObject(stale), stale))
	stale.Status.Phase = "Pending"
	require.NoError(t, w.c.Status().Update(context.Background(), stale))

	found, err := w.p.FindAction(context.Background(), key)
	require.NoError(t, err)
	assert.Nil(t, found, "the predecessor's action is not this proposal's")

	mine := stale.DeepCopy()
	mine.ResourceVersion, mine.Name = "", "ra-of-this-proposal"
	mine.Annotations = map[string]string{AnnotationProposalUID: string(prop.UID)}
	mine.Status = dorguv1.RemediationActionStatus{}
	w.create(t, mine)
	require.NoError(t, w.c.Get(context.Background(), client.ObjectKeyFromObject(mine), mine))
	mine.Status.Phase = "Pending"
	require.NoError(t, w.c.Status().Update(context.Background(), mine))

	found, err = w.p.FindAction(context.Background(), key)
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, "ra-of-this-proposal", found.Name)
}

func TestFindAction_NoProposalOrNoActionIsNil(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	found, err := w.p.FindAction(context.Background(), types.NamespacedName{Namespace: testNS, Name: "ghost"})
	require.NoError(t, err)
	assert.Nil(t, found)
	w.create(t, proposal("p", "alice", "k"))
	found, err = w.p.FindAction(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"})
	require.NoError(t, err)
	assert.Nil(t, found, "decided nothing yet")
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
		{"cpu throttling", incident("i", "CPUThrottling"), gate.ClassResourceAdjustment, gate.ResourcePathCPU},
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
