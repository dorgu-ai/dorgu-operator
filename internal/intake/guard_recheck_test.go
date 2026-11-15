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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"
)

// C2 re-review N-6: the check made at the moment of a ticket cost 34 SelfSubjectAccessReviews
// and 7 other calls, 1.7 s each once the client's rate limit was spent, and a queue of
// approvals paid it one by one. The permission reviews now run on the periodic schedule only
// (their last result still blocks tickets). C2 re-check 2 R-2: the answer is never shared
// between tickets, not even for a moment; every ticket reads the policies fresh (a few GETs, no
// access reviews), and callers are served one at a time.

func counting(name, reason string, n *atomic.Int32, periodicOnly bool, broken *atomic.Bool) Check {
	return Check{Name: name, Reason: reason, PeriodicOnly: periodicOnly, Run: func(context.Context) error {
		n.Add(1)
		if broken != nil && broken.Load() {
			return errors.New(name + " is failing")
		}
		return nil
	}}
}

func TestTicketsBlockedNow_DoesNotRunThePeriodicOnlyChecks(t *testing.T) {
	var policies, rbac atomic.Int32
	p, c := guardUnderTest(t,
		counting("admission-policies", ReasonAdmissionPoliciesMissing, &policies, false, nil),
		counting("operator-rbac", ReasonOperatorRBACMissing, &rbac, true, nil),
	)
	p.Run(context.Background())
	require.EqualValues(t, 1, rbac.Load())

	for range 3 {
		c.advance(time.Second)
		assert.Nil(t, p.TicketsBlockedNow(context.Background()))
	}
	assert.EqualValues(t, 4, policies.Load(), "the policy check runs at every ticket")
	assert.EqualValues(t, 1, rbac.Load(), "the permission reviews wait for the periodic run")
}

func TestTicketsBlockedNow_ThePeriodicOnlyChecksLastResultStillBlocks(t *testing.T) {
	var policies, rbac atomic.Int32
	var rbacBroken atomic.Bool
	rbacBroken.Store(true)
	p, c := guardUnderTest(t,
		counting("admission-policies", ReasonAdmissionPoliciesMissing, &policies, false, nil),
		counting("operator-rbac", ReasonOperatorRBACMissing, &rbac, true, &rbacBroken),
	)
	c.advance(time.Second)
	p.Run(context.Background()) // the periodic run finds the permission gone

	c.advance(time.Minute)
	blocked := p.TicketsBlockedNow(context.Background())
	require.NotNil(t, blocked, "a ticket is not issued on a permission the last periodic run found missing")
	assert.Equal(t, ReasonOperatorRBACMissing, blocked.Reason)
	assert.True(t, p.NotReadyAt(c.t), "a good policy check does not close a window the permission failure holds open")
	assert.NotNil(t, p.TicketsBlocked())

	rbacBroken.Store(false)
	c.advance(time.Minute)
	p.Run(context.Background())
	c.advance(time.Minute)
	assert.Nil(t, p.TicketsBlockedNow(context.Background()), "the periodic run that finds it back reopens tickets")
	assert.False(t, p.NotReadyAt(c.t))
}

func TestTicketsBlockedNow_BeforeAnyPeriodicRunTheGuardIsPendingNotOpen(t *testing.T) {
	var policies, rbac atomic.Int32
	p, c := guardUnderTest(t,
		counting("admission-policies", ReasonAdmissionPoliciesMissing, &policies, false, nil),
		counting("operator-rbac", ReasonOperatorRBACMissing, &rbac, true, nil),
	)
	blocked := p.TicketsBlockedNow(context.Background())
	require.NotNil(t, blocked)
	assert.Equal(t, ReasonPreflightPending, blocked.Reason, "the permissions were never checked: nothing is known")
	assert.EqualValues(t, 0, rbac.Load())
	assert.True(t, p.NotReadyAt(c.t), "and the startup window stays open")
}

func TestTicketsBlockedNow_ConcurrentCallersEachReadFreshOneAtATime(t *testing.T) {
	var policies, inFlight, maxInFlight atomic.Int32
	slow := Check{Name: "admission-policies", Reason: ReasonAdmissionPoliciesMissing, Run: func(context.Context) error {
		policies.Add(1)
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	}}
	p, _ := guardUnderTest(t, slow)
	p.Run(context.Background())
	policies.Store(0)

	var wg sync.WaitGroup
	answers := make([]*Failure, 30)
	for i := range answers {
		wg.Go(func() { answers[i] = p.TicketsBlockedNow(context.Background()) })
	}
	wg.Wait()

	assert.EqualValues(t, 30, policies.Load(), "every caller reads for itself: no answer is shared")
	assert.EqualValues(t, 1, maxInFlight.Load(), "but one at a time, so a burst does not hit the API server at once")
	for _, a := range answers {
		assert.Nil(t, a)
	}
}

