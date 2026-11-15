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

package remediation

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

// applyOwnershipShaping makes the plan match whoever owns the Deployment.
//
// The split this enforces, and the one that is easiest to get wrong:
//
//   - persona-update steps patch the ApplicationPersona. The operator does that
//     itself, it is always safe, and ownership has no say in it. Their
//     autoExecutable semantics are untouched here.
//   - a command that patches the Deployment is run by the CLI or by the reader
//     with their own credentials, and that is the write that breaks the owner's
//     next apply. Ownership governs only that.
//
// For an unmanaged workload the direct kubectl command is the right answer and
// is left exactly as the planner wrote it. For every other value, including
// unknown, workload-writing commands are removed and each affected step is
// rewritten to say what to change in the owner's own source of truth.
func applyOwnershipShaping(action *dorguv1.RemediationAction, ref *dorguv1.WorkloadRef) {
	if action == nil || !ref.IsOwned() {
		return
	}

	changes := proposedResourceChanges(action)
	reason := whyDorguWillNotPatch(ref)
	shaped := false

	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		// persona-update steps are the operator's own write. Never reshaped.
		if step.Type == dorguv1.StepTypePersonaUpdate {
			continue
		}
		if !gate.WritesWorkload(step.Command) {
			continue
		}
		step.Command = ""
		step.Description = ownerInstruction(ref, changes, step.Description)
		step.Rationale = gate.AppendNote(step.Rationale, reason)
		shaped = true
	}

	if shaped {
		action.Spec.PlanSummary = gate.AppendNote(action.Spec.PlanSummary, reason)
	}
}

// resourceChange is one concrete field the plan moves, in the form the reader
// has to reproduce in their own source of truth.
type resourceChange struct {
	// Key is the values-file style path, e.g. "resources.limits.memory".
	Key string
	// Value is the proposed value.
	Value string
}

// proposedResourceChanges reads the concrete field changes out of the plan's
// persona-update patches, so the owner-shaped instruction can name real keys
// and values rather than paraphrasing the model's prose.
func proposedResourceChanges(action *dorguv1.RemediationAction) []resourceChange {
	seen := make(map[string]string)

	collect := func(raw []byte) {
		if len(raw) == 0 {
			return
		}
		for path, value := range gate.PatchLeafValues(raw) {
			if !gate.IsResourcePath(path) {
				continue
			}
			seen[strings.TrimPrefix(path, "spec.")] = value
		}
	}

	for i := range action.Spec.Steps {
		step := &action.Spec.Steps[i]
		if step.Type == dorguv1.StepTypePersonaUpdate && step.Patch != nil {
			collect(step.Patch.Raw)
		}
	}
	if action.Spec.Action.Patch != nil {
		collect(action.Spec.Action.Patch.Raw)
	}

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]resourceChange, 0, len(keys))
	for _, k := range keys {
		out = append(out, resourceChange{Key: k, Value: seen[k]})
	}
	return out
}

// ownerInstruction renders the step a reader can actually carry out: the change
// to make, where to make it, and how it reaches the cluster.
//
// fallback carries the planner's own description of the change, used when the
// plan has no structured resource change to name (a bad image tag, say).
func ownerInstruction(ref *dorguv1.WorkloadRef, changes []resourceChange, fallback string) string {
	what := describeChanges(changes)
	if what == "" {
		what = strings.TrimRight(strings.TrimSpace(fallback), ".")
		if what == "" {
			what = "Apply this change"
		}
	}

	where, how := ownerSourceOfTruth(ref)
	if how == "" {
		return fmt.Sprintf("%s in %s.", what, where)
	}
	return fmt.Sprintf("%s in %s, then %s.", what, where, how)
}

// describeChanges renders the concrete field changes as an instruction, or ""
// when the plan carries none.
func describeChanges(changes []resourceChange) string {
	if len(changes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, fmt.Sprintf("%s: %s", c.Key, c.Value))
	}
	return "Set " + gate.HumanFieldList(parts)
}

