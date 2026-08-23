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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// kubectl subcommands that write to the cluster. A plan for an owned workload
// may not hand the reader any of these against that workload: a direct write
// takes field ownership away from the owner, which is what makes the next
// `helm upgrade` fail outright rather than merely revert the fix (F-02).
// Anything absent (get, describe, logs, top, events, explain, diff, and
// `rollout status`/`history`) only reads, and stays useful on any workload.
var mutatingKubectlVerbs = map[string]bool{
	"annotate":  true,
	"apply":     true,
	"autoscale": true,
	"cordon":    true,
	"create":    true,
	"delete":    true,
	"drain":     true,
	"edit":      true,
	"expose":    true,
	"label":     true,
	"patch":     true,
	"replace":   true,
	"run":       true,
	"scale":     true,
	"set":       true,
	"taint":     true,
	"uncordon":  true,
}

// rolloutSubcommands that write. `kubectl rollout status` and `history` only
// read, so they stay useful on an owned workload.
var mutatingRolloutSubcommands = map[string]bool{
	"undo":    true,
	"restart": true,
	"pause":   true,
	"resume":  true,
}

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
		if !writesWorkload(step.Command) {
			continue
		}
		step.Command = ""
		step.Description = ownerInstruction(ref, changes, step.Description)
		step.Rationale = appendNote(step.Rationale, reason)
		shaped = true
	}

	if shaped {
		action.Spec.PlanSummary = appendNote(action.Spec.PlanSummary, reason)
	}
}

// stripWorkloadWriteCommands is the last gate before persistence: no step on an
// owned workload keeps a command that writes to the cluster, whatever path it
// arrived by. applyOwnershipShaping rewrites those steps properly; this exists
// so a future code path that forgets to call it still cannot emit a
// `kubectl patch` against a Helm release.
func stripWorkloadWriteCommands(action *dorguv1.RemediationAction, ref *dorguv1.WorkloadRef) {
	if action == nil || !ref.IsOwned() {
		return
	}
	for i := range action.Spec.Steps {
		if writesWorkload(action.Spec.Steps[i].Command) {
			action.Spec.Steps[i].Command = ""
		}
	}
}

// readOnlyKubectlVerbs are the kubectl subcommands that only read. They are
// listed explicitly so that a command whose verb matches nothing at all can be
// treated as a write: an unrecognised command against an owned workload is not
// something to hand over on the assumption it is harmless.
var readOnlyKubectlVerbs = map[string]bool{
	"api-resources": true,
	"api-versions":  true,
	"auth":          true,
	"cluster-info":  true,
	"describe":      true,
	"diff":          true,
	"events":        true,
	"explain":       true,
	"get":           true,
	"logs":          true,
	"top":           true,
	"version":       true,
}

// writesWorkload reports whether a suggested kubectl command changes cluster
// state. A non-kubectl or empty command is not a workload write: it is already
// dropped by SanitizeStepCommand before it reaches here.
//
// The verb is found by scanning for the first token that is a kubectl
// subcommand, rather than the first non-flag token, so a global flag with a
// value (`kubectl -n apps patch ...`) does not hide the verb behind its
// argument.
func writesWorkload(command string) bool {
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[0] != "kubectl" {
		return false
	}

	for i, arg := range fields[1:] {
		switch {
		case arg == "rollout":
			return rolloutWrites(fields[i+2:])
		case mutatingKubectlVerbs[arg]:
			return true
		case readOnlyKubectlVerbs[arg]:
			return false
		}
	}

	// No recognisable verb. Treat it as a write so an owned workload never
	// receives a command Dorgu could not classify.
	return true
}

// rolloutWrites classifies a `kubectl rollout` invocation from its remaining
// arguments. status and history only read; undo, restart, pause and resume do
// not.
func rolloutWrites(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return mutatingRolloutSubcommands[arg]
	}
	return false
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
		for path, value := range patchLeafValues(raw) {
			if !isResourcePath(path) {
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
	return "Set " + humanFieldList(parts)
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
		if !resolved(ref) {
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
		if !resolved(ref) {
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

// patchLeafValues walks a JSON merge patch and returns every leaf string value
// keyed by its dot-joined path. Non-string leaves are skipped: the resource
// fields this is used for are always strings.
func patchLeafValues(raw []byte) map[string]string {
	out := make(map[string]string)
	if len(raw) == 0 {
		return out
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return out
	}

	var walk func(prefix string, node map[string]any)
	walk = func(prefix string, node map[string]any) {
		for key, val := range node {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			switch typed := val.(type) {
			case map[string]any:
				walk(path, typed)
			case string:
				out[path] = typed
			}
		}
	}
	walk("", root)
	return out
}
