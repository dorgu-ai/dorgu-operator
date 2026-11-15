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
	"slices"
	"strings"
	"time"
)

// GuardClockMargin widens every guard window at both ends. The windows are the operator's
// clock and the time an approval was written is the API server's: with either ahead, a write
// that really fell inside a window carries a stamp just outside it. The cost of erring this way
// is one approval asked for again, never an approval honoured that should not be.
const GuardClockMargin = 5 * time.Second

// maxGuardWindows bounds the windows kept. Past it the two oldest are folded into one wider
// window, so nothing that was suspect is ever forgotten, only remembered more coarsely.
const maxGuardWindows = 64

// guardWindow is a stretch of time in which the ticket guard was not ready: closed, or open
// but unable to have noticed that it should have been closed.
type guardWindow struct{ from, to time.Time }

// ticketReady reports whether no failure stops tickets.
func ticketReady(failures []Failure) bool {
	for _, f := range failures {
		if ticketBlockers[f.Reason] {
			return false
		}
	}
	return true
}

// ticketChecks are the checks whose failure stops tickets: the admission policies and the
// operator's own permissions (and a static failure of either).
func (p *Preflight) ticketChecks() []Check {
	var out []Check
	for _, c := range p.checks {
		if ticketBlockers[c.Reason] {
			out = append(out, c)
		}
	}
	return out
}

// observeGuardLocked records what a run that started at start and ended at end found. Callers
// hold p.mu.
//
// A window opens at the START of the last run that found the guard ready, not when the failure
// was seen: a policy deleted just after a good run is invisible until the next one, and an
// approval written in between was never checked by anything. It closes at the END of the first
// run that finds the guard ready again, because that run's reads happen somewhere inside it.
func (p *Preflight) observeGuardLocked(start, end time.Time, ready bool) {
	if !ready {
		if p.notReadySince.IsZero() {
			p.notReadySince = p.lastReady
		}
		return
	}
	if !p.notReadySince.IsZero() {
		p.windows = append(p.windows, guardWindow{from: p.notReadySince, to: end})
		p.notReadySince = time.Time{}
		p.foldWindowsLocked()
	}
	p.lastReady = start
}

// foldWindowsLocked keeps the windows within maxGuardWindows.
func (p *Preflight) foldWindowsLocked() {
	for len(p.windows) > maxGuardWindows {
		merged := guardWindow{from: p.windows[0].from, to: p.windows[1].to}
		p.windows = append([]guardWindow{merged}, p.windows[2:]...)
	}
}

// NotReadyAt reports whether the ticket guard was not ready at t: closed, in its startup
// window, or open but long enough since a good run that it could have missed a change. An
// approval written at such a time was not checked by working admission policies, and the
// operator asks for it again (C2 F-4). A nil Preflight (no health check) was always ready.
func (p *Preflight) NotReadyAt(t time.Time) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.notReadySince.IsZero() && !t.Before(p.notReadySince.Add(-GuardClockMargin)) {
		return true
	}
	return slices.ContainsFunc(p.windows, func(w guardWindow) bool {
		return !t.Before(w.from.Add(-GuardClockMargin)) && !t.After(w.to.Add(GuardClockMargin))
	})
}

// EverNotReady reports whether the guard has ever been not ready: it has recorded a window, or
// is in one. Before that there is nothing to compare an approval's time with.
func (p *Preflight) EverNotReady() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.notReadySince.IsZero() || len(p.windows) > 0
}

// TicketsBlockedNow runs the checks that gate tickets again, now, and returns the first that
// fails, or nil. It is what the operator asks right before it issues a ticket: the answer of the
// last periodic run can be a minute old, and a policy deleted since is invisible to it (C2
// F-4). What it finds replaces the stored answer for those checks (so the ClusterPersona
// condition follows) and feeds the guard windows. A nil Preflight blocks nothing.
//
// What keeps this cheap (C2 re-review N-6) is that checks marked PeriodicOnly (the permission
// reviews) are not run here: their last periodic result still blocks tickets, and until the
// first periodic run nothing is known about them, so the guard is pending, not open. What is
// left is a few GETs of the policies and their bindings, read fresh at every ticket. No answer
// is shared between tickets, however close together they are: a policy deleted right after a
// clean check is seen by the next ticket (C2 re-check 2 R-2). The operator reconciles serially,
// so there was nothing to save.
func (p *Preflight) TicketsBlockedNow(ctx context.Context) *Failure {
	if p == nil {
		return nil
	}
	// One at a time: a burst of approvals asks the API server in turn, not all at once.
	p.syncMu.Lock()
	defer p.syncMu.Unlock()

	p.mu.RLock()
	var checks []Check
	periodicOnly := false
	for _, c := range p.ticketChecks() {
		if c.PeriodicOnly {
			periodicOnly = true
		} else {
			checks = append(checks, c)
		}
	}
	timeout, ran := p.timeout, p.ran
	p.mu.RUnlock()

	if periodicOnly && !ran {
		return &Failure{Check: "preflight", Reason: ReasonPreflightPending, Message: "the startup checks have not run yet"}
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := p.now()
	var fresh []Failure
	for _, c := range checks {
		if err := runCheck(rctx, c); err != nil {
			fresh = append(fresh, Failure{Check: c.Name, Reason: failureReason(err, c.Reason), Message: err.Error()})
		}
	}
	end := p.now()

	p.mu.Lock()
	var (
		changed     bool
		subscribers []func([]Failure)
		merged      []Failure
	)
	if p.ran {
		ours := func(f Failure) bool {
			return slices.ContainsFunc(checks, func(c Check) bool { return c.Name == f.Check })
		}
		for _, f := range p.failures {
			if !ours(f) {
				merged = append(merged, f)
			}
		}
	}
	// The guard is ready only if what this round found and what the periodic checks last
	// found (not re-run here) both are.
	p.observeGuardLocked(start, end, ticketReady(fresh) && ticketReady(merged))
	if p.ran {
		merged = append(merged, fresh...)
		changed = !sameFailures(sortedFailures(p.failures), sortedFailures(merged))
		p.failures = merged
		subscribers = append(subscribers, p.subscribers...)
	}
	p.mu.Unlock()

	if changed {
		p.report(merged)
		for _, fn := range subscribers {
			fn(append([]Failure(nil), merged...))
		}
	}
	return p.ticketAnswer(fresh, checks)
}

// ticketAnswer is the first failure that stops tickets: one a round just found, or else one a
// check that is not run at ticket time last found.
func (p *Preflight) ticketAnswer(fresh []Failure, ran []Check) *Failure {
	for i := range fresh {
		if ticketBlockers[fresh[i].Reason] {
			return &fresh[i]
		}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ran {
		return nil
	}
	for _, f := range p.failures {
		if ticketBlockers[f.Reason] && !slices.ContainsFunc(ran, func(c Check) bool { return c.Name == f.Check }) {
			return &f
		}
	}
	return nil
}

// sortedFailures is a copy ordered by check name, so two answers that differ only in the
// order they were assembled compare equal.
func sortedFailures(in []Failure) []Failure {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b Failure) int {
		if a.Check != b.Check {
			return strings.Compare(a.Check, b.Check)
		}
		return strings.Compare(a.Reason, b.Reason)
	})
	return out
}
