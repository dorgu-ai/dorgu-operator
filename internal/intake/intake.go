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

// Package intake is the one door into the remediation system.
//
// A RemediationProposal is the only way a change request enters Dorgu, whether it
// comes from Dorgu's own planner or from an external agent. Processor decides each
// one: it dedupes by idempotency key, rate limits each proposer, makes sure an
// incident exists, asks the gate, and then either creates the RemediationAction or
// rejects the proposal with the stage and reason that refused it.
//
// Processor.createAction is the ONLY place in the operator that creates a
// RemediationAction, and that is structural: every other component is handed a
// client wrapped by GuardActionWrites, which refuses to create, update, patch or
// apply a RemediationAction in any form, and only the Processor holds the raw client
// (cmd/clients.go). A source scan (TestNoStaticRemediationActionCreateOutsideIntake)
// is a cheap extra tripwire.
//
// A proposal's rationale is untrusted text. It is stripped of control characters
// (safetext.Clean) and copied to the action's explanation for people to read. No
// branch, guardrail or gate input looks at it (KPI I5), and it is never put in a
// model's prompt (planner.BuildContext shows only the planner's own explanations).
package intake

import (
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// Intake limits (P2-D12).
const (
	// DefaultRateLimit is how many proposals one proposer may have received in
	// RateWindow before the next is rejected with RateLimited.
	DefaultRateLimit = 30

	// DefaultRateWindow is the window the rate limit counts over.
	DefaultRateWindow = time.Hour

	// DefaultRejectedTTL is how long a Rejected proposal is kept before it is pruned.
	DefaultRejectedTTL = 7 * 24 * time.Hour

	// DefaultLockTimeout is the longest Process waits for a persona or proposal
	// lock. It is longer than any planning call, which holds the persona lock.
	DefaultLockTimeout = 10 * time.Minute
)

// TestOperatorUsername is a username for tests that need an operator identity. No
// production code reads it: an operator that cannot work out its own username
// closes the intake instead of using one (OperatorUsername, OperatorIdentityCheck).
const TestOperatorUsername = "system:serviceaccount:dorgu-system:dorgu-operator"

// Config configures a Processor.
type Config struct {
	// Client reads and writes proposals, incidents and actions.
	Client client.Client

	// Reader is the uncached reader the intake decides on, so that a proposal or
	// action written a moment ago is visible to the next decision. Defaults to Client.
	Reader client.Reader

	// Lister serves the two Lists the intake makes for every undecided proposal:
	// every proposal (dedupe and the rate limit) and the namespace's actions (the
	// one an earlier attempt made). It is the manager's cached reader, so a burst of
	// proposals does not cost a cluster-wide List against the API server each. A
	// slightly stale answer is safe: the arrival order is total and stable, so a
	// stale cache can only miss the newest proposals, never the earlier ones that
	// decide a duplicate, and an action is re-read from the API server before it
	// is removed. Defaults to Reader.
	Lister client.Reader

	// Gate rules on every proposal.
	Gate *gate.Gate

	// Incidents opens an incident for a proposal that names none. May be nil, in
	// which case such a proposal cannot be converted yet.
	Incidents IncidentOpener

	// Locks serialises work per persona. It is shared with the planner so a proposal
	// and an action for one persona never race through the gate. Defaults to a new one.
	Locks *Locks

	// OperatorUsername is the operator's own Kubernetes username. Dorgu's planner
	// proposes as this user (P2-D3). Empty means unknown, and the intake is then
	// closed: it is never defaulted.
	OperatorUsername string

	// Preflight says whether the intake is open. Nil means there is nothing to
	// check. The same Preflight is shared with the planner (Blocked).
	Preflight *Preflight

	// RateLimit, RateWindow and RejectedTTL default to the Default* constants.
	RateLimit   int
	RateWindow  time.Duration
	RejectedTTL time.Duration

	// LockTimeout bounds how long Process waits for a proposal or persona lock
	// before it gives up with ErrLockTimeout. Defaults to DefaultLockTimeout.
	LockTimeout time.Duration

	// Now is the clock; defaults to time.Now.
	Now func() time.Time

	Logger logr.Logger
}

// Processor decides proposals. Build one with New and share it.
type Processor struct {
	client    client.Client
	reader    client.Reader
	lister    client.Reader
	gate      *gate.Gate
	incidents IncidentOpener
	locks     *Locks
	username  string
	preflight *Preflight

	rateLimit   int
	rateWindow  time.Duration
	rejectedTTL time.Duration
	lockTimeout time.Duration
	now         func() time.Time
	log         logr.Logger

	// perProposal serialises Process for one proposal.
	perProposal *keyedLocks

	// claimed holds the proposals the planner is converting itself, in-process.
	claimed sync.Map // client.ObjectKey -> struct{}
}

// New builds a Processor.
func New(cfg Config) *Processor {
	p := &Processor{
		client:      cfg.Client,
		reader:      cfg.Reader,
		lister:      cfg.Lister,
		gate:        cfg.Gate,
		incidents:   cfg.Incidents,
		locks:       cfg.Locks,
		username:    cfg.OperatorUsername,
		preflight:   cfg.Preflight,
		rateLimit:   cfg.RateLimit,
		rateWindow:  cfg.RateWindow,
		rejectedTTL: cfg.RejectedTTL,
		lockTimeout: cfg.LockTimeout,
		perProposal: newKeyedLocks(),
		now:         cfg.Now,
		log:         cfg.Logger.WithName("intake"),
	}
	if p.reader == nil {
		p.reader = cfg.Client
	}
	if p.lister == nil {
		p.lister = p.reader
	}
	if p.locks == nil {
		p.locks = NewLocks()
	}
	if p.preflight == nil {
		p.preflight = NewPreflight(cfg.Logger)
	}
	if p.rateLimit <= 0 {
		p.rateLimit = DefaultRateLimit
	}
	if p.rateWindow <= 0 {
		p.rateWindow = DefaultRateWindow
	}
	if p.rejectedTTL <= 0 {
		p.rejectedTTL = DefaultRejectedTTL
	}
	if p.lockTimeout <= 0 {
		p.lockTimeout = DefaultLockTimeout
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p
}

// OperatorUsername is the username Dorgu's planner proposes as. It is empty when
// the operator does not know it, in which case Blocked is not nil.
func (p *Processor) OperatorUsername() string { return p.username }

// Blocked says why the intake is closed, or nil when it is open. While it is closed
// Process converts nothing and the planner must not plan: the operator's identity is
// unknown, or a CRD it writes is missing or stale (see Preflight).
func (p *Processor) Blocked() *Failure {
	if p.username == "" {
		return &Failure{
			Check: "operator-identity", Reason: ReasonOperatorIdentityUnknown,
			Message: "the operator does not know its own ServiceAccount username",
		}
	}
	return p.preflight.Blocked()
}

// Locks returns the per-persona locks, for the planner to share.
func (p *Processor) Locks() *Locks { return p.locks }

// RejectedTTL is how long a Rejected proposal lives.
func (p *Processor) RejectedTTL() time.Duration { return p.rejectedTTL }

// Now is the processor's clock.
func (p *Processor) Now() time.Time { return p.now() }

// Hint is what Dorgu's own planner knows about a proposal it has just written,
// handed over in process because none of it belongs on the proposal's spec. It
// exists so a planner-written action is exactly what the planner shaped. The
// Processor still runs the gate over the proposal's own steps and trusts nothing
// in the Hint for the decision: a Hint that does not match the proposal (see
// hintMatches) is ignored, and status.gate always records the Processor's own gate
// run, never the planner's.
type Hint struct {
	// ProposalUID is the UID the API server gave the proposal the planner created.
	// The hint applies to that object only, so a proposal deleted and recreated
	// under the same name does not inherit the planner's confidence and plan.
	ProposalUID types.UID

	// Draft is the action the planner shaped. Its spec carries what a bare proposal
	// cannot: confidence, plan source, workload record and so on.
	Draft *dorguv1.RemediationAction

	// Facts are the diagnosis-derived inputs the planner gave the gate.
	Facts gate.Facts

	// PersonaLocked says the caller holds the lock for the proposal's persona (the
	// planner does, for its whole proposal). The intake then does not take it, so
	// it can never wait on a lock its own caller holds. It is a statement about the
	// caller, not about the hint: it applies even when the hint does not match.
	PersonaLocked bool
}
