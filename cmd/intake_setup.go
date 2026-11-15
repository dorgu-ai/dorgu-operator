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

package main

import (
	"sync"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

// intakeSetup is what the proposal intake needs worked out before it is built: the
// operator's own username, and the preflight that keeps the intake closed while
// that username, or a CRD the operator writes, is missing or stale. It is built
// once, early, because the ClusterPersona reconciler reports its state too.
type intakeSetup struct {
	// Username is the operator's ServiceAccount username, or "" when it cannot be
	// worked out. It is never defaulted.
	Username string

	// Preflight says whether the intake is open.
	Preflight *intake.Preflight
}

// newIntakeSetup works out the operator's identity and builds the preflight with
// the checks that exist today: identity, the RemediationProposal CRD, and
// spec.provenance in the RemediationAction CRD. C2 adds its admission-policy check
// with setup.Preflight.Add. reader must be uncached.
func newIntakeSetup(reader client.Reader, operatorNS string, log logr.Logger) intakeSetup {
	username, err := intake.OperatorUsername(operatorNS)
	if err != nil {
		log.Error(err, "the proposal intake is closed: Dorgu will not propose or plan until the operator knows who it is")
	}
	return intakeSetup{
		Username: username,
		Preflight: intake.NewPreflight(log,
			intake.OperatorIdentityCheck(err),
			intake.ProposalCRDCheck(reader),
			intake.ActionProvenanceCheck(reader),
		),
	}
}

// verifyIdentity adds the check that the API server agrees about who the operator
// is. Without a username there is nothing to check: the identity check already
// fails.
func (s intakeSetup) verifyIdentity(whoAmI intake.WhoAmI) {
	if s.Username == "" || whoAmI == nil {
		return
	}
	s.Preflight.Add(intake.IdentityVerifiedCheck(s.Username, whoAmI))
}

// lateRegistration runs register once the intake's CRD preflight first passes, then
// never again. The proposal controller cannot be registered while the
// RemediationProposal CRD is absent (its cache would never sync and the manager would
// exit), so the operator starts without it, reports why, and registers it here when
// the CRD appears, without a restart. A failed registration is tried again on the
// next run.
type lateRegistration struct {
	register func() error
	log      logr.Logger

	mu   sync.Mutex
	done bool
}

// onChecks is a Preflight subscriber.
func (l *lateRegistration) onChecks(failures []intake.Failure) {
	for _, f := range failures {
		if f.Reason == intake.ReasonProposalCRDMissing {
			return
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	if err := l.register(); err != nil {
		l.log.Error(err, "could not register the proposal controller; will try again")
		return
	}
	l.done = true
	l.log.Info("the RemediationProposal CRD is installed: the proposal controller is registered")
}
