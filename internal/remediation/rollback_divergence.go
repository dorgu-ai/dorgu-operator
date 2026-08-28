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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// CR5-02, clean-room run #5: `RolledBack` rolled back half the world.
//
//	before approve    persona limits.memory 512Mi   deployment limits.memory  8Mi
//	after approve     persona limits.memory  16Mi   deployment limits.memory 16Mi
//	after RolledBack  persona limits.memory 512Mi   deployment limits.memory 16Mi
//
// The persona was restored. The Deployment kept the change, and the record said
// only "Remediation rolled back due to degraded health". No condition, no
// event, no log line, no CLI hint. "RolledBack" is a strong word: a reader
// takes it to mean the workload returned to its prior state and does not then
// go and diff the Deployment. Meanwhile the persona and the cluster now
// disagree in a NEW direction, and the next proposal re-anchors its blast
// radius on the still-changed live value.
//
// The answer is NOT to give the operator write access to Deployments. That
// restriction is the product's central promise and `/security-and-permissions`
// documents it; the operator holds get, list and watch on `apps/deployments`
// and nothing else. So it cannot un-patch a workload, and it should not be able
// to.
//
// What it can do is read, which is enough to stop being silent. It compares the
// live container against the values it recorded before the remediation, and
// reports every field the rollback could not reach: on a condition, in the log,
// and in a sentence the CLI can print verbatim, naming the Deployment, the
// container, the field, live-versus-intended, and what to do about it.

// Rollback condition reasons. They are exported because the controller writes
// them onto the RemediationAction and a client may key on them.
const (
	// ReasonWorkloadDiverged means the persona was restored and the live
	// workload still holds the value the remediation put there.
	ReasonWorkloadDiverged = "WorkloadDiverged"

	// ReasonWorkloadRestored means the live workload holds what it held before
	// the remediation, so the rollback really did undo everything.
	ReasonWorkloadRestored = "WorkloadRestored"

	// ReasonWorkloadUnreadable means Dorgu could not read the live workload, so
	// it will not claim either way.
	ReasonWorkloadUnreadable = "WorkloadUnreadable"
)

// WorkloadDivergence is one container field the rollback could not restore.
type WorkloadDivergence struct {
	// Field is the container-relative path, e.g. "resources.limits.memory".
	Field string

	// Live is what the container has now.
	Live string

	// Intended is what the container had when the remediation was proposed,
	// which is what a complete rollback would put back.
	Intended string
}

// RollbackOutcome is what a rollback actually achieved, as opposed to what the
// phase name implies.
type RollbackOutcome struct {
	// Ref is the workload that was compared, when there was one.
	Ref *dorguv1.WorkloadRef

	// Divergences are the fields the live workload still holds against Dorgu's
	// pre-remediation record of them, in a stable field order.
	Divergences []WorkloadDivergence

	// Unreadable says why no comparison was possible, and is empty when one was.
	Unreadable string
}

// Diverged reports whether the live workload kept any part of the change.
func (o RollbackOutcome) Diverged() bool {
	return len(o.Divergences) > 0
}

// Reason is the condition reason for this outcome.
func (o RollbackOutcome) Reason() string {
	switch {
	case o.Unreadable != "":
		return ReasonWorkloadUnreadable
	case o.Diverged():
		return ReasonWorkloadDiverged
	default:
		return ReasonWorkloadRestored
	}
}

// InspectRollback compares the live workload against what Dorgu recorded before
// the remediation, and reports what the rollback could not reach.
//
// It never returns an error. A rollback that succeeded must not be reported as
// failed because the advisory beside it could not be computed, so an unreadable
// workload becomes an outcome that says so rather than an error that hides it.
func (r *Rollback) InspectRollback(ctx context.Context, action *dorguv1.RemediationAction) RollbackOutcome {
	ref := action.Spec.WorkloadRef
	if ref == nil || ref.Name == "" || ref.Namespace == "" {
		return RollbackOutcome{
			Ref:        ref,
			Unreadable: "Dorgu never resolved a live Deployment for this application, so it cannot say whether the workload still carries the change.",
		}
	}

	var deploy appsv1.Deployment
	key := client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}
	if err := r.client.Get(ctx, key, &deploy); err != nil {
		return RollbackOutcome{
			Ref: ref,
			Unreadable: fmt.Sprintf(
				"Dorgu could not read Deployment %s/%s to check whether it still carries the change: %v",
				ref.Namespace, ref.Name, err),
		}
	}

	container := containerNamed(&deploy, ref.Container)
	if container == nil {
		return RollbackOutcome{
			Ref: ref,
			Unreadable: fmt.Sprintf(
				"Dorgu could not find container %q on Deployment %s/%s, so it cannot say whether the workload still carries the change.",
				ref.Container, ref.Namespace, ref.Name),
		}
	}

	return RollbackOutcome{
		Ref:         ref,
		Divergences: compareToPreRemediation(action, ref, container),
	}
}

