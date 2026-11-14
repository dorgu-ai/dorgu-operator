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
	"sync"
	"time"
)

// ErrLockTimeout is returned when a lock could not be taken in time. It means a
// holder never let go (a bug) or is very slow; it is always safe to retry.
var ErrLockTimeout = errors.New("timed out waiting for a lock")

// keyedLocks is a set of mutexes addressed by key. An entry exists only while
// someone holds or waits for it, so the set never outgrows the work in flight,
// whatever keys a flood of proposals names.
//
// A lock is a one-slot channel rather than a sync.Mutex so that a waiter can give
// up on a context or a deadline: an inversion becomes a retryable error, not a
// goroutine that waits forever.
type keyedLocks struct {
	mu      sync.Mutex
	entries map[string]*keyedEntry
}

type keyedEntry struct {
	slot chan struct{} // holds one token while the lock is free
	refs int           // holders plus waiters
}

func newKeyedLocks() *keyedLocks {
	return &keyedLocks{entries: map[string]*keyedEntry{}}
}

// acquire takes the lock for key, waiting at most timeout (no limit when it is
// zero) and never past ctx. It returns the function that releases it.
func (k *keyedLocks) acquire(ctx context.Context, key string, timeout time.Duration) (func(), error) {
	k.mu.Lock()
	e := k.entries[key]
	if e == nil {
		e = &keyedEntry{slot: make(chan struct{}, 1)}
		e.slot <- struct{}{}
		k.entries[key] = e
	}
	e.refs++
	k.mu.Unlock()

	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-e.slot:
		var once sync.Once
		return func() {
			once.Do(func() {
				e.slot <- struct{}{}
				k.release(key, e)
			})
		}, nil
	case <-ctx.Done():
		k.release(key, e)
		return nil, ctx.Err()
	case <-expired:
		k.release(key, e)
		return nil, ErrLockTimeout
	}
}

// release drops one reference and forgets the entry when nobody is left with it.
func (k *keyedLocks) release(key string, e *keyedEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.refs--
	if e.refs == 0 && k.entries[key] == e {
		delete(k.entries, key)
	}
}

// len is how many keys are held or waited for.
func (k *keyedLocks) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}
