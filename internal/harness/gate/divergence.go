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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/workload"
)

// Why a stamp is judged by what it owns, not by who it says it is (re-review N1).
//
// metadata.managedFields[].manager is whatever the writing client sent as
// fieldManager. Anyone who can patch the Deployment can call themselves
// "kube-controller-manager" or "dorgu", so a name-only exemption hides their write.
// What a client cannot choose is the field set the apiserver records for the write:
// the fields it actually changed. A stamp is exempt only when that set is a subset
// of what the named writer legitimately owns, and an entry whose fields are
// missing, empty or unreadable is not exempt (fail toward diverged).

// ConditionWorkloadPatched is the condition the CLI sets on a RemediationAction
// when it applied the ticket's change to the Deployment.
const ConditionWorkloadPatched = "WorkloadPatched"

// unknownWriter is what stampsSince reports when it can tell the Deployment moved
// but not who moved it.
const unknownWriter = "an unknown writer (no usable managedFields)"

// revisionAnnotation is the one main-resource field the Deployment controller
// writes: it records the rollout revision after each template change. The
// controller's other writes are status (its own subresource entry) and ReplicaSet
// bookkeeping (other objects). That is the whole list.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// statusSubresource is the subresource whose entries own only status, which no
// spec change, and so no generation, comes from.
const statusSubresource = "status"

// fieldPath is one node of a FieldsV1 tree: ["f:spec", "f:replicas"]. Each element
// is kept whole because keys such as f:deployment.kubernetes.io/revision contain dots.
type fieldPath []string

