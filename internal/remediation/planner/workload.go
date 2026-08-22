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

package planner

import (
	"slices"

	appsv1 "k8s.io/api/apps/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// AnnotationImportedImage is the image reference the CLI records on a persona
// when it imports an existing application. It is a version Dorgu has actually
// read, so the planner is allowed to name it, unlike a tag recalled from
// training data.
const AnnotationImportedImage = "dorgu.io/imported-image"

// NewWorkloadContext assembles the live-workload half of a RemediationContext
// from an observed Deployment, its CRD record, and the persona's recorded image
// history.
//
// A nil ref (no Deployment resolved) yields a nil context, which the prompt
// reports as "the live workload could not be read" instead of quietly falling
// back to persona numbers.
func NewWorkloadContext(ref *dorguv1.WorkloadRef, deploy *appsv1.Deployment, persona *dorguv1.ApplicationPersona) *WorkloadContext {
	if ref == nil || ref.Name == "" {
		return nil
	}

	wc := &WorkloadContext{
		Ref:         ref,
		PriorImages: priorImages(persona, ref.ObservedImage),
	}
	if deploy != nil {
		if deploy.Spec.Replicas != nil {
			wc.Replicas = *deploy.Spec.Replicas
		}
		wc.ReadyReplicas = deploy.Status.ReadyReplicas
	}
	return wc
}

// priorImages collects the image references Dorgu has on record for this
// application, most trustworthy first, excluding the one that is running now.
//
// F-03: the correct rollback target for a bad tag was sitting in an annotation
// Dorgu itself wrote, while the plan recommended a seven-minor-version
// downgrade it had invented. Anything listed here was read from the cluster.
func priorImages(persona *dorguv1.ApplicationPersona, currentImage string) []string {
	if persona == nil {
		return nil
	}

	candidates := []string{
		persona.GetAnnotations()[AnnotationImportedImage],
	}
	if d := persona.Status.Deployments; d != nil {
		candidates = append(candidates, d.LastSuccessful, d.Current)
	}

	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c == "" || c == currentImage || slices.Contains(out, c) {
			continue
		}
		out = append(out, c)
	}
	return out
}
