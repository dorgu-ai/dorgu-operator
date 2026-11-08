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
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Persona-spec resource paths a remediation may target, and the live container
// field each one lands on once the CLI applies the persona to the Deployment.
const (
	PathPrefixLimits   = "spec.resources.limits."
	PathPrefixRequests = "spec.resources.requests."

	// ResourcePathMemory and ResourcePathCPU are the two limit paths a
	// remediation may change. They are the whole of the persona-update allowlist's
	// writable surface today.
	ResourcePathMemory = "spec.resources.limits.memory"
	ResourcePathCPU    = "spec.resources.limits.cpu"

	ResourceKeyCPU    = "cpu"
	ResourceKeyMemory = "memory"
)

// PatchTouchesPath reports whether a merge patch sets a value at the given
// dot-joined leaf path, e.g. {"spec":{"resources":{"limits":{"memory":"384Mi"}}}}
// touches "spec.resources.limits.memory".
func PatchTouchesPath(raw []byte, target string) bool {
	return slices.Contains(PatchLeafPaths(raw), target)
}

// PatchLeafPaths walks a JSON merge patch and returns the dot-joined path of
// every leaf (non-object) value it sets. Returns nil for empty or invalid JSON.
func PatchLeafPaths(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	var paths []string
	var walk func(prefix string, node map[string]any)
	walk = func(prefix string, node map[string]any) {
		for key, val := range node {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if child, ok := val.(map[string]any); ok {
				walk(path, child)
				continue
			}
			paths = append(paths, path)
		}
	}
	walk("", root)
	return paths
}

// PatchLeafValues walks a JSON merge patch and returns every leaf string value
// keyed by its dot-joined path. Non-string leaves are skipped: the resource
// fields this is used for are always strings.
func PatchLeafValues(raw []byte) map[string]string {
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

// ParseResourcePatch extracts resource field→quantity mappings from a JSON patch.
// Expected format: {"spec":{"resources":{"limits":{"memory":"512Mi","cpu":"500m"}}}}
func ParseResourcePatch(raw []byte) (map[string]resource.Quantity, error) {
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, fmt.Errorf("unmarshalling patch: %w", err)
	}

	result := make(map[string]resource.Quantity)
	extractResources(patch, "", result)
	return result, nil
}

// extractResources recursively walks the patch structure to find resource values.
func extractResources(obj map[string]any, prefix string, result map[string]resource.Quantity) {
	for key, val := range obj {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}

		switch v := val.(type) {
		case map[string]any:
			extractResources(v, fullKey, result)
		case string:
			qty, err := resource.ParseQuantity(v)
			if err == nil {
				result[fullKey] = qty
			}
		}
	}
}

// Resolved reports whether a WorkloadRef describes a workload that was actually
// found, as opposed to the placeholder written when resolution failed.
func Resolved(ref *dorguv1.WorkloadRef) bool {
	return ref != nil && ref.Name != ""
}

// ObservedValue returns the live value at a persona-spec resource path, e.g.
// "spec.resources.limits.memory". The second result distinguishes "the workload
// does not set this key" from "the value happens to be empty", which is the
// difference between a safe change and introducing a field (F-05).
func ObservedValue(ref *dorguv1.WorkloadRef, path string) (string, bool) {
	if !Resolved(ref) || ref.ObservedResources == nil {
		return "", false
	}

	var section *dorguv1.ResourceValues
	var key string
	switch {
	case strings.HasPrefix(path, PathPrefixLimits):
		section, key = ref.ObservedResources.Limits, strings.TrimPrefix(path, PathPrefixLimits)
	case strings.HasPrefix(path, PathPrefixRequests):
		section, key = ref.ObservedResources.Requests, strings.TrimPrefix(path, PathPrefixRequests)
	default:
		return "", false
	}
	if section == nil {
		return "", false
	}

	switch key {
	case ResourceKeyCPU:
		return section.CPU, section.CPU != ""
	case ResourceKeyMemory:
		return section.Memory, section.Memory != ""
	default:
		return "", false
	}
}

// IsResourcePath reports whether a persona-spec path lands on a container
// resource key. Only these paths are subject to the "never introduce a key the
// workload lacks" rule; the rest of the persona spec is Dorgu's own record.
func IsResourcePath(path string) bool {
	if !strings.HasPrefix(path, PathPrefixLimits) && !strings.HasPrefix(path, PathPrefixRequests) {
		return false
	}
	leaf := path[strings.LastIndex(path, ".")+1:]
	return leaf == ResourceKeyCPU || leaf == ResourceKeyMemory
}