// C2 re-check 2 R-2: a policy deleted right after a clean pre-ticket check is seen by the very
// next ticket, however close behind it is.
func TestTicketsBlockedNow_APolicyDeletedRightAfterACleanCheckBlocksTheNextTicket(t *testing.T) {
	var policies atomic.Int32
	var broken atomic.Bool
	p, _ := guardUnderTest(t, counting("admission-policies", ReasonAdmissionPoliciesMissing, &policies, false, &broken))
	p.Run(context.Background())
	policies.Store(0)

	require.Nil(t, p.TicketsBlockedNow(context.Background()))
	broken.Store(true) // a policy is deleted right after, with no time passing at all
	blocked := p.TicketsBlockedNow(context.Background())
	require.NotNil(t, blocked, "the next ticket reads again and finds it")
	assert.Equal(t, ReasonAdmissionPoliciesMissing, blocked.Reason)
	assert.EqualValues(t, 2, policies.Load())

	// and an outage is read every time too: nothing is remembered between tickets
	require.NotNil(t, p.TicketsBlockedNow(context.Background()))
	assert.EqualValues(t, 3, policies.Load())
}

// C2 re-check 2 R-3: the guard is ready only if what the round found and what the periodic
// checks last found (and the round did not re-run) both are. A round with clean policies must
// not close a window the permission failure holds open: an approval written during the
// permission outage, after such a round, is voided once the permission is back.
func TestTicketsBlockedNow_ReadinessIsOverTheMergedFailures(t *testing.T) {
	var policies, rbac atomic.Int32
	var rbacBroken atomic.Bool
	rbacBroken.Store(true)
	p, c := guardUnderTest(t,
		counting("admission-policies", ReasonAdmissionPoliciesMissing, &policies, false, nil),
		counting("operator-rbac", ReasonOperatorRBACMissing, &rbac, true, &rbacBroken),
	)
	ctx := context.Background()
	c.advance(time.Second)
	p.Run(ctx) // the periodic run: permission missing, the guard is not ready

	c.advance(10 * time.Second)
	require.NotNil(t, p.TicketsBlockedNow(ctx), "a round with clean policies is still blocked by the permission")

	c.advance(10 * time.Second)
	duringOutage := c.t // written well after that round, while the permission is still missing
	c.advance(10 * time.Second)

	rbacBroken.Store(false)
	p.Run(ctx) // the periodic run finds the permission back and closes the window
	assert.True(t, p.NotReadyAt(duringOutage),
		"the window opened at the last good run and was not closed by a round that never checked the permission")
}

func TestTicketsBlockedNow_ACallerThatGaveUpDoesNotPoisonTheNext(t *testing.T) {
	var policies atomic.Int32
	check := Check{Name: "admission-policies", Reason: ReasonAdmissionPoliciesMissing, Run: func(ctx context.Context) error {
		policies.Add(1)
		return ctx.Err()
	}}
	p, _ := guardUnderTest(t, check)
	p.Run(context.Background())
	policies.Store(0)

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	require.NotNil(t, p.TicketsBlockedNow(gone), "its own answer is a failure")

	assert.Nil(t, p.TicketsBlockedNow(context.Background()), "but nobody else is told the API server failed")
	assert.EqualValues(t, 2, policies.Load())
}

func TestOperatorRBACCheck_IsPeriodicOnlyAndTicketTimeMakesNoAccessReviews(t *testing.T) {
	var reviews atomic.Int32
	review := func(context.Context, authzv1.ResourceAttributes) (bool, string, error) {
		reviews.Add(1)
		return true, "", nil
	}
	rbac := OperatorRBACCheck(review)
	assert.True(t, rbac.PeriodicOnly)

	var policies atomic.Int32
	p, c := guardUnderTest(t, counting("admission-policies", ReasonAdmissionPoliciesMissing, &policies, false, nil), rbac)
	p.Run(context.Background())
	periodic := reviews.Load()
	require.Positive(t, periodic, "the periodic run asks for every permission")

	for range 5 {
		c.advance(time.Second)
		require.Nil(t, p.TicketsBlockedNow(context.Background()))
	}
	assert.Equal(t, periodic, reviews.Load(), "five tickets, no further SelfSubjectAccessReview")
}
