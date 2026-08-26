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
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/workload"
)

// Persona-spec resource paths a remediation may target, and the live container
// field each one lands on once the CLI applies the persona to the Deployment.
const (
	pathPrefixLimits   = "spec.resources.limits."
	pathPrefixRequests = "spec.resources.requests."

	resourceKeyCPU    = "cpu"
	resourceKeyMemory = "memory"
)

// observeWorkload reads the live Deployment the diagnosis concerns.
//
// It never fails a proposal: a cluster we cannot read is reported as an
// unresolved workload (ManagedBy unknown, which every consumer treats as
// owned), so the worst case is that Dorgu explains instead of writing. The
// alternative, proceeding as though the persona were current, is the bug this
// whole change exists to remove.
func (p *Proposer) observeWorkload(ctx context.Context, ref *dorguv1.PersonaReference) (*workload.Observation, *dorguv1.WorkloadRef) {
	now := metav1.Now()
	if ref == nil {
		return nil, workload.UnresolvedRef(now)
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	obs, err := workload.Observe(ctx, p.client, namespace, ref.Name)
	if err != nil {
		p.logger.V(0).Info("could not read the live workload; grounding falls back to the persona and the workload is treated as owned",
			"persona", ref.Name, "namespace", namespace, "error", err.Error())
		return nil, workload.UnresolvedRef(now)
	}
	if obs == nil {
		p.logger.V(1).Info("no Deployment resolved for persona; grounding falls back to the persona",
			"persona", ref.Name, "namespace", namespace, "tried", workload.ChainDescription())
		return nil, workload.UnresolvedRef(now)
	}

	return obs, obs.Ref(now)
}

// resolved reports whether a WorkloadRef describes a workload that was actually
// found, as opposed to the placeholder written when resolution failed.
func resolved(ref *dorguv1.WorkloadRef) bool {
	return ref != nil && ref.Name != ""
}

// observedValue returns the live value at a persona-spec resource path, e.g.
// "spec.resources.limits.memory". The second result distinguishes "the workload
// does not set this key" from "the value happens to be empty", which is the
// difference between a safe change and introducing a field (F-05).
func observedValue(ref *dorguv1.WorkloadRef, path string) (string, bool) {
	if !resolved(ref) || ref.ObservedResources == nil {
		return "", false
	}

	var section *dorguv1.ResourceValues
	var key string
	switch {
	case strings.HasPrefix(path, pathPrefixLimits):
		section, key = ref.ObservedResources.Limits, strings.TrimPrefix(path, pathPrefixLimits)
	case strings.HasPrefix(path, pathPrefixRequests):
		section, key = ref.ObservedResources.Requests, strings.TrimPrefix(path, pathPrefixRequests)
	default:
		return "", false
	}
	if section == nil {
		return "", false
	}

	switch key {
	case resourceKeyCPU:
		return section.CPU, section.CPU != ""
	case resourceKeyMemory:
		return section.Memory, section.Memory != ""
	default:
		return "", false
	}
}

// isResourcePath reports whether a persona-spec path lands on a container
// resource key. Only these paths are subject to the "never introduce a key the
// workload lacks" rule; the rest of the persona spec is Dorgu's own record.
func isResourcePath(path string) bool {
	if !strings.HasPrefix(path, pathPrefixLimits) && !strings.HasPrefix(path, pathPrefixRequests) {
		return false
	}
	leaf := path[strings.LastIndex(path, ".")+1:]
	return leaf == resourceKeyCPU || leaf == resourceKeyMemory
}

// groundedPrePatch rebuilds a pre-patch snapshot from the LIVE workload instead
// of the persona, at exactly the paths the patch touches.
//
// The blast-radius guardrail measures a change against its pre-patch state. The
// action's own PrePatchState is the persona's prior value, because that is what
// a persona rollback has to restore, but measuring the cap against it is what
// let a 32Mi container be raised to 144Mi and still be called "within 2x"
// (F-04). Safety and the clamp disclosure are therefore run against this
// snapshot.
//
// Returns nil when no live value is known for any path in the patch, in which
// case callers keep the persona-based baseline.
func groundedPrePatch(patch *apiextensionsv1.JSON, ref *dorguv1.WorkloadRef) *apiextensionsv1.JSON {
	if patch == nil || len(patch.Raw) == 0 || !resolved(ref) {
		return nil
	}

	values := make(map[string]string)
	for _, path := range patchLeafPaths(patch.Raw) {
		if live, ok := observedValue(ref, path); ok {
			values[path] = live
		}
	}
	if len(values) == 0 {
		return nil
	}

	snapshot := make(map[string]any)
	for _, path := range sortedStringKeys(values) {
		mergeNestedPath(snapshot, strings.Split(path, "."), values[path])
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

// mergeNestedPath writes value at the dot-path segments into root, creating
// intermediate maps as needed.
func mergeNestedPath(root map[string]any, segments []string, value string) {
	node := root
	for _, seg := range segments[:len(segments)-1] {
		child, ok := node[seg].(map[string]any)
		if !ok {
			child = make(map[string]any)
			node[seg] = child
		}
		node = child
	}
	node[segments[len(segments)-1]] = value
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// groundedSafetyProbe returns a copy of the action whose pre-patch state, on the
// single Action and on every step, is the live workload's value rather than the
// persona's. Safety checks run against this copy so the 2x cap bounds what
// actually happens to the cluster.
func groundedSafetyProbe(action *dorguv1.RemediationAction, ref *dorguv1.WorkloadRef) *dorguv1.RemediationAction {
	if !resolved(ref) {
		return action
	}
	probe := action.DeepCopy()
	if grounded := groundedPrePatch(probe.Spec.Action.Patch, ref); grounded != nil {
		probe.Spec.Action.PrePatchState = grounded
	}
	for i := range probe.Spec.Steps {
		if grounded := groundedPrePatch(probe.Spec.Steps[i].Patch, ref); grounded != nil {
			probe.Spec.Steps[i].PrePatchState = grounded
		}
	}
	return probe
}

// discloseGroundedBlastRadiusClamp runs the clamp disclosure against live
// values.
//
// discloseBlastRadiusClamp reads Patch versus PrePatchState, and PrePatchState
// holds persona values. With a stale persona that comparison both misses real
// clamps and invents imaginary ones, so the pre-patch fields are swapped for
// the live snapshot for the duration of the call and restored afterwards. The
// disclosure mutates prose and confidence only, never the patches, so the
// restore is exact.
func discloseGroundedBlastRadiusClamp(action *dorguv1.RemediationAction, ref *dorguv1.WorkloadRef) bool {
	if !resolved(ref) {
		return discloseBlastRadiusClamp(action)
	}

	type saved struct {
		target **apiextensionsv1.JSON
		value  *apiextensionsv1.JSON
	}
	var restore []saved

	swap := func(target **apiextensionsv1.JSON, patch *apiextensionsv1.JSON) {
		grounded := groundedPrePatch(patch, ref)
		if grounded == nil {
			return
		}
		restore = append(restore, saved{target: target, value: *target})
		*target = grounded
	}

	swap(&action.Spec.Action.PrePatchState, action.Spec.Action.Patch)
	for i := range action.Spec.Steps {
		swap(&action.Spec.Steps[i].PrePatchState, action.Spec.Steps[i].Patch)
	}

	disclosed := discloseBlastRadiusClamp(action)

	for _, s := range restore {
		*s.target = s.value
	}
	return disclosed
}

// dropAbsentResourceKeys removes any leaf of a persona patch that targets a
// resource key the live container does not set, returning the surviving patch
// and the paths that were dropped.
//
// F-05: approving a memory fix added a CPU limit the workload never had,
// because the persona had invented one at import time and the patch carried it
// along. A remediation may only move a number the workload already has. Adding
// a field has to be its own deliberate step, not a side effect.
//
// With no live observation nothing is dropped: we cannot claim a key is absent
// from a workload we could not read.
func dropAbsentResourceKeys(patch *apiextensionsv1.JSON, ref *dorguv1.WorkloadRef) (*apiextensionsv1.JSON, []string) {
	if patch == nil || len(patch.Raw) == 0 || !resolved(ref) {
		return patch, nil
	}

	var dropped []string
	for _, path := range patchLeafPaths(patch.Raw) {
		if !isResourcePath(path) {
			continue
		}
		if _, present := observedValue(ref, path); !present {
			dropped = append(dropped, path)
		}
	}
	if len(dropped) == 0 {
		return patch, nil
	}
	sort.Strings(dropped)

	return prunePatchPaths(patch, dropped), dropped
}

// deleteNestedPath removes a leaf at the given dot-path and prunes any map left
// empty by the removal.
func deleteNestedPath(node map[string]any, segments []string) {
	if len(segments) == 1 {
		delete(node, segments[0])
		return
	}
	child, ok := node[segments[0]].(map[string]any)
	if !ok {
		return
	}
	deleteNestedPath(child, segments[1:])
	if len(child) == 0 {
		delete(node, segments[0])
	}
}

// absentKeyNote explains, in the plan itself, which fields Dorgu declined to
// introduce and why. Silence here would be the same silent surprise as adding
// them.
func absentKeyNote(ref *dorguv1.WorkloadRef, dropped []string) string {
	names := make([]string, 0, len(dropped))
	for _, path := range dropped {
		names = append(names, path[strings.LastIndex(path, ".")+1:]+" ("+sectionOf(path)+")")
	}
	slices.Sort(names)
	return fmt.Sprintf(
		"Left out %s: container %q on %s does not set %s today, and introducing a resource field as a side effect of another fix changes how the workload is scheduled and throttled. Ask for it as its own change if you want it.",
		humanFieldList(names), ref.Container, ref.Name,
		pluralField(len(names)))
}

// sectionOf names the resource section a persona-spec path belongs to.
func sectionOf(path string) string {
	if strings.HasPrefix(path, pathPrefixRequests) {
		return "requests"
	}
	return "limits"
}

func pluralField(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
