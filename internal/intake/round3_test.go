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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

// Fix round 3 of C1 (re-review N-1, N-3, N-5, N-7, N-8).

func withClient(w *world, funcs interceptor.Funcs) {
	w.p.client = interceptor.NewClient(w.c.(client.WithWatch), funcs)
}

// N-1: an action the API server refuses as Invalid can never be created by
// retrying. The proposal ends Rejected{Intake, ActionInvalid}, with the server's
// message cut short, and is not retried.
func TestProcess_AnInvalidActionIsATerminalRejectionNotARetry(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	var creates int
	withClient(w, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*dorguv1.RemediationAction); ok {
				creates++
				return apierrors.NewInvalid(schema.GroupKind{Group: "dorgu.io", Kind: "RemediationAction"}, obj.GetName(),
					field.ErrorList{field.TooMany(field.NewPath("spec", "steps").Index(0).Child("safety"), 99, 16),
						field.Invalid(field.NewPath("spec", "explanation"), "x", strings.Repeat("long ", 2000))})
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	w.create(t, proposal("p-invalid", "alice", "k"))

	out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p-invalid"}, nil)

	require.NoError(t, err, "an Invalid create is final: returning the error would retry it forever")
	require.NotNil(t, out)
	assert.Equal(t, v1alpha1.PhaseRejected, out.Phase)
	assert.Equal(t, &v1alpha1.ProposalRejection{Stage: v1alpha1.StageIntake, Reason: v1alpha1.ReasonActionInvalid}, out.Rejection)
	assert.Contains(t, out.Message, "safety", "the API server's reason is kept")
	assert.LessOrEqual(t, utf8.RuneCountInString(out.Message), maxAPIMessage+200, "and cut short")

	got := w.get(t, "p-invalid")
	assert.Equal(t, v1alpha1.PhaseRejected, got.Status.Phase)
	assert.Equal(t, v1alpha1.ReasonActionInvalid, got.Status.Rejection.Reason)

	again := w.process(t, "p-invalid", nil)
	assert.True(t, again.Replayed, "decided once")
	assert.Equal(t, 1, creates, "never retried")
}

func TestProcess_AnInvalidCreateMessageIsCleanedOfControlCharacters(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	withClient(w, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*dorguv1.RemediationAction); ok {
				return apierrors.NewInvalid(schema.GroupKind{Kind: "RemediationAction"}, obj.GetName(),
					field.ErrorList{field.Invalid(field.NewPath("spec"), "v", "bad \x1b[2J value \u202e")})
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	w.create(t, proposal("p-ctl", "alice", "k"))

	out := w.process(t, "p-ctl", nil)

	assert.NotContains(t, out.Message, "\x1b")
	assert.NotContains(t, out.Message, "\u202e")
}

// N-3: the intake only reads what the cache lists, so it asks for no deep copy.
func TestProcess_ListsDoNotDeepCopyTheCache(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	var mu sync.Mutex
	listed := map[string]bool{}
	w.p.lister = interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := &client.ListOptions{}
			lo.ApplyOptions(opts)
			mu.Lock()
			listed[strings.TrimPrefix(strings.TrimPrefix(typeName(list), "*"), "v1alpha1.")] =
				lo.UnsafeDisableDeepCopy != nil && *lo.UnsafeDisableDeepCopy
			mu.Unlock()
			return cl.List(ctx, list, opts...)
		},
	})
	w.create(t, proposal("p-nocopy", "alice", "k"))

	out := w.process(t, "p-nocopy", nil)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	require.Len(t, listed, 2, "the proposal list and the action list")
	for kind, noCopy := range listed {
		assert.True(t, noCopy, "the %s List asks for UnsafeDisableDeepCopy", kind)
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *v1alpha1.RemediationProposalList:
		return "RemediationProposalList"
	case *dorguv1.RemediationActionList:
		return "RemediationActionList"
	}
	return "other"
}

// N-3, the audit's other half: objects that come back from an uncopied List are the
// cache's own, so nothing the intake hands on may alias them.
func TestActionFor_ReturnsACopyOfWhatTheCacheListed(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-alias", "alice", "k"))
	require.Equal(t, v1alpha1.PhaseConverted, w.process(t, "p-alias", nil).Phase)
	var live dorguv1.RemediationActionList
	require.NoError(t, w.c.List(context.Background(), &live))
	require.Len(t, live.Items, 1)
	store := live.Items // what the cache holds: handed out by shallow copy, maps shared

	cache := interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if ras, ok := list.(*dorguv1.RemediationActionList); ok {
				ras.Items = append([]dorguv1.RemediationAction(nil), store...)
				return nil
			}
			return cl.List(ctx, list, opts...)
		},
	})
	prop := w.get(t, "p-alias")

	got, err := w.p.actionFor(context.Background(), prop, false, cache)

	require.NoError(t, err)
	require.NotNil(t, got)
	before := store[0].DeepCopy()
	got.Annotations["scribbled"] = "yes"
	got.Spec.Steps = nil
	got.Spec.Provenance.Proposal = "scribbled"
	assert.Equal(t, before.Annotations, store[0].Annotations, "the cached object was not written through")
	assert.Equal(t, before.Spec, store[0].Spec)
}

