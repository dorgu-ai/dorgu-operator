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
	"sync"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Locks serialises work per persona. The gate's rate limit, concurrency and dedup
// decisions read the actions that already exist; two proposals for one persona
// racing between that read and their create would each see nothing and both be
// written (R11). Holding a persona's lock from the first gate run to the create
// makes the second one's gate see the first's action. The operator runs a single
// leader, so an in-process lock is the whole of the exclusion needed.
type Locks struct {
	byPersona sync.Map // persona key -> *sync.Mutex
}

// NewLocks returns an empty set of locks.
func NewLocks() *Locks { return &Locks{} }

// Lock takes the lock for a persona and returns the function that releases it.
func (l *Locks) Lock(ref *dorguv1.PersonaReference) func() {
	key := ref.Kind + "/" + ref.Namespace + "/" + ref.Name
	mu, _ := l.byPersona.LoadOrStore(key, &sync.Mutex{})
	lock := mu.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}
