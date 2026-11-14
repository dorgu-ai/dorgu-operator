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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// processWithin runs Process and fails the test if it has not returned in d: a
// lock inversion shows up as a hang, and a hang must not hang the suite.
func processWithin(t *testing.T, d time.Duration, w *world, name string, hint *Hint) (*Outcome, error) {
	t.Helper()
	type result struct {
		out *Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, hint)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(d):
		t.Fatal("DEADLOCK: Process did not return")
		return nil, nil
	}
}

// The reviewer's probe (C1 review F-1): the planner holds the persona lock for its
// whole proposal, and hands over a hint whose steps are not the proposal's.
func TestProcess_AMismatchedHintUnderTheCallersLockDoesNotDeadlock(t *testing.T) {
	w := newWorld(t, Config{OperatorUsername: testOperator}, baseObjects()...)
	w.create(t, proposal("p-hint", "alice", "k"))
	prop := w.get(t, "p-hint")

	draft := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	draft.Spec.Steps = []dorguv1.RemediationStep{{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "something else"}}

	ref := personaRefOf(prop)
	unlock := w.p.Locks().Lock(&ref) // what Propose does
	defer unlock()

	out, err := processWithin(t, 3*time.Second, w, "p-hint", &Hint{
		Draft: draft, Facts: gate.Facts{Incident: incident("inc", "ImagePullBackOff")}, PersonaLocked: true,
	})

	require.NoError(t, err)
	require.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
	assert.Equal(t, "0", out.Action.Spec.Confidence, "the mismatched hint was ignored, and nothing waited")
}

func TestProcess_WaitingForALockThatNeverFreesIsARetryableError(t *testing.T) {
	w := newWorld(t, Config{LockTimeout: 100 * time.Millisecond}, baseObjects()...)
	w.create(t, proposal("p", "alice", "k"))
	ref := personaRefOf(w.get(t, "p"))
	unlock := w.p.Locks().Lock(&ref)
	var once sync.Once
	release := func() { once.Do(unlock) }
	defer release()

	// No hint: the caller does not claim the lock, so Process waits, and gives up.
	_, err := processWithin(t, 3*time.Second, w, "p", nil)

	require.ErrorIs(t, err, ErrLockTimeout)
	assert.NotEqual(t, v1alpha1.PhaseConverted, w.get(t, "p").Status.Phase, "nothing was decided")

	release()
	out, err := processWithin(t, 3*time.Second, w, "p", nil)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.PhaseConverted, out.Phase, "the retry succeeds once the lock is free")
}

func TestProcess_AHintThatMatchesAlsoSkipsTheLockTheCallerHolds(t *testing.T) {
	w := newWorld(t, Config{OperatorUsername: testOperator, LockTimeout: 100 * time.Millisecond}, baseObjects()...)
	w.create(t, proposal("p-hint", "alice", "k"))
	prop := w.get(t, "p-hint")
	draft := derivedAction(prop, incident("inc", "ImagePullBackOff"))
	ref := personaRefOf(prop)
	unlock := w.p.Locks().Lock(&ref)
	defer unlock()

	out, err := processWithin(t, 3*time.Second, w, "p-hint", &Hint{
		Draft: draft, Facts: gate.Facts{Incident: incident("inc", "ImagePullBackOff")}, PersonaLocked: true,
	})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.PhaseConverted, out.Phase)
}

func TestPersonaKey_AnEmptyNamespaceMeansTheDefault(t *testing.T) {
	bare := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "web"}
	full := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "web", Namespace: "default"}
	assert.Equal(t, PersonaKey(full, ""), PersonaKey(bare, "default"))
	assert.NotEqual(t, PersonaKey(full, ""), PersonaKey(bare, ""))

	l := NewLocks()
	unlock, err := l.Acquire(context.Background(), bare, "default", 0)
	require.NoError(t, err)
	_, err = l.Acquire(context.Background(), full, "", 50*time.Millisecond)
	assert.ErrorIs(t, err, ErrLockTimeout, "the same persona under either spelling is one lock")
	unlock()
}

func TestKeyedLocks_ContextEndsTheWait(t *testing.T) {
	k := newKeyedLocks()
	unlock, err := k.acquire(context.Background(), "a", 0)
	require.NoError(t, err)
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = k.acquire(ctx, "a", 0)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, k.len(), "the give-up left only the holder's entry")
}

func TestKeyedLocks_ReleaseIsIdempotent(t *testing.T) {
	k := newKeyedLocks()
	unlock, err := k.acquire(context.Background(), "a", 0)
	require.NoError(t, err)
	unlock()
	unlock()
	again, err := k.acquire(context.Background(), "a", 50*time.Millisecond)
	require.NoError(t, err, "a double release must not hand out the lock twice or poison it")
	again()
}

func TestKeyedLocks_MutualExclusionAndShrink(t *testing.T) {
	k := newKeyedLocks()
	var inside, max int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock, err := k.acquire(context.Background(), fmt.Sprintf("k%d", i%4), 0)
			if err != nil {
				return
			}
			mu.Lock()
			inside++
			if inside > max {
				max = inside
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			unlock()
		}(i)
	}
	wg.Wait()
	assert.LessOrEqual(t, max, 4, "at most one holder per key")
	assert.Zero(t, k.len(), "no entry outlives its last holder")
}

func TestLocks_DoNotGrowWithEveryPersonaEverSeen(t *testing.T) {
	l := NewLocks()
	for i := range 500 {
		ref := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Namespace: testNS, Name: fmt.Sprintf("flood-%d", i)}
		l.Lock(ref)()
	}
	assert.Zero(t, l.Len(), "a flood of proposals naming new personas leaves nothing behind")
}

func TestProcess_PerProposalLocksAreForgotten(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	for i := range 5 {
		name := fmt.Sprintf("p-%d", i)
		w.create(t, proposal(name, "alice", name))
		w.process(t, name, nil)
	}
	assert.Zero(t, w.p.perProposal.len())
	assert.Zero(t, w.p.Locks().Len())
}