// N-7: after a failed decision the planner asks whether an action exists. The cache
// may not show one made a moment ago, so FindAction asks the API server.
func TestFindAction_ReadsTheAPIServerNotTheCache(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-find", "alice", "k"))
	require.Equal(t, v1alpha1.PhaseConverted, w.process(t, "p-find", nil).Phase)

	w.p.lister = fake.NewClientBuilder().WithScheme(scheme(t)).Build() // a cache that has seen nothing yet

	got, err := w.p.FindAction(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p-find"})

	require.NoError(t, err)
	require.NotNil(t, got, "the action exists, whatever the cache has caught up with")
	assert.Equal(t, "p-find", got.Spec.Provenance.Proposal)
}

// N-8: a delete names the object it means.
func TestProcess_DeletesCarryAUIDPrecondition(t *testing.T) {
	cases := map[string]struct {
		prepare func(w *world)
	}{
		"the status write fails": {prepare: func(w *world) {
			withClient(w, interceptor.Funcs{
				SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
					if _, ok := obj.(*dorguv1.RemediationAction); ok {
						return errors.New("denied")
					}
					return nil
				},
				Delete: recordDeletes(w),
			})
		}},
		"the API server dropped the provenance": {prepare: func(w *world) {
			withClient(w, interceptor.Funcs{
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if ra, ok := obj.(*dorguv1.RemediationAction); ok {
						ra.Spec.Provenance = nil
					}
					return cl.Create(ctx, obj, opts...)
				},
				Delete: recordDeletes(w),
			})
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, Config{}, baseObjects()...)
			deletes = nil
			tc.prepare(w)
			w.create(t, proposal("p-del", "alice", "k"))

			_, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p-del"}, nil)

			require.Error(t, err)
			require.Len(t, deletes, 1, "the unfinished action was deleted")
			assert.NotEmpty(t, deletes[0], "and the delete names the UID it means")
			assert.Empty(t, actionNames(t, w.c))
		})
	}
}

var deletes []string

// recordDeletes notes the UID precondition of every RemediationAction delete.
func recordDeletes(_ *world) func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
	return func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		do := &client.DeleteOptions{}
		do.ApplyOptions(opts)
		uid := ""
		if do.Preconditions != nil && do.Preconditions.UID != nil {
			uid = string(*do.Preconditions.UID)
		}
		if _, ok := obj.(*dorguv1.RemediationAction); ok {
			deletes = append(deletes, uid)
		}
		return cl.Delete(ctx, obj, opts...)
	}
}

// N-8: an action the intake made and could neither finish nor delete (the delete
// failed) is found again by the annotation that names the proposal's UID, and
// removed, instead of leaving the proposal to fail with AlreadyExists forever.
func TestProcess_AnOrphanThatCouldNotBeDeletedIsRemovedByTheRetry(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	var failDeletes, stripProvenance = true, true
	withClient(w, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if ra, ok := obj.(*dorguv1.RemediationAction); ok && stripProvenance {
				ra.Spec.Provenance = nil
			}
			return cl.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*dorguv1.RemediationAction); ok && failDeletes {
				return errors.New("the API server is busy")
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})
	w.create(t, proposal("p-orphan", "alice", "k"))
	key := types.NamespacedName{Namespace: testNS, Name: "p-orphan"}

	_, err := w.p.Process(context.Background(), key, nil)
	var notReady *NotReadyError
	require.ErrorAs(t, err, &notReady, "the intake closes on a CRD that drops provenance")
	require.Len(t, actionNames(t, w.c), 1, "the orphan is still there: phase empty, no provenance")

	// The CRD is fixed, the API server answers, the checks run again.
	failDeletes, stripProvenance = false, false
	w.p.preflight.Run(context.Background())

	out, err := w.p.Process(context.Background(), key, nil)

	require.NoError(t, err)
	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	names := actionNames(t, w.c)
	require.Len(t, names, 1, "the orphan was replaced, not adopted without provenance and not left to wedge the proposal")
	var ra dorguv1.RemediationAction
	require.NoError(t, w.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: names[0]}, &ra))
	require.NotNil(t, ra.Spec.Provenance)
	assert.Equal(t, "Pending", ra.Status.Phase)
}

// An unfinished action the annotation names is removed, but never one that has a
// phase: that is somebody's live action.
func TestProcess_AnActionWithAPhaseIsNeverRemovedWhateverItsProvenance(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-live", "alice", "k"))
	prop := w.get(t, "p-live")
	live := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	live.Annotations[AnnotationProposalUID] = string(prop.UID)
	live.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
	w.create(t, live)
	live.Status = dorguv1.RemediationActionStatus{Phase: "Approved"}
	require.NoError(t, w.c.Status().Update(context.Background(), live))

	got, err := w.p.actionFor(context.Background(), prop, true, w.p.lister)

	// It carries this proposal's UID, has a phase and has no provenance: neither
	// removable nor adoptable, so it is reported as an orphan (R-3), and left alone.
	var orphan *orphanedActionError
	require.ErrorAs(t, err, &orphan)
	assert.Nil(t, got)
	assert.Equal(t, []string{live.Name}, actionNames(t, w.c), "an Approved action was not touched")
}
