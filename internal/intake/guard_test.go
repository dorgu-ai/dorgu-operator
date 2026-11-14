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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

// The shapes below are the seven the C1 review used to slip a RemediationAction
// create past the syntactic scan. Every one reaches the guarded client as an
// ordinary call, so every one is refused where the object is known.

// RA is a type alias for the action (shape 6).
type RA = dorguv1.RemediationAction

func newRA(name string) *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}}
}

// holder keeps an action in a struct field (shape 7).
type holder struct{ ra *dorguv1.RemediationAction }

// generic is a helper that takes any client.Object (shape 3).
func generic(ctx context.Context, c client.Client, obj client.Object) error {
	return c.Create(ctx, obj)
}

func unstructuredRA(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(dorguv1.GroupVersion.WithKind("RemediationAction"))
	u.SetName(name)
	u.SetNamespace(testNS)
	return u
}

func guardedWorld(t *testing.T) (guarded, raw client.Client) {
	t.Helper()
	raw = fake.NewClientBuilder().WithScheme(scheme(t)).
		WithStatusSubresource(&dorguv1.RemediationAction{}, &v1alpha1.RemediationProposal{}).Build()
	return GuardActionWrites(raw), raw
}

func actionCount(t *testing.T, c client.Client) int {
	t.Helper()
	var list dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &list))
	return len(list.Items)
}

func TestGuardActionWrites_RefusesEveryCreationShape(t *testing.T) {
	ctx := context.Background()
	h := holder{ra: &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "shape-7", Namespace: testNS}}}
	raPtr := new(dorguv1.RemediationAction)
	raPtr.Name, raPtr.Namespace = "shape-5", testNS
	aliased := &RA{ObjectMeta: metav1.ObjectMeta{Name: "shape-6", Namespace: testNS}}

	shapes := map[string]func(c client.Client) error{
		"1 unstructured with the RemediationAction GVK": func(c client.Client) error {
			return c.Create(ctx, unstructuredRA("shape-1"))
		},
		"2 a helper returns the object": func(c client.Client) error { return c.Create(ctx, newRA("shape-2")) },
		"3 a generic client.Object parameter": func(c client.Client) error {
			return generic(ctx, c, newRA("shape-3"))
		},
		"4 server-side apply through Patch": func(c client.Client) error {
			//nolint:staticcheck // the deprecated shape is the bypass under test
			return c.Patch(ctx, newRA("shape-4"), client.Apply,
				client.FieldOwner("x"), client.ForceOwnership)
		},
		"5 new(T)":         func(c client.Client) error { return c.Create(ctx, raPtr) },
		"6 a type alias":   func(c client.Client) error { return c.Create(ctx, aliased) },
		"7 a struct field": func(c client.Client) error { return c.Create(ctx, h.ra) },
		"8 apply configuration": func(c client.Client) error {
			return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(unstructuredRA("shape-8")))
		},
		"9 merge patch": func(c client.Client) error {
			return c.Patch(ctx, newRA("shape-9"), client.RawPatch("merge", []byte(`{}`)))
		},
		"10 update":              func(c client.Client) error { return c.Update(ctx, newRA("shape-10")) },
		"11 unstructured update": func(c client.Client) error { return c.Update(ctx, unstructuredRA("shape-11")) },
		"12 partial object metadata": func(c client.Client) error {
			m := &metav1.PartialObjectMetadata{}
			m.SetGroupVersionKind(dorguv1.GroupVersion.WithKind("RemediationAction"))
			m.SetName("shape-12")
			m.SetNamespace(testNS)
			return c.Patch(ctx, m, client.RawPatch("merge", []byte(`{}`)))
		},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			guarded, raw := guardedWorld(t)
			err := shape(guarded)
			require.ErrorIs(t, err, ErrActionWriteRefused)
			assert.Zero(t, actionCount(t, raw), "nothing reached the API server")
		})
	}
}

func TestGuardActionWrites_AllowsEverythingElse(t *testing.T) {
	ctx := context.Background()
	guarded, raw := guardedWorld(t)

	// The raw client (the intake's) creates; the guarded one can still read it and
	// write its status, which is how every other component works with actions.
	ra := newRA("existing")
	require.NoError(t, raw.Create(ctx, ra))

	var got dorguv1.RemediationAction
	require.NoError(t, guarded.Get(ctx, client.ObjectKeyFromObject(ra), &got))
	got.Status.Phase = "Approved"
	require.NoError(t, guarded.Status().Update(ctx, &got), "status writes stay allowed")
	require.NoError(t, guarded.SubResource("status").Update(ctx, &got))
	patched := got.DeepCopy()
	patched.Status.Phase = "Executing"
	require.NoError(t, guarded.Status().Patch(ctx, patched, client.MergeFrom(&got)), "status patches stay allowed")

	var list dorguv1.RemediationActionList
	require.NoError(t, guarded.List(ctx, &list))
	assert.Len(t, list.Items, 1)

	// Other kinds are untouched, including the intake's own proposals and the unstructured form of them.
	prop := &v1alpha1.RemediationProposal{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: testNS}}
	require.NoError(t, guarded.Create(ctx, prop))
	require.NoError(t, guarded.Update(ctx, prop))
	require.NoError(t, guarded.Patch(ctx, prop, client.MergeFrom(prop.DeepCopy())))
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(dorguv1.GroupVersion.WithKind("IncidentMemory"))
	u.SetName("im")
	u.SetNamespace(testNS)
	u.Object["spec"] = map[string]any{}
	require.NoError(t, guarded.Create(ctx, u))

	// Deleting an action is not creating one.
	require.NoError(t, guarded.Delete(ctx, &got))
	assert.Zero(t, actionCount(t, raw))
}

func TestGuardActionWrites_AnApplyThatCannotBeIdentifiedIsRefused(t *testing.T) {
	guarded, _ := guardedWorld(t)
	err := guarded.Apply(context.Background(), opaqueApply{})
	require.ErrorIs(t, err, ErrActionWriteRefused)
}

// opaqueApply is an apply configuration that says nothing about its kind.
type opaqueApply struct{}

func (opaqueApply) IsApplyConfiguration() {}

// The intake itself holds the raw client, so the guard does not stop the one
// component that is meant to create actions.
func TestIntakeStillCreatesThroughItsOwnClient(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.create(t, proposal("p", "alice", "k"))
	out := w.process(t, "p", nil)
	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
}
