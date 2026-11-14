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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ErrActionWriteRefused is returned by a guarded client that is asked to create or
// change a RemediationAction. Only the intake does that.
var ErrActionWriteRefused = errors.New(
	"refused: only the proposal intake may create or change a RemediationAction (status writes aside)")

const remediationActionKind = "RemediationAction"

// GuardActionWrites wraps c so that it refuses to Create, Update, Patch (every
// patch type, server-side apply included) or Apply a RemediationAction, however the
// object is spelled: typed, unstructured, partial metadata, or any client.Object
// whose kind the scheme resolves to RemediationAction. Status subresource writes,
// reads, deletes and every other kind pass through.
//
// This is what makes "the proposal intake is the only creator of a
// RemediationAction" structural. Every component is handed a guarded client; only
// the intake Processor gets the raw one (cmd/main.go). A scan of the source for a
// forbidden Create call cannot see a helper that returns the object, an
// unstructured object, a generic client.Object parameter, server-side apply, new(T),
// a type alias or a struct field; a client that checks the object it is given at
// the moment of the call sees all of them.
func GuardActionWrites(c client.Client) client.Client {
	return &actionGuard{Client: c}
}

type actionGuard struct {
	client.Client
}

// isAction reports whether obj is a RemediationAction in any representation.
func (g *actionGuard) isAction(obj runtime.Object) bool {
	switch obj.(type) {
	case *dorguv1.RemediationAction, *dorguv1.RemediationActionList:
		return true
	}
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind == "" {
		// A typed object carries no kind until the scheme is asked.
		if resolved, err := g.Client.GroupVersionKindFor(obj); err == nil {
			gvk = resolved
		}
	}
	return isActionKind(gvk)
}

func isActionKind(gvk schema.GroupVersionKind) bool {
	return gvk.Group == dorguv1.GroupVersion.Group && strings.EqualFold(gvk.Kind, remediationActionKind)
}

func (g *actionGuard) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if g.isAction(obj) {
		return ErrActionWriteRefused
	}
	return g.Client.Create(ctx, obj, opts...)
}

func (g *actionGuard) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if g.isAction(obj) {
		return ErrActionWriteRefused
	}
	return g.Client.Update(ctx, obj, opts...)
}

func (g *actionGuard) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if g.isAction(obj) {
		return ErrActionWriteRefused
	}
	return g.Client.Patch(ctx, obj, patch, opts...)
}

// Apply refuses an apply configuration that is a RemediationAction, and one whose
// kind cannot be told: a server-side apply creates the object when it is absent, so
// an unreadable one is not given the benefit of the doubt.
func (g *actionGuard) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	kind, group, known := applyKind(obj)
	if !known || isActionKind(schema.GroupVersionKind{Group: group, Kind: kind}) {
		return ErrActionWriteRefused
	}
	return g.Client.Apply(ctx, obj, opts...)
}

// applyKind reads the kind and API group from an apply configuration, which the
// generated ones expose as GetKind and GetAPIVersion.
func applyKind(obj runtime.ApplyConfiguration) (kind, group string, known bool) {
	k, ok := obj.(interface{ GetKind() *string })
	v, ok2 := obj.(interface{ GetAPIVersion() *string })
	if !ok || !ok2 || k.GetKind() == nil || v.GetAPIVersion() == nil {
		return "", "", false
	}
	gv, err := schema.ParseGroupVersion(*v.GetAPIVersion())
	if err != nil {
		return "", "", false
	}
	return *k.GetKind(), gv.Group, true
}
