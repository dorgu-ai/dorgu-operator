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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

// C2: C1 re-check 3, R-2 to R-6.

// R-2: an identity is never rewritten. A username that safetext would change (a control,
// format or invisible character) or that is over the CRD's bound is Rejected at Intake,
// where C1 cleaned it. The admission policy denies the same names, so provenance can only
// ever hold a name that was verified and left untouched: the approver (compared raw) and
// the recorded proposer can never differ by an invisible character.
func TestProcess_AUsernameThatNeedsCleaningIsRejectedNotRewritten(t *testing.T) {
	cases := map[string]string{
		"terminal escape":      "alice\x1b[2J",
		"bidi override":        "alice\u202e",
		"zero width space":     "alice\u200b",
		"tag character":        "alice\U000E0041",
		"variation selector":   "alice\uFE0F",
		"line separator":       "alice\u2028bob",
		"soft hyphen":          "ali\u00adce",
		"newline":              "alice\nbob",
		"over the CRD bound":   strings.Repeat("é", maxUsernameLength+1),
		"only invisible":       "\u200b\u200b",
		"empty":                "",
		"word joiner":          "alice\u2060",
		"c1 control character": "alice\u0085",
	}
	for name, username := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, Config{}, baseObjects()...)
			w.create(t, proposal("p-name", username, "k"))

			out := w.process(t, "p-name", nil)

			require.Equal(t, v1alpha1.PhaseRejected, out.Phase, out.Message)
			assert.Equal(t, &v1alpha1.ProposalRejection{Stage: v1alpha1.StageIntake, Reason: v1alpha1.ReasonProposerInvalid}, out.Rejection)
			assert.Empty(t, actionNames(t, w.c), "no action is made for an identity nobody can verify")
			assert.NotContains(t, out.Message, "\x1b", "the message never carries the raw name")
			assert.NotContains(t, out.Message, "\u202e")
			assert.LessOrEqual(t, len([]rune(out.Message)), maxConditionMessage)
			cond := w.get(t, "p-name").Status.Conditions
			require.NotEmpty(t, cond)
			assert.Equal(t, "ProposerInvalid", cond[0].Reason)
		})
	}
}

// What is left untouched is recorded as it came, byte for byte, including a legitimate
// non-ASCII name and a ServiceAccount username.
func TestProcess_AVerifiedUsernameIsRecordedByteForByte(t *testing.T) {
	for _, username := range []string{
		"alice",
		"ålice.østergård@example.com",
		"system:serviceaccount:agents:planner-bot",
		"arn:aws:sts::123456789012:assumed-role/Approvers/alice",
		strings.Repeat("é", maxUsernameLength),
	} {
		t.Run(username[:min(len(username), 24)], func(t *testing.T) {
			w := newWorld(t, Config{}, baseObjects()...)
			w.create(t, proposal("p-ok", username, "k"))

			out := w.process(t, "p-ok", nil)

			require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
			assert.Equal(t, username, out.Action.Spec.Provenance.Proposer.Username)
		})
	}
}

// Dedupe and the rate limit compare the usernames as they are: a squatter whose name
// differs by an invisible character is not "alice", and is rejected, not folded in.
func TestProcess_DedupeAndRateLimitCompareUsernamesRaw(t *testing.T) {
	w := newWorld(t, Config{RateLimit: 1}, baseObjects()...)
	w.create(t, proposal("p-a", "mallory", "same"))
	w.create(t, proposal("p-b", "mallory2", "same"))
	require.Equal(t, v1alpha1.PhaseConverted, w.process(t, "p-a", nil).Phase)

	// Another proposer's key is its own and so is its allowance: whatever the gate
	// says about the persona, the intake neither calls this a duplicate nor rate limits it.
	other := w.process(t, "p-b", nil)
	if other.Rejection != nil {
		assert.NotEqual(t, v1alpha1.ReasonDuplicate, other.Rejection.Reason)
		assert.NotEqual(t, v1alpha1.ReasonRateLimited, other.Rejection.Reason)
	}
}

func TestUsernameProblem(t *testing.T) {
	assert.Empty(t, usernameProblem("alice"))
	assert.Empty(t, usernameProblem("system:serviceaccount:ns:sa"))
	assert.NotEmpty(t, usernameProblem(""))
	assert.NotEmpty(t, usernameProblem("alice\xff"), "a byte that is not UTF-8 cannot be recorded as it is")
	assert.NotEmpty(t, usernameProblem("a\u200bb"))
	assert.NotEmpty(t, usernameProblem(strings.Repeat("x", maxUsernameLength+1)))
	assert.Empty(t, usernameProblem(strings.Repeat("x", maxUsernameLength)))
}

