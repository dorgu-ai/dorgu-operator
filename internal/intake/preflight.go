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
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Reasons a preflight check fails with. They are CamelCase because the ClusterPersona
// condition that reports them uses them as its reason.
const (
	// ReasonPreflightPending: the checks have not run yet. The intake is closed
	// until they have.
	ReasonPreflightPending = "PreflightPending"

	// ReasonOperatorIdentityUnknown: the operator does not know its own
	// ServiceAccount username (see OperatorUsername).
	ReasonOperatorIdentityUnknown = "OperatorIdentityUnknown"

	// ReasonProposalCRDMissing: the RemediationProposal CRD is not installed.
	ReasonProposalCRDMissing = "ProposalCRDMissing"

	// ReasonActionProvenanceMissing: the installed RemediationAction CRD has no
	// spec.provenance, so the API server would prune it from every action.
	ReasonActionProvenanceMissing = "ActionProvenanceMissing"
)

// DefaultPreflightInterval is how often the checks are run again.
const DefaultPreflightInterval = time.Minute

// Check is one thing that must be true before the intake may convert a proposal or
// the planner may plan: the operator's own identity is known, the CRDs it writes are
// installed with the fields it relies on. Admission policies (C2, task 2.8) are
// another: add a Check for them with Preflight.Add, nothing else needs to change.
type Check struct {
	// Name identifies the check in logs.
	Name string

	// Reason is the CamelCase token reported when the check fails.
	Reason string

	// Run returns nil when the check passes, and an error whose text says what is
	// wrong and what to do about it when it does not. An error with a
	// FailureReason() string method is reported with that reason instead of
	// Reason, for a check that can fail in more than one way.
	Run func(ctx context.Context) error
}

// Failure is one check that is failing.
type Failure struct {
	Check   string
	Reason  string
	Message string
}

// Preflight runs the Checks, remembers which fail, and says whether the intake is
// open. It fails closed: until the checks have run once, it is not ready.
//
// While anything fails, the intake converts no proposal and the planner is not asked
// to plan or to propose (nothing is spent on a model for a plan that could not be
// recorded), but detection, diagnosis and the lifecycle of existing actions go on.
// The operator does not crash-loop: it reports the failure (a ClusterPersona
// condition and a log line on every change) and checks again every interval.
//
// It implements manager.Runnable, without leader election, so every replica keeps
// its own answer.
type Preflight struct {
	log logr.Logger

	mu          sync.RWMutex
	checks      []Check
	failures    []Failure
	ran         bool
	interval    time.Duration
	subscribers []func([]Failure)
}

// NewPreflight builds a Preflight over checks.
func NewPreflight(log logr.Logger, checks ...Check) *Preflight {
	return &Preflight{log: log.WithName("preflight"), checks: checks, interval: DefaultPreflightInterval}
}

// Add appends a check. It takes effect from the next run.
func (p *Preflight) Add(c Check) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks = append(p.checks, c)
}

// SetInterval changes how often Start runs the checks.
func (p *Preflight) SetInterval(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.interval = d
	}
}

// Subscribe registers fn to be called, with the current failures, after every run.
func (p *Preflight) Subscribe(fn func([]Failure)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subscribers = append(p.subscribers, fn)
}

// Run evaluates every check once and returns what is failing.
func (p *Preflight) Run(ctx context.Context) []Failure {
	p.mu.RLock()
	checks := append([]Check(nil), p.checks...)
	p.mu.RUnlock()

	var failures []Failure
	for _, c := range checks {
		if err := runCheck(ctx, c); err != nil {
			failures = append(failures, Failure{Check: c.Name, Reason: failureReason(err, c.Reason), Message: err.Error()})
		}
	}

	p.mu.Lock()
	changed := !p.ran || !sameFailures(p.failures, failures)
	p.failures, p.ran = failures, true
	subscribers := append([]func([]Failure){}, p.subscribers...)
	p.mu.Unlock()

	if changed {
		p.report(failures)
	}
	for _, fn := range subscribers {
		fn(append([]Failure(nil), failures...))
	}
	return failures
}

// Report closes the intake at once with a failure found outside a check run: the
// create path noticed something the periodic checks have not seen yet. It lasts until
// the next Run, whose own answer replaces it.
func (p *Preflight) Report(f Failure) {
	p.mu.Lock()
	if slices.Contains(p.failures, f) {
		p.mu.Unlock()
		return
	}
	p.failures = append(append([]Failure(nil), p.failures...), f)
	p.ran = true
	p.mu.Unlock()
	p.report([]Failure{f})
}

// runCheck runs one check, turning a panic or an error with no text into a failure
// that says something.
func runCheck(ctx context.Context, c Check) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("check %s panicked: %v", c.Name, r)
		}
	}()
	if err = c.Run(ctx); err != nil && err.Error() == "" {
		err = fmt.Errorf("check %s failed with no message", c.Name)
	}
	return err
}

func (p *Preflight) report(failures []Failure) {
	if len(failures) == 0 {
		p.log.Info("preflight passed: the proposal intake is open")
		return
	}
	for _, f := range failures {
		p.log.Error(errors.New(f.Message), "preflight failed: the proposal intake is CLOSED. "+
			"No proposal is converted and nothing is planned until this is fixed; detection continues",
			"check", f.Check, "reason", f.Reason)
	}
}

func sameFailures(a, b []Failure) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Failures is what is failing now. Before the first run it is a single
// PreflightPending failure (when there are checks at all).
func (p *Preflight) Failures() []Failure {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ran && len(p.checks) > 0 {
		return []Failure{{Check: "preflight", Reason: ReasonPreflightPending, Message: "the startup checks have not run yet"}}
	}
	return append([]Failure(nil), p.failures...)
}

// Blocked returns the first failing check, or nil when the intake is open.
func (p *Preflight) Blocked() *Failure {
	if f := p.Failures(); len(f) > 0 {
		return &f[0]
	}
	return nil
}

// Start runs the checks every interval until ctx ends. It does not run them
// immediately: the caller runs them once before registering anything, so a closed
// intake is known at startup.
func (p *Preflight) Start(ctx context.Context) error {
	p.mu.RLock()
	interval := p.interval
	p.mu.RUnlock()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.Run(ctx)
		}
	}
}

// NeedLeaderElection is false: every replica reports its own readiness.
func (p *Preflight) NeedLeaderElection() bool { return false }

// NotReadyError is returned by Process while the preflight is failing. It is
// retryable, and decides nothing.
type NotReadyError struct {
	Failure Failure
}

func (e *NotReadyError) Error() string {
	return fmt.Sprintf("the proposal intake is closed (%s: %s)", e.Failure.Reason, e.Failure.Message)
}

// StaticCheck is a check whose answer is already known: it fails with err, or
// passes when err is nil. It carries a fact worked out at startup, such as the
// operator's identity.
func StaticCheck(name, reason string, err error) Check {
	return Check{Name: name, Reason: reason, Run: func(context.Context) error { return err }}
}
