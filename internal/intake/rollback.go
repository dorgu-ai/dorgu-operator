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
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/safetext"
)

// Ticketed rollback (P2-D10), the intake's half.
//
// A rollback proposal (spec.rollbackOf) is a person's request about one action. The
// operator does not read its steps (the CRD refuses any, and this code would ignore
// them): it derives the plan from what it recorded when it applied the action
// (gate.DeriveRollback), makes the RemediationAction as itself, and records the
// person in provenance.requestedBy. From there it is an ordinary action: the gate rules
// on live state, a person approves it (the same person may, because the proposer is the
// operator), and approval is turned into an apply ticket.

// systemUserPrefix starts the username of every service account and every component
// identity. None of them is a person.
const systemUserPrefix = "system:"

// rollbackIsOpenPhase reports whether a rollback action in this phase still stands in
// the way of another rollback of the same action. A finished one does not: Rejected,
// Expired and Failed changed nothing, and Completed, Acknowledged or RolledBack leave
// the workload for the next request to judge (it is refused as nothing to restore when
// the rollback worked).
func rollbackIsOpenPhase(phase string) bool {
	return !slices.Contains([]string{"Completed", "Acknowledged", "RolledBack", "Failed", "Rejected", "Expired"}, phase)
}

// rollbackPersonaMatches reports whether the proposal's target is the persona the
// action changed.
func rollbackPersonaMatches(prop *v1alpha1.RemediationProposal, ra *dorguv1.RemediationAction) bool {
	have := ra.Spec.PersonaRef
	if have.Namespace == "" {
		have.Namespace = ra.Namespace
	}
	return have == personaRefOf(prop)
}

// planRollback decides a rollback proposal as far as intake can: it finds the action,
// refuses what cannot be rolled back, and derives the steps. A nil outcome with a nil
// error and a derivation means the proposal goes on to the gate. The caller holds the
// persona's lock, so two requests for one action cannot both see "nothing open".
func (p *Processor) planRollback(
	ctx context.Context, prop *v1alpha1.RemediationProposal,
) (*Outcome, *gate.RollbackDerivation, error) {
	name := prop.Spec.RollbackOf.Name

	var ra dorguv1.RemediationAction
	switch err := p.reader.Get(ctx, client.ObjectKey{Namespace: prop.Namespace, Name: name}, &ra); {
	case apierrors.IsNotFound(err):
		out, rejectErr := p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonRollbackTargetNotFound, nil,
			fmt.Sprintf("action %s/%s does not exist, so there is nothing to roll back", prop.Namespace, shortened(name)))
		return out, nil, rejectErr
	case err != nil:
		return nil, nil, fmt.Errorf("reading the action to roll back: %w", err)
	}

	if !rollbackPersonaMatches(prop, &ra) {
		out, err := p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonRollbackTargetMismatch, nil,
			fmt.Sprintf("action %s changed persona %s, not the proposal's target %s: a rollback restores the persona its action changed",
				ra.Name, ra.Spec.PersonaRef.Name, prop.Spec.Target.Name))
		return out, nil, err
	}

	open, err := p.openRollbackOf(ctx, prop, &ra)
	if err != nil {
		return nil, nil, err
	}
	if open != "" {
		out, rejectErr := p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonRollbackAlreadyOpen, nil,
			fmt.Sprintf("a rollback of %s is already open (%s): approve or reject that one first", ra.Name, open))
		return out, nil, rejectErr
	}

	derived, problem, err := p.gate.DeriveRollback(ctx, &ra)
	if err != nil {
		return nil, nil, fmt.Errorf("deriving the rollback: %w", err)
	}
	if problem != nil {
		out, rejectErr := p.reject(ctx, prop, nil, v1alpha1.StageIntake, problem.Reason, nil, problem.Message)
		return out, nil, rejectErr
	}
	return nil, derived, nil
}

// openRollbackOf names a rollback action of ra that is still in flight, or "". The
// action an earlier attempt of this very proposal made is not "another" rollback.
// The list is the uncached reader's: a stale answer would let two rollbacks through.
func (p *Processor) openRollbackOf(
	ctx context.Context, prop *v1alpha1.RemediationProposal, ra *dorguv1.RemediationAction,
) (string, error) {
	var list dorguv1.RemediationActionList
	if err := p.reader.List(ctx, &list, client.InNamespace(prop.Namespace)); err != nil {
		return "", fmt.Errorf("listing RemediationActions to look for an open rollback: %w", err)
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Annotations[dorguv1.AnnotationRollbackOf] != ra.Name ||
			other.Annotations[AnnotationProposalUID] == string(prop.UID) ||
			!rollbackIsOpenPhase(other.Status.Phase) {
			continue
		}
		return other.Name, nil
	}
	return "", nil
}

// rollbackDraft is the action a rollback proposal becomes, before the gate rules on it.
// Everything in it is the operator's: the plan is derived, the explanation names the
// action and the person, and the requester's reason is appended as untrusted text.
func rollbackDraft(
	prop *v1alpha1.RemediationProposal, incident *dorguv1.IncidentMemory, derived *gate.RollbackDerivation,
) *dorguv1.RemediationAction {
	shaped := prop.DeepCopy()
	shaped.Spec.Steps = derived.Steps
	draft := derivedAction(shaped, incident)
	draft.Annotations[dorguv1.AnnotationRollbackOf] = prop.Spec.RollbackOf.Name
	draft.Spec.Explanation = fmt.Sprintf("Roll back action %s, as requested by %s: restore %s to the values Dorgu recorded before it changed them. Reason given: %s",
		prop.Spec.RollbackOf.Name, safetext.Clean(prop.Spec.Proposer.Username, false),
		strings.Join(derived.Restored, ", "), safetext.Clean(prop.Spec.Rationale, true))
	return draft
}

// rollbackProvenance is the record on a rollback action. The operator wrote the plan,
// so it is the proposer, and an approver other than the requester is not needed (the
// same person may approve). The person who asked is requestedBy. The proposal's
// username was verified against the authenticated user by the admission policy; the
// operator records it as it is (intakeChecks has refused one that needs cleaning).
func rollbackProvenance(prop *v1alpha1.RemediationProposal, operatorUsername string) *dorguv1.RemediationProvenance {
	return &dorguv1.RemediationProvenance{
		Proposal:    prop.Name,
		Proposer:    dorguv1.ProposerIdentity{Username: operatorUsername},
		Source:      dorguv1.ProposalSourceRollback,
		RequestedBy: prop.Spec.Proposer.Username,
	}
}
