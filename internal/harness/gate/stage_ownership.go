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
	"fmt"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ownershipStage records who owns the live workload, as resolved from the
// cluster, and holds the one line the operator never crosses: it does not write
// to a workload someone else owns.
//
// The resolution itself is workload.DetectOwner, run during Ground; this stage
// stores the result on the decision and enforces what follows from it.
type ownershipStage struct{ g *Gate }

func (ownershipStage) Name() string { return StageOwnership }

func (ownershipStage) Run(_ context.Context, cand *Candidate, d *Decision) error {
	ref := d.WorkloadRef
	managedBy := dorguv1.ManagedByUnknown
	detail := ""
	if ref != nil {
		if ref.ManagedBy != "" {
			managedBy = ref.ManagedBy
		}
		detail = ref.ManagedByDetail
	}
	d.Ownership = &dorguv1.GateOwnership{
		ManagedBy: managedBy,
		Detail:    truncate(detail, 253),
		Resolved:  Resolved(ref),
	}

	owned := ref.IsOwned()
	if owned {
		d.pass(StageOwnership, GuardrailOwnership,
			fmt.Sprintf("workload is owned (%s): Dorgu recommends and does not write it", managedBy),
			map[string]string{"managedBy": managedBy})
	} else {
		d.pass(StageOwnership, GuardrailOwnership, "workload is unmanaged", map[string]string{"managedBy": managedBy})
	}

	if cand.Final && owned {
		for i := range d.Steps {
			step := &d.Steps[i]
			if WritesWorkload(step.Command) {
				d.refuse(StageOwnership, GuardrailOwnerCommands,
					fmt.Sprintf("step %d still carries a command that writes to a workload %s owns", step.Order, managedBy),
					map[string]string{"step": fmt.Sprint(step.Order), "managedBy": managedBy})
				return nil
			}
		}
	}

	if cand.Final && DeployerOwned(ref) && hasAutoApplicable(d) {
		deployerOwned(d, managedBy)
	}
	return nil
}

// deployerOwned enforces B10 (b): a workload that Helm, Argo CD, Flux or kustomize owns
// gets advice, never an applied change, and that includes the persona-update step. The
// persona is Dorgu's own record, but patching it on approval made the plan look half
// applied, and a deployer's next sync leaves the two disagreeing (D1.1 finding 5).
//
// At proposal the proposer has already shaped such a plan as advice, so a step that is
// still auto-executable is a plan that was changed after that and is refused. At
// approval it is an action proposed by an earlier operator (0.13.0 proposed one): the
// steps are taken out of the decision, which is then allowed with nothing to apply, and
// the caller acknowledges the approval. The plan on the object is not rewritten.
func deployerOwned(d *Decision, managedBy string) {
	numbers := map[string]string{"managedBy": managedBy}
	if d.Phase == PhaseProposal {
		d.refuse(StageOwnership, GuardrailOwnerAutoStep,
			fmt.Sprintf("the plan still carries a step the operator would apply, on a workload %s owns: it is advice there, never an applied change", managedBy),
			numbers)
		return
	}
	for i := range d.Steps {
		step := &d.Steps[i]
		if step.Type != dorguv1.StepTypePersonaUpdate && !step.AutoExecutable {
			continue
		}
		step.AutoExecutable = false
		step.Type = dorguv1.StepTypeConfigChange
		step.Patch, step.PrePatchState = nil, nil
	}
	d.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
	d.ownerAdvisory = true
	d.pass(StageOwnership, GuardrailOwnerAdvisory,
		fmt.Sprintf("workload is owned (%s): approving this plan records the decision and applies nothing, not the persona and not the workload", managedBy),
		numbers)
}

// DeployerOwned reports whether a declarative deployer reconciles the workload: Helm,
// Argo CD, Flux or kustomize, as workload.DetectOwner resolved it. Unknown and an
// unresolved workload are owned for the commands Dorgu will not suggest (WorkloadRef.IsOwned),
// but not here: with no evidence of a deployer, the persona proposal stands as it did.
func DeployerOwned(ref *dorguv1.WorkloadRef) bool {
	if ref == nil {
		return false
	}
	switch ref.ManagedBy {
	case dorguv1.ManagedByHelm, dorguv1.ManagedByArgoCD, dorguv1.ManagedByFlux, dorguv1.ManagedByKustomize:
		return true
	}
	return false
}
