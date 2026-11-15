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
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Locks serialises work per persona. The gate's rate limit, concurrency and dedup
// decisions read the actions that already exist; two proposals for one persona
// racing between that read and their create would each see nothing and both be
// written (R11). Holding a persona's lock from the first gate run to the create
// makes the second one's gate see the first's action. The operator runs a single
// leader, so an in-process lock is the whole of the exclusion needed.
//
// The locks are not reentrant. A caller that already holds a persona's lock says
// so (Hint.PersonaLocked) and the intake does not take it again; any other path
// that waits too long gets ErrLockTimeout instead of hanging.
type Locks struct {
	byPersona *keyedLocks
}

// NewLocks returns an empty set of locks.
func NewLocks() *Locks { return &Locks{byPersona: newKeyedLocks()} }

// PersonaKey is the lock key for a persona. defaultNamespace stands in for an
// empty namespace (a persona reference may omit it), so the planner, which holds
// the lock under the reference it was given, and the intake, which holds it under
// the proposal's target, always agree on the key.
func PersonaKey(ref *dorguv1.PersonaReference, defaultNamespace string) string {
	namespace := ref.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}
	return ref.Kind + "/" + namespace + "/" + ref.Name
}

// Lock takes the lock for a persona and returns the function that releases it. It
// waits as long as it takes; use Acquire where the holder may be stuck.
func (l *Locks) Lock(ref *dorguv1.PersonaReference) func() {
	unlock, _ := l.byPersona.acquire(context.Background(), PersonaKey(ref, ""), 0)
	return unlock
}

// Acquire takes the lock for a persona, giving up when ctx ends or after timeout
// (no limit when zero). defaultNamespace is the namespace an empty one means.
func (l *Locks) Acquire(
	ctx context.Context, ref *dorguv1.PersonaReference, defaultNamespace string, timeout time.Duration,
) (func(), error) {
	return l.byPersona.acquire(ctx, PersonaKey(ref, defaultNamespace), timeout)
}

// Len is how many personas are locked or waited for right now.
func (l *Locks) Len() int { return l.byPersona.len() }