// ownedPaths lists the nodes a managedFields entry owns: every leaf, and every
// node marked with "." (owned itself, not only through its children). It reports
// false for no fields, empty fields, or anything that is not a FieldsV1 tree.
func ownedPaths(fields *metav1.FieldsV1) ([]fieldPath, bool) {
	if fields == nil || len(fields.Raw) == 0 {
		return nil, false
	}
	var root map[string]any
	if err := json.Unmarshal(fields.Raw, &root); err != nil || len(root) == 0 {
		return nil, false
	}
	var out []fieldPath
	valid := true
	var walk func(prefix fieldPath, node map[string]any)
	walk = func(prefix fieldPath, node map[string]any) {
		if len(node) == 0 {
			if len(prefix) > 0 {
				out = append(out, slices.Clone(prefix))
			}
			return
		}
		for key, val := range node {
			child, isMap := val.(map[string]any)
			if !isMap {
				valid = false
				return
			}
			if key == "." {
				out = append(out, slices.Clone(prefix))
				continue
			}
			walk(append(slices.Clone(prefix), key), child)
		}
	}
	walk(nil, root)
	if !valid || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// ownsOnly reports whether every node the entry owns lies on the way to, or at,
// one of the allowed paths. An entry that owns nothing readable owns nothing the
// caller can vouch for, so it is false.
func ownsOnly(fields *metav1.FieldsV1, allowed []fieldPath) bool {
	owned, ok := ownedPaths(fields)
	if !ok {
		return false
	}
	for _, path := range owned {
		if !slices.ContainsFunc(allowed, func(a fieldPath) bool { return isPrefix(path, a) }) {
			return false
		}
	}
	return true
}

// isPrefix reports whether p is a, or an ancestor of a.
func isPrefix(p, a fieldPath) bool {
	return len(p) <= len(a) && slices.Equal(p, a[:len(p)])
}

// controllerOwnedPaths is everything the Deployment controller owns on the main
// resource: the revision annotation (re-review N1).
func controllerOwnedPaths() []fieldPath {
	return []fieldPath{{"f:metadata", "f:annotations", "f:" + revisionAnnotation}}
}

// ticketOwnedPaths is what the CLI's strategic-merge patch of the ticket owns on
// the Deployment: the container's list entry and merge key, and each resource
// leaf the ticket patches (spec.resources.limits.memory becomes
// resources.limits.memory under the container). Nothing else is Dorgu's, whatever
// the entry's manager name says.
func ticketOwnedPaths(dep *appsv1.Deployment, ticket *dorguv1.ApplyTicket) []fieldPath {
	container := containerFor(dep, ticket.Container)
	if container == nil || ticket.Patch == nil {
		return nil
	}
	base := fieldPath{"f:spec", "f:template", "f:spec", "f:containers", fmt.Sprintf(`k:{"name":%q}`, container.Name)}
	allowed := []fieldPath{append(slices.Clone(base), "f:name")}
	for _, leaf := range resourceLeaves(ticket) {
		path := slices.Clone(base)
		path = append(path, "f:resources")
		for part := range strings.SplitSeq(leaf.field, ".") {
			path = append(path, "f:"+part)
		}
		allowed = append(allowed, path)
	}
	return allowed
}

// containerFor finds the Deployment container a ticket is about: the one it names,
// or the only one when it names none that exists, as the rollback does.
func containerFor(dep *appsv1.Deployment, name string) *corev1.Container {
	containers := dep.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	if len(containers) == 1 {
		return &containers[0]
	}
	return nil
}

// stampSummary is what the managedFields of a moved Deployment say about who
// wrote it since the ticket.
type stampSummary struct {
	// other names a writer that is not accounted for ("" when none is).
	other string
	// dorguPatch is true when an exempt entry shows Dorgu's CLI patched the
	// ticket's fields since the ticket.
	dorguPatch bool
}

// stampsSince reads dep's managedFields for writes at or after since.
//
// Exempt, because the entry owns nothing but what its writer legitimately owns:
//   - Dorgu's CLI (DorguFieldManager) when it owns only the ticket's fields;
//   - the Deployment controller (kubeControllerManager) when it owns only the
//     revision annotation;
//   - any writer's status subresource entry (status moves no spec).
//
// Every other entry, including one under those two names that owns anything more,
// is the writer it names. The stamps are the apiserver's own, compared with a margin
// of ClockSkewMargin before since because the issue time is the operator's clock;
// managedFields stamps whole seconds, so the comparison is made at that resolution.
// No entries at all, or an entry with no stamp, is an unknown writer: the caller
// only asks once the resourceVersion has moved, and a move nothing explains is not
// clean.
func stampsSince(dep *appsv1.Deployment, ticket *dorguv1.ApplyTicket, since time.Time) stampSummary {
	cut := since.Add(-ClockSkewMargin).Truncate(time.Second)
	if len(dep.ManagedFields) == 0 {
		return stampSummary{other: unknownWriter}
	}
	var summary stampSummary
	ticketPaths := ticketOwnedPaths(dep, ticket)
	for i := range dep.ManagedFields {
		mf := &dep.ManagedFields[i]
		if mf.Subresource == statusSubresource {
			continue
		}
		if mf.Time == nil {
			summary.other = firstNonEmpty(summary.other, unknownWriter)
			continue
		}
		if mf.Time.Time.Before(cut) {
			continue
		}
		switch {
		case mf.Manager == DorguFieldManager && mf.Subresource == "" && ownsOnly(mf.FieldsV1, ticketPaths):
			summary.dorguPatch = true
		case mf.Manager == kubeControllerManager && mf.Subresource == "" && ownsOnly(mf.FieldsV1, controllerOwnedPaths()):
		default:
			summary.other = firstNonEmpty(summary.other, mf.Manager)
		}
	}
	return summary
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// generationProblem bounds metadata.generation (re-review N2). A write that
// removes a field (a JSON-patch remove, a client-side apply that drops one) moves
// the generation without leaving a fresh managedFields stamp, so the stamps alone
// cannot show it. The ticket recorded the generation it was written against; the
// only thing since that may account for a move of it is Dorgu's own patch, which is
// one generation: recorded by the CLI (WorkloadPatched) or visible as an exempt
// Dorgu stamp on the ticket's fields. Anything beyond that is somebody else's
// spec change.
func generationProblem(dep *appsv1.Deployment, action *dorguv1.RemediationAction, stamps stampSummary) string {
	ticket := action.Status.ApplyTicket
	allowed := ticket.ObservedGeneration
	if workloadPatchedRecorded(action) || stamps.dorguPatch {
		allowed++
	}
	if dep.Generation <= allowed {
		return ""
	}
	return fmt.Sprintf("the Deployment's spec changed since the ticket (generation %d, the ticket accounts for %d) and nothing recorded explains it",
		dep.Generation, allowed)
}

// workloadPatchedRecorded reports the CLI's WorkloadPatched=True on the action.
func workloadPatchedRecorded(action *dorguv1.RemediationAction) bool {
	for i := range action.Status.Conditions {
		c := action.Status.Conditions[i]
		if c.Type == ConditionWorkloadPatched {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// resourceLeaf is one resource value the ticket writes.
type resourceLeaf struct {
	// path is the persona path, spec.resources.limits.memory.
	path string
	// field is the container-relative path, limits.memory.
	field string
	value string
}

// resourceLeaves lists the ticket's resource values, sorted by path.
func resourceLeaves(ticket *dorguv1.ApplyTicket) []resourceLeaf {
	values := PatchLeafValues(ticket.Patch.Raw)
	out := make([]resourceLeaf, 0, len(values))
	for path, value := range values {
		if !IsResourcePath(path) {
			continue
		}
		out = append(out, resourceLeaf{path: path, field: strings.TrimPrefix(path, "spec.resources."), value: value})
	}
	slices.SortFunc(out, func(a, b resourceLeaf) int { return strings.Compare(a.path, b.path) })
	return out
}

// HealEvidence is what the live Deployment says about a heal: the spec generation,
// and whether the container holds the values the ticket wrote.
type HealEvidence struct {
	// Generation is metadata.generation of the Deployment the ticket is bound to.
	Generation int64
	// ResourcesMatchTicket is true when every resource value the ticket wrote is
	// what the live container has now, compared as quantities.
	ResourcesMatchTicket bool
	// Mismatch says which value was not the ticket's, when ResourcesMatchTicket is false.
	Mismatch string
}

// HealEvidence reads the Deployment the ticket is bound to, uncached. It errors
// when the workload cannot be read or is not the ticket's: the generation or the
// resources of some other Deployment say nothing about this heal.
//
// A heal is credited only when the apiserver shows the workload now holds what the
// ticket wrote. The CLI's WorkloadPatched condition and a moved generation are
// claims anyone with a status or spec write can produce; the live values are what
// the heal was for.
func (g *Gate) HealEvidence(ctx context.Context, action *dorguv1.RemediationAction) (*HealEvidence, error) {
	if g == nil || action.Status.ApplyTicket == nil {
		return nil, errors.New("no gate or no ticket to read the workload for")
	}
	ticket := action.Status.ApplyTicket
	key := ticketKey(action)
	obs, err := workload.Observe(ctx, g.reader, key.Namespace, key.Name)
	if err != nil {
		return nil, fmt.Errorf("reading the live workload for %s/%s: %w", key.Namespace, key.Name, err)
	}
	if obs == nil || obs.Deployment == nil {
		return nil, errors.New("the Deployment the ticket was written against no longer exists")
	}
	dep := obs.Deployment
	if string(dep.UID) != ticket.WorkloadUID {
		return nil, errors.New("the Deployment is not the one the ticket was written against")
	}
	evidence := &HealEvidence{Generation: dep.Generation}
	evidence.Mismatch = resourceMismatch(dep, ticket)
	evidence.ResourcesMatchTicket = evidence.Mismatch == ""
	return evidence, nil
}

// resourceMismatch names the first ticket resource value the live container does
// not hold, or "" when it holds all of them. A ticket with no resource value, a
// container that cannot be found, or a value that is unset or unreadable is a
// mismatch.
func resourceMismatch(dep *appsv1.Deployment, ticket *dorguv1.ApplyTicket) string {
	container := containerFor(dep, ticket.Container)
	if container == nil {
		return fmt.Sprintf("the ticket's container %q is not in the Deployment", ticket.Container)
	}
	if ticket.Patch == nil {
		return "the ticket carries no patch"
	}
	leaves := resourceLeaves(ticket)
	if len(leaves) == 0 {
		return "the ticket writes no resource value"
	}
	for _, leaf := range leaves {
		want, err := resource.ParseQuantity(leaf.value)
		if err != nil {
			return fmt.Sprintf("%s: the ticket's value %q is not a quantity", leaf.path, leaf.value)
		}
		live, ok := liveResource(container, leaf.field)
		if !ok {
			return fmt.Sprintf("%s: the live container sets none, the ticket wrote %s", leaf.path, leaf.value)
		}
		if live.Cmp(want) != 0 {
			return fmt.Sprintf("%s: the live container has %s, the ticket wrote %s", leaf.path, live.String(), leaf.value)
		}
	}
	return ""
}

// liveResource reads a container-relative field (limits.memory, requests.cpu).
func liveResource(container *corev1.Container, field string) (resource.Quantity, bool) {
	section, name, found := strings.Cut(field, ".")
	if !found {
		return resource.Quantity{}, false
	}
	var list corev1.ResourceList
	switch section {
	case "limits":
		list = container.Resources.Limits
	case "requests":
		list = container.Resources.Requests
	default:
		return resource.Quantity{}, false
	}
	q, ok := list[corev1.ResourceName(name)]
	return q, ok
}
