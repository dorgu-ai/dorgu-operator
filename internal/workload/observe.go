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

package workload

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// kindDeployment is the only workload kind Dorgu resolves today.
const kindDeployment = "Deployment"

// Observation is the live state of the workload a persona describes, read at
// the moment a remediation is planned.
//
// The ApplicationPersona is a point-in-time import and the workload drifts from
// it, so nothing downstream may state a current fact or compute a cap from the
// persona. This is the ground truth those callers use instead.
type Observation struct {
	// Deployment is the resolved live Deployment.
	Deployment *appsv1.Deployment

	// Container is the container within the pod template that the persona
	// concerns, and whose resources a fix would change.
	Container *corev1.Container

	// MatchedBy is the resolution rung that found the Deployment, for logs and
	// status messages.
	MatchedBy string

	// Ownership is who reconciles this workload's desired state.
	Ownership Ownership
}

// Observe resolves the live Deployment a persona describes and reads the state
// a remediation must be grounded in.
//
// It returns (nil, nil) when nothing matched: an unresolvable workload is not
// an error, it just means callers fall back to the persona and must say so. A
// listing failure is returned as an error, since silently treating an API
// outage as "no workload" would let a stale persona drive the plan unannounced.
func Observe(ctx context.Context, c client.Reader, namespace, personaName string) (*Observation, error) {
	if personaName == "" || namespace == "" {
		return nil, nil
	}

	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Deployments in namespace %s: %w", namespace, err)
	}

	match, rung, err := Resolve(deployments.Items, personaName)
	if err != nil {
		// Ambiguous: several Deployments answer to this persona. Picking one is
		// how a reliability tool grounds itself in the wrong workload.
		return nil, err
	}
	if match == nil {
		return nil, nil
	}

	return &Observation{
		Deployment: match,
		Container:  PickContainer(match, personaName),
		MatchedBy:  rung,
		Ownership:  DetectOwner(match),
	}, nil
}

// PickContainer chooses the container a persona describes.
//
// A persona names an application, not a container, and the two rarely match in
// brownfield clusters (persona "frontend" over Deployment "frontend-podinfo"
// whose container is "podinfo"). The order is: exact name match, then the sole
// container, then the first. Callers record the chosen name so a reader can see
// what was actually inspected.
func PickContainer(deploy *appsv1.Deployment, personaName string) *corev1.Container {
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return nil
	}
	for i := range containers {
		if containers[i].Name == personaName {
			return &containers[i]
		}
	}
	return &containers[0]
}

// Ref builds the CRD record of this observation: what the workload is, who owns
// it, and the resource keys it actually sets.
func (o *Observation) Ref(now metav1.Time) *dorguv1.WorkloadRef {
	if o == nil || o.Deployment == nil {
		return UnresolvedRef(now)
	}

	ref := &dorguv1.WorkloadRef{
		Kind:            kindDeployment,
		Name:            o.Deployment.Name,
		Namespace:       o.Deployment.Namespace,
		ManagedBy:       o.Ownership.ManagedBy,
		ManagedByDetail: o.Ownership.Detail,
		ObservedAt:      &now,
	}
	if o.Container != nil {
		ref.Container = o.Container.Name
		ref.ObservedImage = o.Container.Image
		ref.ObservedResources = observedResources(o.Container)
	}
	return ref
}

// UnresolvedRef is the record written when no live workload could be observed.
// ManagedBy is unknown, which every consumer treats as owned, so an unreadable
// workload never becomes a licence to patch one.
func UnresolvedRef(now metav1.Time) *dorguv1.WorkloadRef {
	return &dorguv1.WorkloadRef{
		Kind:       kindDeployment,
		ManagedBy:  dorguv1.ManagedByUnknown,
		ObservedAt: &now,
	}
}

// observedResources reads a container's resource block, preserving which keys
// are set. An absent key stays an empty string rather than becoming a zero
// quantity, because "the workload has no CPU limit" and "the workload has a CPU
// limit of 0" are different facts and only the first forbids introducing one.
func observedResources(container *corev1.Container) *dorguv1.ObservedResources {
	limits := resourceValues(container.Resources.Limits)
	requests := resourceValues(container.Resources.Requests)
	if limits == nil && requests == nil {
		return nil
	}
	return &dorguv1.ObservedResources{Limits: limits, Requests: requests}
}

// resourceValues converts a ResourceList to the CRD's string pair, returning
// nil when neither cpu nor memory is set.
func resourceValues(list corev1.ResourceList) *dorguv1.ResourceValues {
	out := &dorguv1.ResourceValues{}
	if qty, ok := list[corev1.ResourceCPU]; ok {
		out.CPU = qty.String()
	}
	if qty, ok := list[corev1.ResourceMemory]; ok {
		out.Memory = qty.String()
	}
	if out.CPU == "" && out.Memory == "" {
		return nil
	}
	return out
}
