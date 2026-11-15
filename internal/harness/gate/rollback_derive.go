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

package gate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"encoding/json"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/workload"
)

// Ticketed rollback (P2-D10). A person asks for a rollback with a RemediationProposal
// that names an action (spec.rollbackOf). The proposal carries no steps. The operator
// derives them here, from two things it wrote itself:
//
//   - status.gate.prePatchState, the live values the gate measured at approval, at the
//     paths the plan touches; and
//   - status.applyTicket.patch, the fields and values the ticket wrote.
//
// A rollback can therefore restore only a field Dorgu changed, and only to the value
// Dorgu recorded before it changed it. Nothing is read from the action's spec (whose
// prePatchState is whoever wrote the plan's say-so) or from the proposal.
//
// The derived plan is then judged like any other: the gate runs over it on live state,
// a person approves it, and an apply ticket is issued for it.

// Reasons a rollback cannot be derived. They are the Reason of the Intake rejection
// on the proposal; the api/v1alpha1 constants of the same names are pinned to these by
// a test.
const (
	// RollbackReasonNotApplied: the action never applied a change (no ticket), or has
	// not finished applying it, or ended without applying it.
	RollbackReasonNotApplied = "RollbackNotApplied"

	// RollbackReasonOfRollback: the action is itself a rollback.
	RollbackReasonOfRollback = "RollbackOfRollback"

	// RollbackReasonNoRecordedState: the operator did not record what is needed to
	// restore the change exactly.
	RollbackReasonNoRecordedState = "RollbackNoRecordedState"

	// RollbackReasonWorkloadDrifted: the live workload or the persona no longer holds
	// what the change left there.
	RollbackReasonWorkloadDrifted = "RollbackWorkloadDrifted"

	// RollbackReasonNothingToRestore: every changed field already holds its baseline.
	RollbackReasonNothingToRestore = "RollbackNothingToRestore"
)

// RollbackProblem is why a rollback cannot be derived from an action. Message is a
// sentence for people, written by the operator from its own records.
type RollbackProblem struct {
	Reason  string
	Message string
}

func (p *RollbackProblem) Error() string { return p.Reason + ": " + p.Message }

