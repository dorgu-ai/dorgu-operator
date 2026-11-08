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
	return nil
}