// ownerSourceOfTruth names where an owned workload's desired state actually
// lives, and how a change there reaches the cluster.
//
// The wording hedges where Dorgu genuinely does not know: chart values keys are
// chart-specific and Dorgu has not read the chart, so it says "commonly" rather
// than asserting a path it cannot verify.
func ownerSourceOfTruth(ref *dorguv1.WorkloadRef) (where, how string) {
	owner := ref.ManagedByDetail
	switch ref.ManagedBy {
	case dorguv1.ManagedByHelm:
		if owner == "" {
			owner = "the Helm release that owns this Deployment"
		}
		return fmt.Sprintf("the values for %s (the key is chart-specific, commonly under `resources`)", owner),
			"run your usual `helm upgrade` for that release"

	case dorguv1.ManagedByArgoCD:
		if owner == "" {
			owner = "the ArgoCD application that owns this Deployment"
		}
		return fmt.Sprintf("the Git manifests for %s", owner),
			"commit and let ArgoCD sync"

	case dorguv1.ManagedByFlux:
		if owner == "" {
			owner = "the Flux resource that owns this Deployment"
		}
		return fmt.Sprintf("the Git source reconciled by %s", owner),
			"commit and let Flux reconcile it"

	case dorguv1.ManagedByKustomize:
		return "your kustomize overlay for this Deployment", "re-apply the overlay"

	default:
		if !gate.Resolved(ref) {
			return "whatever manages this application (Dorgu could not resolve its Deployment)", ""
		}
		// An unknown owner is often still a named one: detection records the
		// field manager it refused on behalf of even when it cannot say what
		// that manager belongs to. Passing the name on is the difference
		// between a lead and a dead end.
		if detail := strings.TrimSpace(ref.ManagedByDetail); detail != "" {
			return fmt.Sprintf("whatever manages Deployment %s (%s)", ref.Name, detail), ""
		}
		return fmt.Sprintf("whatever manages Deployment %s (Dorgu could not identify it)", ref.Name), ""
	}
}

// whyDorguWillNotPatch is the one-line explanation printed beside a reshaped
// step. It is written to read as competence rather than breakage: the reader
// should finish it understanding what would have gone wrong.
func whyDorguWillNotPatch(ref *dorguv1.WorkloadRef) string {
	owner := ref.ManagedByDetail
	switch ref.ManagedBy {
	case dorguv1.ManagedByHelm:
		if owner == "" {
			owner = "a Helm release"
		}
		return fmt.Sprintf(
			"Dorgu will not patch this Deployment because %s owns it: a direct patch claims the fields it sets, and the next `helm upgrade` then fails with a field-manager conflict.",
			owner)

	case dorguv1.ManagedByArgoCD:
		if owner == "" {
			owner = "an ArgoCD application"
		}
		return fmt.Sprintf(
			"Dorgu will not patch this Deployment because %s owns it: a direct patch is reverted on the next sync, or fails outright under server-side apply.",
			owner)

	case dorguv1.ManagedByFlux:
		if owner == "" {
			owner = "a Flux controller"
		}
		return fmt.Sprintf(
			"Dorgu will not patch this Deployment because %s reconciles it: a direct patch is reverted on the next reconciliation.",
			owner)

	case dorguv1.ManagedByKustomize:
		return "Dorgu will not patch this Deployment because a kustomize overlay owns it: a direct patch is overwritten the next time the overlay is applied."

	default:
		if !gate.Resolved(ref) {
			return "Dorgu could not resolve the Deployment for this application, so it cannot tell what owns it and will not suggest a command that writes to it. Unresolved is treated as owned."
		}
		if detail := strings.TrimSpace(ref.ManagedByDetail); detail != "" {
			return fmt.Sprintf(
				"Dorgu will not patch this Deployment. What it can see: %s. It cannot tell what that manager belongs to, and unknown is treated as owned, because a patch that collides with an unseen owner breaks their next deploy.",
				detail)
		}
		return "Dorgu could not identify what manages this Deployment, so it will not patch it. Unknown is treated as owned, because a patch that collides with an unseen owner breaks their next deploy."
	}
}

