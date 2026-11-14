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
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

// countingReader wraps the uncached reader and counts the Lists made through it.
func countingReader(w *world, lists *atomic.Int64) client.Reader {
	return interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			return c.List(ctx, list, opts...)
		},
	})
}

// F-10: deciding a proposal lists every proposal (dedupe, rate limit) and the
// namespace's actions (adopt or redo). Through the uncached reader that is one
// cluster-wide List against the API server per undecided proposal, so a burst of N
// proposals costs O(N squared) bytes. Both Lists go to the cache; the uncached
// reader is for Gets of the one object being decided.
func TestProcess_ListsAreServedFromTheCacheNotTheAPIServer(t *testing.T) {
	var uncachedLists atomic.Int64
	w := newWorld(t, Config{}, baseObjects()...)
	w.p.reader = countingReader(w, &uncachedLists)
	w.p.lister = w.c
	w.create(t, proposal("p", "alice", "k"))

	out := w.process(t, "p", nil)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Zero(t, uncachedLists.Load(), "no List went to the uncached reader")
}

func TestNew_TheListerDefaultsToTheReaderAndIsOverridable(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	lister := fake.NewClientBuilder().WithScheme(scheme(t)).Build()

	assert.True(t, New(Config{Client: reader, Reader: reader, Logger: logr.Discard()}).lister == client.Reader(reader),
		"without a Lister the Lists go through the Reader")
	assert.True(t, New(Config{Client: reader, Reader: reader, Lister: lister, Logger: logr.Discard()}).lister == client.Reader(lister),
		"a Lister is used for the Lists")
}

// A stale cache must never delete a live action: an action that looks unfinished in
// the cache is re-read from the API server before it is removed.
func TestProcess_AStaleCacheDoesNotDeleteAnActionThatHasItsStatus(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p-stale", "alice", "k"))
	prop := w.get(t, "p-stale")
	live := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	live.Spec.Provenance = provenanceFor(prop, false)
	live.Annotations[AnnotationProposalUID] = string(prop.UID)
	live.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
	w.create(t, live)
	live.Status = dorguv1.RemediationActionStatus{Phase: "Pending"}
	require.NoError(t, w.c.Status().Update(context.Background(), live))

	// The cache still shows the action as it was just after create: no phase.
	w.p.lister = interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if ras, ok := list.(*dorguv1.RemediationActionList); ok {
				for i := range ras.Items {
					ras.Items[i].Status.Phase = ""
				}
			}
			return nil
		},
	})

	out := w.process(t, "p-stale", nil)

	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Equal(t, live.Name, out.Action.Name, "the live action was adopted, not deleted and remade")
	assert.Equal(t, []string{live.Name}, actionNames(t, w.c))
}