// R-3: an action that carries this proposal's UID, has a phase and has no provenance
// (an orphan that somebody wrote a phase on) can neither be removed nor adopted. The
// proposal ends Rejected{Intake, ActionInvalid} naming it, instead of retrying with
// "already exists" for ever.
func TestProcess_AnOrphanThatGainedAPhaseIsATerminalRejection(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-wedge", "alice", "k"))
	prop := w.get(t, "p-wedge")
	orphan := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	orphan.Annotations[AnnotationProposalUID] = string(prop.UID)
	orphan.Spec.Provenance = nil
	w.create(t, orphan)
	orphan.Status = dorguv1.RemediationActionStatus{Phase: "Approved"}
	require.NoError(t, w.c.Status().Update(context.Background(), orphan))

	for attempt := range 3 {
		out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p-wedge"}, nil)
		require.NoError(t, err, "attempt %d: a decision, not a retry", attempt)
		require.Equal(t, v1alpha1.PhaseRejected, out.Phase, out.Message)
		assert.Equal(t, v1alpha1.ReasonActionInvalid, out.Rejection.Reason)
		assert.Contains(t, out.Message, orphan.Name, "the message names the action a person has to deal with")
	}
	assert.Equal(t, []string{orphan.Name}, actionNames(t, w.c), "the action is left alone: it has a phase")
}

// R-4: removing an unfinished action names the object and its version. A status writer
// that sets a phase between the read and the delete makes the delete fail as a conflict
// (an ordinary retry); it can no longer take a live action with it.
func TestProcess_TheOrphanDeleteCarriesTheResourceVersionItRead(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-rv", "alice", "k"))
	prop := w.get(t, "p-rv")
	orphan := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	orphan.Annotations[AnnotationProposalUID] = string(prop.UID)
	w.create(t, orphan) // no status: unfinished

	var uids, versions []string
	withClient(w, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*dorguv1.RemediationAction); ok {
				do := &client.DeleteOptions{}
				do.ApplyOptions(opts)
				require.NotNil(t, do.Preconditions)
				require.NotNil(t, do.Preconditions.UID)
				require.NotNil(t, do.Preconditions.ResourceVersion, "the delete must carry the resourceVersion of the fresh read")
				uids = append(uids, string(*do.Preconditions.UID))
				versions = append(versions, *do.Preconditions.ResourceVersion)
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})
	var fresh dorguv1.RemediationAction
	require.NoError(t, w.c.Get(context.Background(), client.ObjectKeyFromObject(orphan), &fresh))

	_, err := w.p.actionFor(context.Background(), prop, true, w.p.lister)

	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, fresh.ResourceVersion, versions[0])
	assert.Equal(t, string(fresh.UID), uids[0])
}

func TestProcess_AConflictOnTheOrphanDeleteIsARetryNotARejection(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-conf", "alice", "k"))
	prop := w.get(t, "p-conf")
	orphan := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	orphan.Annotations[AnnotationProposalUID] = string(prop.UID)
	w.create(t, orphan)
	withClient(w, interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "dorgu.io", Resource: "remediationactions"}, obj.GetName(),
				errors.New("the object has been modified"))
		},
	})

	_, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p-conf"}, nil)

	require.Error(t, err)
	assert.Equal(t, v1alpha1.PhaseReceived, w.get(t, "p-conf").Status.Phase, "not decided: it is retried")
}