// adviseForOwner turns a plan for a workload that Helm, Argo CD, Flux or kustomize owns
// into advice, and reports how many persona-update steps it converted (B10 b, D1.1 #5).
//
// applyOwnershipShaping above leaves persona-update steps alone, on the reasoning that the
// operator writing its own ApplicationPersona is always safe. It is safe, and it was
// still wrong for these workloads: the plan read "1 applied on approval", approving it
// patched the persona, the Deployment kept its old values, and the deployer's source of
// truth was never asked. An owned workload's change belongs in the owner's source, so
// every step here is advice and none is auto-executable.
//
// Each persona-update step becomes a config-change step that says what to set and where,
// in the same words the owner-shaped workload step already uses. A conversion whose
// words equal a step the plan already has is dropped rather than repeated. The patch and
// its pre-patch state go with the step: nothing in the plan can be read as a change to
// apply, by this operator or by a client that reads steps[].
//
// Unknown ownership and an unresolved workload are not touched: with no evidence of a
// deployer the persona proposal stands as before. It must run before the gate rules on
// the plan as final, which refuses an owned workload's plan that still has an auto step.
func adviseForOwner(action *dorguv1.RemediationAction, ref *dorguv1.WorkloadRef) int {
	if action == nil || !gate.DeployerOwned(ref) {
		return 0
	}

	reason := whyDorguWillNotPatch(ref)

	seen := make(map[string]bool, len(action.Spec.Steps))
	for i := range action.Spec.Steps {
		if action.Spec.Steps[i].Type != dorguv1.StepTypePersonaUpdate {
			seen[action.Spec.Steps[i].Description] = true
		}
	}

	kept := make([]dorguv1.RemediationStep, 0, len(action.Spec.Steps))
	converted := 0
	for i := range action.Spec.Steps {
		step := action.Spec.Steps[i].DeepCopy()
		if step.Type != dorguv1.StepTypePersonaUpdate {
			kept = append(kept, *step)
			continue
		}
		converted++

		own := changesOfPatch(step.Patch)
		if len(own) > 0 {
			step.Description = ownerInstruction(ref, own, step.Description)
		}
		step.Rationale = gate.AppendNote(step.Rationale, reason)
		step.Type = dorguv1.StepTypeConfigChange
		step.AutoExecutable = false
		step.Patch, step.PrePatchState, step.Command = nil, nil, ""
		if seen[step.Description] {
			continue
		}
		seen[step.Description] = true
		kept = append(kept, *step)
	}
	if converted == 0 {
		return 0
	}

	for i := range kept {
		kept[i].Order = int32(i + 1) //nolint:gosec // bounded by the plan's own step count
		kept[i].ID = fmt.Sprintf("step-%d", i+1)
	}
	action.Spec.Steps = kept
	action.Spec.Action = gate.DeriveAction(kept)
	action.Spec.PlanSummary = gate.AppendNote(action.Spec.PlanSummary, reason)
	return converted
}

// adviseAndSeal shapes a rule-based plan for a workload a deployer owns as advice and
// has the gate rule on it as final. It returns the sealing decision, or the refusal.
func (p *Proposer) adviseAndSeal(
	ctx context.Context,
	action *dorguv1.RemediationAction,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
	ref *dorguv1.WorkloadRef,
) (*gate.Decision, *ProposalResult) {
	if adviseForOwner(action, ref) > 0 {
		action.Spec.Explanation = gate.AppendNote(action.Spec.Explanation, ownerAdviceNote(ref))
	}
	sealed, refusal := p.seal(ctx, action, diag, incident, false)
	if refusal != nil {
		return nil, refusal
	}
	intake.InstallDecision(action, sealed)
	return sealed, nil
}

// changesOfPatch reads the resource changes one persona patch makes, as the keys and
// values a reader sets in their own source of truth.
func changesOfPatch(patch *apiextensionsv1.JSON) []resourceChange {
	if patch == nil || len(patch.Raw) == 0 {
		return nil
	}
	probe := &dorguv1.RemediationAction{Spec: dorguv1.RemediationActionSpec{
		Steps: []dorguv1.RemediationStep{{Type: dorguv1.StepTypePersonaUpdate, Patch: patch}},
	}}
	return proposedResourceChanges(probe)
}

// ownerAdviceNote is the sentence appended to an owned workload's explanation: who owns
// it, and that Dorgu changes neither the persona nor the Deployment.
func ownerAdviceNote(ref *dorguv1.WorkloadRef) string {
	owner := strings.TrimSpace(ref.ManagedByDetail)
	if owner == "" {
		switch ref.ManagedBy {
		case dorguv1.ManagedByHelm:
			owner = "a Helm release"
		case dorguv1.ManagedByArgoCD:
			owner = "an ArgoCD application"
		case dorguv1.ManagedByFlux:
			owner = "a Flux controller"
		default:
			owner = "a kustomize overlay"
		}
	}
	return fmt.Sprintf("%s owns this workload, so Dorgu will not change its persona or the Deployment: this is advice for you to apply in its source of truth.",
		strings.ToUpper(owner[:1])+owner[1:])
}