func rollbackRefusal(reason, format string, args ...any) *RollbackProblem {
	return &RollbackProblem{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// RollbackDerivation is the plan derived for a rollback.
type RollbackDerivation struct {
	// Steps is the plan: one auto-executable persona-update step that puts every field
	// still holding the change back to the value recorded before it.
	Steps []dorguv1.RemediationStep

	// Restored are the persona-spec paths the plan restores, sorted.
	Restored []string
}

// DeriveRollback derives the rollback of an action from the operator's own record of
// it, or says why it cannot (a *RollbackProblem). A non-nil error means the cluster
// could not be read, so nothing was decided and the caller should retry.
//
// A rollback is refused unless all of these hold:
//
//   - the action applied a change: it ended Completed or RolledBack and holds an apply
//     ticket (a RolledBack action restored the persona and left the Deployment as the
//     change set it, which is what a ticketed rollback finishes);
//   - it is not a rollback itself (re-applying a change is a new proposal);
//   - the operator recorded a baseline for every field the ticket changed;
//   - the workload has not drifted: the Deployment is the one the ticket was measured
//     against and its container is the ticket's, every changed field on it holds the
//     value the ticket wrote (or already its baseline), and the persona's value is the
//     one the change left or the baseline. A value that is neither was set by someone
//     else after the change and is theirs, so a restore would overwrite it;
//   - something is left to restore.
func (g *Gate) DeriveRollback(ctx context.Context, action *dorguv1.RemediationAction) (*RollbackDerivation, *RollbackProblem, error) {
	if ineligible := rollbackEligibility(action); ineligible != nil {
		return nil, ineligible, nil
	}
	baseline, noBaseline := RollbackBaseline(action)
	if noBaseline != nil {
		return nil, rollbackRefusal(RollbackReasonNoRecordedState, "%s", noBaseline.Reason), nil
	}
	fields := RolledBackFields(action)
	if len(fields) == 0 {
		return nil, rollbackRefusal(RollbackReasonNoRecordedState,
			"the apply ticket changed no container resource field, so there is nothing recorded to restore"), nil
	}
	before := PatchLeafValues(baseline.Raw)
	wrote := PatchLeafValues(action.Status.ApplyTicket.Patch.Raw)
	for _, path := range fields {
		if _, ok := before[path]; !ok {
			return nil, rollbackRefusal(RollbackReasonNoRecordedState,
				"the operator recorded no value of %s from before the change, and a rollback restores only what Dorgu recorded", path), nil
		}
	}

	live, problem, err := g.liveForRollback(ctx, action)
	if problem != nil || err != nil {
		return nil, problem, err
	}
	current, problem, err := g.personaForRollback(ctx, action)
	if problem != nil || err != nil {
		return nil, problem, err
	}

	var restore []string
	for _, path := range fields {
		liveValue, _ := ObservedValue(live, path)
		switch {
		case sameQuantity(liveValue, before[path]):
			// Already put back: nothing to do for this field.
		case sameQuantity(liveValue, wrote[path]):
			if !sameQuantity(current[path], wrote[path]) && !sameQuantity(current[path], before[path]) {
				return nil, rollbackRefusal(RollbackReasonWorkloadDrifted,
					"the persona's %s is %s, neither what the change set (%s) nor what it was before (%s): someone edited it since, and a rollback would overwrite that edit",
					path, shown(current[path]), wrote[path], before[path]), nil
			}
			restore = append(restore, path)
		default:
			return nil, rollbackRefusal(RollbackReasonWorkloadDrifted,
				"%s on the live workload is %s, neither what the change set (%s) nor what it was before (%s): someone changed it since, and a rollback would overwrite that change",
				path, shown(liveValue), wrote[path], before[path]), nil
		}
	}
	if len(restore) == 0 {
		return nil, rollbackRefusal(RollbackReasonNothingToRestore,
			"the live workload already holds the values recorded before the change, so there is nothing to restore"), nil
	}
	slices.Sort(restore)

	patch, err := restoringPatch(restore, before)
	if err != nil {
		return nil, nil, err
	}
	return &RollbackDerivation{
		Steps: []dorguv1.RemediationStep{{
			Order: 1, ID: "rollback-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
			Description: fmt.Sprintf("Restore %s to the value Dorgu recorded before action %s changed it",
				strings.Join(restore, ", "), action.Name),
			Rationale: "Derived by the operator from the baseline it recorded at approval (status.gate.prePatchState), " +
				"for the fields its apply ticket changed. Nothing in this step came from the requester.",
			Patch: patch,
		}},
		Restored: restore,
	}, nil, nil
}

// rollbackEligibility is the part of the decision that needs only the action.
func rollbackEligibility(action *dorguv1.RemediationAction) *RollbackProblem {
	if action.Status.Phase != "Completed" && action.Status.Phase != phaseRolledBack {
		return rollbackRefusal(RollbackReasonNotApplied,
			"the action is %s: only an action that ended Completed or RolledBack applied a change there is to roll back", phaseOrNone(action.Status.Phase))
	}
	if action.Status.ApplyTicket == nil {
		return rollbackRefusal(RollbackReasonNotApplied, "the action holds no apply ticket, so it never applied anything")
	}
	if p := action.Spec.Provenance; p != nil && p.Source == dorguv1.ProposalSourceRollback {
		return rollbackRefusal(RollbackReasonOfRollback,
			"the action is itself a rollback: rolling it back would re-apply the change it undid, which is a new proposal to make")
	}
	return nil
}

func phaseOrNone(phase string) string {
	if phase == "" {
		return "in no phase yet"
	}
	return phase
}

func shown(value string) string {
	if value == "" {
		return "not set"
	}
	return value
}

// liveForRollback reads the Deployment the ticket was measured against, as a workload
// ref whose observed resources are the live container's. It refuses when the
// Deployment is gone, ambiguous, replaced or no longer has the ticket's container.
func (g *Gate) liveForRollback(ctx context.Context, action *dorguv1.RemediationAction) (*dorguv1.WorkloadRef, *RollbackProblem, error) {
	ticket := action.Status.ApplyTicket
	key := ticketKey(action)
	if key.Name == "" || key.Namespace == "" {
		return nil, rollbackRefusal(RollbackReasonWorkloadDrifted, "the action names no persona, so its workload cannot be found"), nil
	}
	obs, err := workload.Observe(ctx, g.reader, key.Namespace, key.Name)
	var ambiguous *workload.AmbiguousError
	switch {
	case errors.As(err, &ambiguous):
		return nil, rollbackRefusal(RollbackReasonWorkloadDrifted,
			"more than one Deployment now answers to persona %s/%s, so the workload the change was applied to cannot be told apart", key.Namespace, key.Name), nil
	case err != nil:
		return nil, nil, fmt.Errorf("reading the live workload for %s/%s: %w", key.Namespace, key.Name, err)
	case obs == nil || obs.Deployment == nil:
		return nil, rollbackRefusal(RollbackReasonWorkloadDrifted,
			"no Deployment answers to persona %s/%s any more", key.Namespace, key.Name), nil
	case string(obs.Deployment.UID) != ticket.WorkloadUID:
		return nil, rollbackRefusal(RollbackReasonWorkloadDrifted,
			"Deployment %s/%s is not the one the change was applied to (its UID changed)", obs.Deployment.Namespace, obs.Deployment.Name), nil
	}
	ref := obs.Ref(metav1.NewTime(g.now()))
	if ref.Container != ticket.Container {
		return nil, rollbackRefusal(RollbackReasonWorkloadDrifted,
			"the container the change was applied to (%q) is no longer the one the persona resolves to (%q)", ticket.Container, ref.Container), nil
	}
	return ref, nil, nil
}

// personaForRollback reads the persona's current spec as persona-spec leaf values.
func (g *Gate) personaForRollback(ctx context.Context, action *dorguv1.RemediationAction) (map[string]string, *RollbackProblem, error) {
	key := ticketKey(action)
	var persona dorguv1.ApplicationPersona
	switch err := g.reader.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, &persona); {
	case apierrors.IsNotFound(err):
		return nil, rollbackRefusal(RollbackReasonWorkloadDrifted, "ApplicationPersona %s/%s does not exist", key.Namespace, key.Name), nil
	case err != nil:
		return nil, nil, fmt.Errorf("reading ApplicationPersona %s/%s: %w", key.Namespace, key.Name, err)
	}
	raw, err := json.Marshal(map[string]any{"spec": persona.Spec})
	if err != nil {
		return nil, nil, fmt.Errorf("reading the persona as JSON: %w", err)
	}
	return PatchLeafValues(raw), nil, nil
}

// restoringPatch is the merge patch that sets each path to its recorded baseline.
func restoringPatch(paths []string, before map[string]string) (*apiextensionsv1.JSON, error) {
	root := make(map[string]any)
	for _, path := range paths {
		mergeNestedPath(root, strings.Split(path, "."), before[path])
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("building the restoring patch: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}
