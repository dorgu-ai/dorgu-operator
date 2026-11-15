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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// C2 review F-4. The guard that stops tickets is only as current as its last run, so two
// things are added: it can be asked again at the moment a ticket would be issued
// (TicketsBlockedNow), and it remembers every stretch in which it was closed or could not
// have noticed (the time since its last good run), so an approval written then can be sent
// back (NotReadyAt).

// clock is a hand-set time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// switchable is a ticket-guard check (a reason that blocks tickets) whose answer a test
// flips.
func switchable(broken *atomic.Bool) Check {
	return Check{Name: "admission-policies", Reason: ReasonAdmissionPoliciesMissing, Run: func(context.Context) error {
		if broken.Load() {
			return errors.New("policy deleted")
		}
		return nil
	}}
}

func guardUnderTest(t *testing.T, checks ...Check) (*Preflight, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	p := newPreflightAt(logr.Discard(), c.now, checks...)
	return p, c
}

func TestTicketsBlockedNow_SeesAChangeTheLastPeriodicRunDidNot(t *testing.T) {
	var broken atomic.Bool
	p, _ := guardUnderTest(t, switchable(&broken))
	p.Run(context.Background())
	require.Nil(t, p.TicketsBlocked(), "the periodic run found everything in place")

	broken.Store(true) // a policy is deleted a second after that run

	assert.Nil(t, p.TicketsBlocked(), "the cached answer is stale: this is the window")
	now := p.TicketsBlockedNow(context.Background())
	require.NotNil(t, now, "asked again at ticket time, the guard sees it")
	assert.Equal(t, ReasonAdmissionPoliciesMissing, now.Reason)
	require.NotNil(t, p.TicketsBlocked(), "and what it found is what it now reports")

	broken.Store(false)
	assert.Nil(t, p.TicketsBlockedNow(context.Background()), "a restored policy reopens it at the next round")
	assert.Nil(t, p.TicketsBlocked())
}

func TestTicketsBlockedNow_RunsOnlyTheChecksThatGateTickets(t *testing.T) {
	var ticketRuns, otherRuns atomic.Int32
	p, _ := guardUnderTest(t,
		Check{Name: "policies", Reason: ReasonAdmissionPoliciesMissing, Run: func(context.Context) error { ticketRuns.Add(1); return nil }},
		Check{Name: "crd", Reason: ReasonProposalCRDMissing, Run: func(context.Context) error { otherRuns.Add(1); return nil }},
	)
	p.Run(context.Background())
	p.TicketsBlockedNow(context.Background())
	assert.EqualValues(t, 2, ticketRuns.Load())
	assert.EqualValues(t, 1, otherRuns.Load(), "a ticket does not wait for the CRD check")
}

func TestTicketsBlockedNow_NilPreflightBlocksNothing(t *testing.T) {
	var p *Preflight
	assert.Nil(t, p.TicketsBlockedNow(context.Background()))
	assert.False(t, p.NotReadyAt(time.Now()))
}

func TestNotReadyAt_TheStartupWindowRunsFromCreationToTheFirstGoodRun(t *testing.T) {
	var broken atomic.Bool
	p, c := guardUnderTest(t, switchable(&broken))
	created := c.t
	assert.True(t, p.NotReadyAt(created.Add(time.Second)), "before the first run nothing is known")

	c.advance(2 * time.Second)
	p.Run(context.Background())

	assert.True(t, p.NotReadyAt(created.Add(time.Second)), "an approval from the startup window stays suspect")
	assert.False(t, p.NotReadyAt(c.t.Add(time.Minute)), "after the first good run the guard is ready")
}