// R-5: the API server refuses the status of the action just created (Invalid) and the
// delete of that action fails too. The proposal must not be Rejected over a live
// orphan (phase empty, which the gate counts as an active remediation and which would
// block the persona until someone deleted it by hand): the removal is tried once more
// first, and only a clean state is recorded as a rejection.
func TestProcess_ATerminalActionInvalidNeverLeavesAnOrphanBehind(t *testing.T) {
	invalidStatus := func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if _, ok := obj.(*dorguv1.RemediationAction); ok {
			return apierrors.NewInvalid(schema.GroupKind{Group: "dorgu.io", Kind: "RemediationAction"}, obj.GetName(),
				field.ErrorList{field.TooMany(field.NewPath("status", "gate", "verdicts"), 99, 64)})
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}

	t.Run("the second removal succeeds", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		var deleteCalls int
		withClient(w, interceptor.Funcs{
			SubResourceUpdate: invalidStatus,
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteCalls++
				if deleteCalls == 1 {
					return errors.New("the API server is busy")
				}
				return cl.Delete(ctx, obj, opts...)
			},
		})
		w.create(t, proposal("p-orph", "alice", "k"))

		out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p-orph"}, nil)

		require.NoError(t, err)
		require.Equal(t, v1alpha1.PhaseRejected, out.Phase, out.Message)
		assert.Equal(t, v1alpha1.ReasonActionInvalid, out.Rejection.Reason)
		assert.Empty(t, actionNames(t, w.c), "nothing is left behind to block the persona")
	})

	t.Run("it still cannot be removed", func(t *testing.T) {
		w := newWorld(t, Config{}, baseObjects()...)
		withClient(w, interceptor.Funcs{
			SubResourceUpdate: invalidStatus,
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return errors.New("the API server is busy")
			},
		})
		w.create(t, proposal("p-stuck", "alice", "k"))

		out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p-stuck"}, nil)

		require.Error(t, err, "the proposal retries instead of being rejected over a live orphan")
		assert.Nil(t, out)
		assert.Equal(t, v1alpha1.PhaseReceived, w.get(t, "p-stuck").Status.Phase)
		assert.Len(t, actionNames(t, w.c), 1)
	})
}

// R-6: the periodic run is bounded. A check that hangs (an API server that accepts the
// connection and goes quiet) used to stop every later run, because Start sat in it.
func TestPreflight_AHungCheckDoesNotStopLaterRuns(t *testing.T) {
	var calls int
	var mu syncMutex
	hang := Check{Name: "hang", Reason: "Hung", Run: func(ctx context.Context) error {
		mu.add(&calls)
		<-ctx.Done()
		return ctx.Err()
	}}
	p := NewPreflight(logr.Discard(), hang)
	p.SetInterval(20 * time.Millisecond)
	p.SetTimeout(30 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	require.NoError(t, p.Start(ctx))

	assert.GreaterOrEqual(t, mu.get(&calls), 4, "later ticks ran although every run hung until its timeout")
	b := p.Blocked()
	require.NotNil(t, b, "a check that never answers closes the intake")
	assert.Equal(t, "Hung", b.Reason)
}

func TestPreflight_TheDefaultRunTimeoutIsBounded(t *testing.T) {
	p := NewPreflight(logr.Discard())
	assert.Equal(t, DefaultPreflightTimeout, p.timeout)
	assert.Positive(t, DefaultPreflightTimeout)
	p.SetTimeout(0)
	assert.Equal(t, DefaultPreflightTimeout, p.timeout, "a zero or negative timeout is ignored")
}

// syncMutex counts calls from another goroutine.
type syncMutex struct{ mu sync.Mutex }

func (s *syncMutex) add(n *int) { s.mu.Lock(); *n++; s.mu.Unlock() }
func (s *syncMutex) get(n *int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *n
}

// R-3, the other branch (C2 review F-7): the lister (the cache) shows the orphan with no
// phase, and by the time the intake re-reads it from the API server somebody has given it a
// phase. It still has no provenance, so it can be neither removed nor adopted: a terminal
// rejection, not a "continue" that tries to create the action again and fails with "already
// exists" for ever. The branch that returns the orphan from the re-read is the only thing
// standing between the two.
func TestProcess_AnOrphanThatGainedAPhaseAfterTheListIsATerminalRejection(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-race", "alice", "k"))
	prop := w.get(t, "p-race")
	orphan := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	orphan.Annotations[AnnotationProposalUID] = string(prop.UID)
	orphan.Spec.Provenance = nil
	w.create(t, orphan) // the lister (the world's own client) lists it with no phase

	var rereads int
	w.p.reader = interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if ra, ok := obj.(*dorguv1.RemediationAction); ok {
				rereads++
				ra.Status.Phase = "Approved" // written between the list and this read
			}
			return nil
		},
	})
	w.p.lister = w.c

	for attempt := range 3 {
		out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p-race"}, nil)
		require.NoError(t, err, "attempt %d: a decision, not a retry", attempt)
		require.Equal(t, v1alpha1.PhaseRejected, out.Phase, out.Message)
		require.NotNil(t, out.Rejection)
		assert.Equal(t, v1alpha1.ReasonActionInvalid, out.Rejection.Reason)
		assert.Equal(t, v1alpha1.StageIntake, out.Rejection.Stage)
		assert.Contains(t, out.Message, orphan.Name)
	}
	assert.Positive(t, rereads, "the decision came from the re-read, which is what this test is about")
	assert.Equal(t, []string{orphan.Name}, actionNames(t, w.c), "nothing was removed: it has a phase now")
}