// GroundedPrePatch rebuilds a pre-patch snapshot from the LIVE workload instead
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
func GroundedPrePatch(patch *apiextensionsv1.JSON, ref *dorguv1.WorkloadRef) *apiextensionsv1.JSON {
	if patch == nil || len(patch.Raw) == 0 || !Resolved(ref) {
		return nil
	}

	values := make(map[string]string)
	for _, path := range PatchLeafPaths(patch.Raw) {
		if live, ok := ObservedValue(ref, path); ok {
			values[path] = live
		}
	}
	if len(values) == 0 {
		return nil
	}

	snapshot := make(map[string]any)
	for _, path := range SortedStringKeys(values) {
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

func SortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// DropAbsentResourceKeys removes any leaf of a persona patch that targets a
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
func DropAbsentResourceKeys(patch *apiextensionsv1.JSON, ref *dorguv1.WorkloadRef) (*apiextensionsv1.JSON, []string) {
	if patch == nil || len(patch.Raw) == 0 || !Resolved(ref) {
		return patch, nil
	}

	var dropped []string
	for _, path := range PatchLeafPaths(patch.Raw) {
		if !IsResourcePath(path) {
			continue
		}
		if _, present := ObservedValue(ref, path); !present {
			dropped = append(dropped, path)
		}
	}
	if len(dropped) == 0 {
		return patch, nil
	}
	sort.Strings(dropped)

	return PrunePatchPaths(patch, dropped), dropped
}

// MergePatches returns overlay's leaves merged over base's, with overlay
// winning at any path both set. A nil or empty base yields overlay unchanged.
func MergePatches(base, overlay *apiextensionsv1.JSON) *apiextensionsv1.JSON {
	if base == nil || len(base.Raw) == 0 {
		return overlay
	}
	if overlay == nil || len(overlay.Raw) == 0 {
		return base
	}

	var baseMap, overlayMap map[string]any
	if err := json.Unmarshal(base.Raw, &baseMap); err != nil {
		return overlay
	}
	if err := json.Unmarshal(overlay.Raw, &overlayMap); err != nil {
		return base
	}

	// mergeMaps is the executor's own JSON merge-patch implementation, reused so
	// what the plan records and what the executor later applies agree by
	// construction rather than by two similar functions staying in step.
	raw, err := json.Marshal(MergeMaps(baseMap, overlayMap))
	if err != nil {
		return overlay
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

// PrunePatchPaths returns the patch with the given dot-joined leaf paths
// removed, pruning any map left empty, and nil when nothing survives.
func PrunePatchPaths(patch *apiextensionsv1.JSON, paths []string) *apiextensionsv1.JSON {
	if patch == nil || len(patch.Raw) == 0 || len(paths) == 0 {
		return patch
	}

	var root map[string]any
	if err := json.Unmarshal(patch.Raw, &root); err != nil {
		return patch
	}
	for _, path := range paths {
		deleteNestedPath(root, strings.Split(path, "."))
	}
	if len(root) == 0 {
		return nil
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return patch
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

// FormatRatio renders a multiplier the way the guardrail's own messages do.
func FormatRatio(ratio float64) string {
	return fmt.Sprintf("%.1fx", ratio)
}

// DorguAuthoredDescription is the sentence shown for a step Dorgu has ruled on:
// what the step does, then every verdict that shaped it.
func DorguAuthoredDescription(base string, safety []dorguv1.StepSafety) string {
	parts := make([]string, 0, len(safety)+1)
	if base == "" {
		base = "This step applies nothing."
	}
	parts = append(parts, base)
	for _, s := range safety {
		parts = append(parts, s.Message)
	}
	return strings.Join(parts, " ")
}

// describeStepPatch states what a persona patch sets, field by field.
func describeStepPatch(patch *apiextensionsv1.JSON) string {
	if patch == nil || len(patch.Raw) == 0 {
		return ""
	}
	values := PatchLeafValues(patch.Raw)
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, field := range SortedStringKeys(values) {
		parts = append(parts, fmt.Sprintf("%s to %s", field, values[field]))
	}
	return "Set " + HumanFieldList(parts) + " on the ApplicationPersona."
}

// GuardedRationale replaces the model's reasoning on a step a guardrail has
// changed. The model argued for a value that is no longer the value in the
// patch, and leaving that argument in place beside Dorgu's verdict is what made
// the guardrail read as one more opinion.
func GuardedRationale() string {
	return "Dorgu decides what its guardrails permit, so the outcome above is Dorgu's own arithmetic against the workload it observed, not the plan's account of it."
}

// BlastRadiusRejectedMessage states a refusal with nothing left in its place.
func BlastRadiusRejectedMessage(s dorguv1.StepSafety) string {
	return fmt.Sprintf(
		"Blast-radius guardrail: the plan asked to set %s to %s, which is %s the current %s. The ceiling is %s, so this field is refused and nothing will be applied for it.",
		s.Field, s.Requested, s.Ratio, s.Baseline, s.MaxRatio)
}

// BlastRadiusClampedMessage states a refusal and the value Dorgu applies
// instead, including the caveat that the permitted value may not be enough.
func BlastRadiusClampedMessage(s dorguv1.StepSafety) string {
	return fmt.Sprintf(
		"Blast-radius guardrail: the plan asked to set %s to %s, which is %s the current %s. The ceiling is %s, so Dorgu will apply %s instead, which may not be enough on its own.",
		s.Field, s.Requested, s.Ratio, s.Baseline, s.MaxRatio, s.Permitted)
}

// DerivedMessage states that Dorgu supplied a value the plan did not.
func DerivedMessage(field, permitted string) string {
	return fmt.Sprintf(
		"The plan named a change to %s but carried no patch Dorgu could apply, so Dorgu sized it to %s from the workload it observed.",
		field, permitted)
}

// HumanFieldList renders field paths as prose: "a", "a and b", "a, b and c".
func HumanFieldList(fields []string) string {
	switch len(fields) {
	case 0:
		return "the proposed value"
	case 1:
		return fields[0]
	default:
		return strings.Join(fields[:len(fields)-1], ", ") + " and " + fields[len(fields)-1]
	}
}

// SnapshotPrePatch builds a pre-patch snapshot: the current persona-spec values
// at exactly the paths the patch touches. This gives safety.checkBlastRadius the
// old values to compare against and the executor a rollback target. Returns nil
// when no snapshot can be produced (e.g. persona unavailable or paths absent).
func SnapshotPrePatch(persona *dorguv1.ApplicationPersona, patch json.RawMessage) json.RawMessage {
	if persona == nil {
		return nil
	}

	var patchMap map[string]any
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil
	}

	specBytes, err := json.Marshal(persona.Spec)
	if err != nil {
		return nil
	}
	var specMap map[string]any
	if err := json.Unmarshal(specBytes, &specMap); err != nil {
		return nil
	}
	// The patch is wrapped in {"spec": ...}; align the current object the same way.
	currentMap := map[string]any{"spec": specMap}

	snap := mirrorExistingPaths(patchMap, currentMap)
	if len(snap) == 0 {
		return nil
	}
	out, err := json.Marshal(snap)
	if err != nil {
		return nil
	}
	return out
}

// mirrorExistingPaths walks the patch structure and returns a map containing the
// current values at each leaf path of the patch that also exists in current.
// Paths absent from current are skipped (no prior value to record).
func mirrorExistingPaths(patch, current map[string]any) map[string]any {
	out := make(map[string]any)
	for key, patchVal := range patch {
		curVal, ok := current[key]
		if !ok {
			continue
		}
		patchChild, patchIsMap := patchVal.(map[string]any)
		curChild, curIsMap := curVal.(map[string]any)
		if patchIsMap && curIsMap {
			child := mirrorExistingPaths(patchChild, curChild)
			if len(child) > 0 {
				out[key] = child
			}
			continue
		}
		// Leaf (or shape mismatch): record the current value.
		out[key] = curVal
	}
	return out
}

// MergeMaps recursively merges patch into target following JSON merge patch semantics.
func MergeMaps(target, patch map[string]any) map[string]any {
	result := make(map[string]any, len(target))
	maps.Copy(result, target)

	for k, patchVal := range patch {
		if patchVal == nil {
			delete(result, k)
			continue
		}

		patchMap, patchIsMap := patchVal.(map[string]any)
		if !patchIsMap {
			result[k] = patchVal
			continue
		}

		targetVal, targetExists := result[k]
		if !targetExists {
			result[k] = patchVal
			continue
		}

		targetMap, targetIsMap := targetVal.(map[string]any)
		if !targetIsMap {
			result[k] = patchVal
			continue
		}

		result[k] = MergeMaps(targetMap, patchMap)
	}

	return result
}