// compareToPreRemediation returns every field the remediation touched whose
// live value no longer matches what Dorgu observed before it ran.
//
// The comparison is against ObservedResources, the live values read at proposal
// time, and NOT against the patch's prePatchState. prePatchState holds the
// PERSONA's prior value, which in the clean-room case was 512Mi — a value the
// Deployment never had. Telling a user to set their workload to 512Mi because
// the persona used to say so would be a worse bug than the one being fixed.
func compareToPreRemediation(
	action *dorguv1.RemediationAction,
	ref *dorguv1.WorkloadRef,
	container *corev1.Container,
) []WorkloadDivergence {
	if action.Spec.Action.Patch == nil || len(action.Spec.Action.Patch.Raw) == 0 {
		return nil
	}

	paths := make([]string, 0, 4)
	for path := range patchLeafValues(action.Spec.Action.Patch.Raw) {
		if isResourcePath(path) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	out := make([]WorkloadDivergence, 0, len(paths))
	for _, personaPath := range paths {
		field := strings.TrimPrefix(personaPath, "spec.")
		intended := observedResourceValue(ref, field)
		live := liveResourceValue(container, field)
		if intended == "" || live == intended {
			continue
		}
		out = append(out, WorkloadDivergence{Field: field, Live: live, Intended: intended})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Message is the sentence the condition carries and the CLI prints.
//
// It is written for someone who has just read the word "RolledBack" and
// believes the cluster is back where it started.
func (o RollbackOutcome) Message() string {
	if o.Unreadable != "" {
		return "Rollback restored the ApplicationPersona. " + o.Unreadable
	}
	if !o.Diverged() {
		return fmt.Sprintf(
			"Rollback restored the ApplicationPersona, and Deployment %s/%s matches what it held before the remediation. Nothing is left to undo.",
			o.Ref.Namespace, o.Ref.Name)
	}

	fields := make([]string, 0, len(o.Divergences))
	for _, d := range o.Divergences {
		fields = append(fields, fmt.Sprintf("%s is %s, and was %s", d.Field, d.Live, d.Intended))
	}

	return fmt.Sprintf(
		"Rollback restored the ApplicationPersona but NOT the workload. "+
			"Dorgu has no write access to Deployments, so it cannot un-patch one. "+
			"Container %q on Deployment %s/%s still carries the remediation: %s. "+
			"%s",
		o.Ref.Container, o.Ref.Namespace, o.Ref.Name,
		humanFieldList(fields),
		o.revertInstruction())
}

// revertInstruction says how to finish the rollback, in the form this
// workload's owner permits.
//
// An unmanaged workload gets the command, because a direct write is what
// unmanaged means and the reader would otherwise have to compose it themselves
// from four values printed above. Everything else, including unknown, gets the
// same owner-shaped instruction the plan itself would have used: a rollback
// advisory is not an exemption from the ownership rule, and handing a Helm user
// a `kubectl set resources` here would recreate F-02 on the way out.
func (o RollbackOutcome) revertInstruction() string {
	if o.Ref.ManagedBy == dorguv1.ManagedByUnmanaged {
		return "To finish the rollback: " + revertCommand(o.Ref, o.Divergences)
	}

	changes := make([]resourceChange, 0, len(o.Divergences))
	for _, d := range o.Divergences {
		changes = append(changes, resourceChange{Key: d.Field, Value: d.Intended})
	}
	return "To finish the rollback: " + ownerInstruction(o.Ref, changes, "") +
		" " + whyDorguWillNotPatch(o.Ref)
}

// revertCommand builds the one kubectl invocation that puts every diverged
// field back, grouped the way `kubectl set resources` expects.
func revertCommand(ref *dorguv1.WorkloadRef, divergences []WorkloadDivergence) string {
	limits := make([]string, 0, 2)
	requests := make([]string, 0, 2)
	for _, d := range divergences {
		key := d.Field[strings.LastIndex(d.Field, ".")+1:] + "=" + d.Intended
		if strings.Contains(d.Field, ".requests.") {
			requests = append(requests, key)
			continue
		}
		limits = append(limits, key)
	}

	cmd := fmt.Sprintf("kubectl set resources deployment/%s -n %s --containers=%s",
		ref.Name, ref.Namespace, ref.Container)
	if len(limits) > 0 {
		cmd += " --limits=" + strings.Join(limits, ",")
	}
	if len(requests) > 0 {
		cmd += " --requests=" + strings.Join(requests, ",")
	}
	return cmd
}

// containerNamed picks the container the workload ref names, falling back to
// the sole container the same way the observation did.
func containerNamed(deploy *appsv1.Deployment, name string) *corev1.Container {
	containers := deploy.Spec.Template.Spec.Containers
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

// observedResourceValue reads a container-relative path out of the pre-remediation
// record, returning "" when the workload did not set it.
func observedResourceValue(ref *dorguv1.WorkloadRef, field string) string {
	if ref.ObservedResources == nil {
		return ""
	}
	values := ref.ObservedResources.Limits
	if strings.Contains(field, ".requests.") {
		values = ref.ObservedResources.Requests
	}
	if values == nil {
		return ""
	}
	if strings.HasSuffix(field, ".cpu") {
		return values.CPU
	}
	if strings.HasSuffix(field, ".memory") {
		return values.Memory
	}
	return ""
}

// liveResourceValue reads the same path off the live container.
func liveResourceValue(container *corev1.Container, field string) string {
	list := container.Resources.Limits
	if strings.Contains(field, ".requests.") {
		list = container.Resources.Requests
	}
	name := corev1.ResourceMemory
	if strings.HasSuffix(field, ".cpu") {
		name = corev1.ResourceCPU
	}
	qty, ok := list[name]
	if !ok {
		return ""
	}
	return qty.String()
}