func TestNotReadyAt_ARecordedWindowStartsAtTheLastGoodRunNotWhenTheFailureWasSeen(t *testing.T) {
	var broken atomic.Bool
	p, c := guardUnderTest(t, switchable(&broken))
	c.advance(time.Second)
	p.Run(context.Background())
	lastGood := c.t

	// The policy is deleted 5 seconds into the minute, and nobody looks until the next tick.
	c.advance(45 * time.Second)
	approvedInTheGap := lastGood.Add(20 * time.Second)
	broken.Store(true)
	c.advance(15 * time.Second)
	p.Run(context.Background())
	require.NotNil(t, p.TicketsBlocked())

	c.advance(5 * time.Minute)
	broken.Store(false)
	c.advance(time.Minute)
	p.Run(context.Background())
	require.Nil(t, p.TicketsBlocked())

	assert.True(t, p.NotReadyAt(approvedInTheGap), "written between the last good run and the failure being seen")
	assert.True(t, p.NotReadyAt(lastGood.Add(3*time.Minute)), "written while it was closed")
	assert.False(t, p.NotReadyAt(lastGood.Add(-time.Minute)), "before the last good run")
	assert.False(t, p.NotReadyAt(c.t.Add(time.Minute)), "after it reopened")
}

func TestNotReadyAt_AnOpenWindowCoversNow(t *testing.T) {
	var broken atomic.Bool
	p, c := guardUnderTest(t, switchable(&broken))
	p.Run(context.Background())
	c.advance(time.Minute)
	broken.Store(true)
	p.Run(context.Background())

	c.advance(time.Hour)
	assert.True(t, p.NotReadyAt(c.t), "still closed")
}

func TestNotReadyAt_ASynchronousCheckOpensAndClosesWindowsToo(t *testing.T) {
	var broken atomic.Bool
	p, c := guardUnderTest(t, switchable(&broken))
	c.advance(time.Second)
	p.Run(context.Background())
	good := c.t

	c.advance(10 * time.Second)
	broken.Store(true)
	c.advance(10 * time.Second)
	require.NotNil(t, p.TicketsBlockedNow(context.Background()))
	c.advance(10 * time.Second)
	broken.Store(false)
	c.advance(10 * time.Second)
	require.Nil(t, p.TicketsBlockedNow(context.Background()))

	assert.True(t, p.NotReadyAt(good.Add(15*time.Second)))
	assert.False(t, p.NotReadyAt(c.t.Add(time.Minute)))
}

func TestNotReadyAt_AFailingCheckThatDoesNotGateTicketsOpensNoWindow(t *testing.T) {
	p, c := guardUnderTest(t,
		passing("a"),
		Check{Name: "crd", Reason: ReasonProposalCRDMissing, Run: func(context.Context) error { return errors.New("no crd") }},
	)
	c.advance(time.Second)
	p.Run(context.Background())
	good := c.t
	c.advance(time.Minute)
	p.Run(context.Background())

	assert.False(t, p.NotReadyAt(good.Add(30*time.Second)))
}

func TestNotReadyAt_ClockSkewWidensAWindowBothWays(t *testing.T) {
	var broken atomic.Bool
	p, c := guardUnderTest(t, switchable(&broken))
	p.Run(context.Background())
	good := c.t
	broken.Store(true)
	c.advance(time.Minute)
	p.Run(context.Background())
	broken.Store(false)
	c.advance(time.Minute)
	p.Run(context.Background())
	end := c.t

	assert.True(t, p.NotReadyAt(good.Add(-GuardClockMargin+time.Second)), "the API server's clock may be behind the operator's")
	assert.True(t, p.NotReadyAt(end.Add(GuardClockMargin-time.Second)), "or ahead")
	assert.False(t, p.NotReadyAt(end.Add(GuardClockMargin+time.Second)))
}

func TestNotReadyAt_ManyWindowsAreKeptBoundedWithoutForgettingAny(t *testing.T) {
	var broken atomic.Bool
	p, c := guardUnderTest(t, switchable(&broken))
	p.Run(context.Background())
	first := c.t
	for range 3 * maxGuardWindows {
		c.advance(time.Hour)
		broken.Store(true)
		p.Run(context.Background())
		c.advance(time.Minute)
		broken.Store(false)
		p.Run(context.Background())
	}
	p.mu.RLock()
	kept := len(p.windows)
	p.mu.RUnlock()
	assert.LessOrEqual(t, kept, maxGuardWindows)
	assert.True(t, p.NotReadyAt(first.Add(30*time.Minute)), "the oldest suspect stretch is still covered (folded into a wider one)")
}
