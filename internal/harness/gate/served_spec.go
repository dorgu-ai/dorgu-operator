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
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ErrSpecMoved means the RemediationAction the API server serves is not the
// object the caller holds: its spec was edited (metadata.generation moved) or the
// action was deleted and recreated (another UID) after the caller read it. No
// hash may be taken of the served spec for the object in hand.
var ErrSpecMoved = errors.New("the RemediationAction was changed after it was read")

// ErrSpecUnhashable means the served spec is missing or is not a document JCS can
// canonicalise. A stored spec never is; it is refused, not guessed at.
var ErrSpecUnhashable = errors.New("the served spec cannot be hashed")

var remediationActionKind = dorguv1.GroupVersion.WithKind("RemediationAction")

// ServedSpecHash is the specHash of the action's .spec exactly as the API server
// serves it now (P2-D14): an unstructured read, the "spec" object of it, hashed
// with dorguv1.SpecHashFromMap. It is never the hash of the typed Go struct, whose
// JSON drops empty arrays, re-spells durations and re-zones timestamps, so that a
// CLI, a dashboard or a non-Go client hashing the served spec would disagree.
//
// The read is live (the gate's uncached reader, the API server's own answer), at
// ticket issue and at apply alike. The reconciler's typed object comes from the
// informer cache, which lags: at apply a cached spec could still be the one that
// was approved after somebody edited it, which is the edit this hash exists to see.
//
// The served object must be the one in hand: the same UID and the same
// metadata.generation as action. Otherwise ErrSpecMoved is returned (wrapped): at
// issue the caller retries with a fresher read, and at apply the spec has been
// edited since the ticket was cleared. Any other error means the API server could
// not be read, and the caller must treat it as a refusal to proceed.
func (g *Gate) ServedSpecHash(ctx context.Context, action *dorguv1.RemediationAction) (string, error) {
	if g == nil || action == nil {
		return "", errors.New("no gate or no action to hash")
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(remediationActionKind)
	if err := g.reader.Get(ctx, client.ObjectKeyFromObject(action), live); err != nil {
		return "", fmt.Errorf("reading the RemediationAction as the API server serves it: %w", err)
	}
	if live.GetUID() != action.UID || live.GetGeneration() != action.Generation {
		return "", fmt.Errorf("%w (generation %d, now %d)", ErrSpecMoved, action.Generation, live.GetGeneration())
	}
	spec, found, err := unstructured.NestedMap(live.Object, "spec")
	if err != nil || !found {
		return "", fmt.Errorf("%w: the served RemediationAction has no spec object", ErrSpecUnhashable)
	}
	hash, err := dorguv1.SpecHashFromMap(spec)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrSpecUnhashable, err)
	}
	return hash, nil
}
