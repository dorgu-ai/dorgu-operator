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
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// R-1 of the C3 re-check 2: a gate built without WithReader fell back to the cached
// client for the reads that exist to avoid the cache (sibling actions, the live
// workload at approval, the served spec hash). Production always passes the API
// reader, so it was a footgun for the next caller, not a defect today; the gate now
// never falls back. With no reader those reads fail closed and never touch c.
func TestGateWithoutAReader_NeverReadsThroughTheCachedClient(t *testing.T) {
	var reads atomic.Int32
	counting := interceptor.NewClient(fakeClient(t, persona("256Mi"), deployment("256Mi")), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			reads.Add(1)
			return c.List(ctx, list, opts...)
		},
	})
	g := New(counting, logr.Discard())

	_, err := g.ServedSpecHash(context.Background(), &dorguv1.RemediationAction{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoLiveReader)
	assert.Zero(t, reads.Load(), "the served spec is never read through the cached client")
}

func TestGateWithAReader_ReadsThroughIt(t *testing.T) {
	c := fakeClient(t, persona("256Mi"), deployment("256Mi"))
	var reads atomic.Int32
	live := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
	})
	g := New(c, logr.Discard(), WithReader(live))
	_, err := g.ServedSpecHash(context.Background(), &dorguv1.RemediationAction{})
	require.Error(t, err, "no such action")
	assert.NotErrorIs(t, err, ErrNoLiveReader)
	assert.EqualValues(t, 1, reads.Load())
}
