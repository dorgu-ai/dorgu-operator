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

package controller

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// validateResources checks container resource limits against persona constraints.
func validateResources(persona *dorguv1.ApplicationPersona, deploy *appsv1.Deployment) []dorguv1.ValidationIssue {
	if persona.Spec.Resources == nil {
		return nil
	}

	var issues []dorguv1.ValidationIssue

	for _, container := range deploy.Spec.Template.Spec.Containers {
		// Check requests
		if persona.Spec.Resources.Limits != nil {
			if persona.Spec.Resources.Limits.CPU != "" {
				personaLimit := resource.MustParse(persona.Spec.Resources.Limits.CPU)
				containerLimit := container.Resources.Limits[corev1.ResourceCPU]
				if !containerLimit.IsZero() && containerLimit.Cmp(personaLimit) > 0 {
					issues = append(issues, dorguv1.ValidationIssue{
						Severity:   "warning",
						Field:      fmt.Sprintf("containers[%s].resources.limits.cpu", container.Name),
						Message:    fmt.Sprintf("Container CPU limit (%s) exceeds persona limit (%s)", containerLimit.String(), personaLimit.String()),
						Suggestion: fmt.Sprintf("Set CPU limit to at most %s", persona.Spec.Resources.Limits.CPU),
					})
				}
			}
			if persona.Spec.Resources.Limits.Memory != "" {
				personaLimit := resource.MustParse(persona.Spec.Resources.Limits.Memory)
				containerLimit := container.Resources.Limits[corev1.ResourceMemory]
				if !containerLimit.IsZero() && containerLimit.Cmp(personaLimit) > 0 {
					issues = append(issues, dorguv1.ValidationIssue{
						Severity:   "warning",
						Field:      fmt.Sprintf("containers[%s].resources.limits.memory", container.Name),
						Message:    fmt.Sprintf("Container memory limit (%s) exceeds persona limit (%s)", containerLimit.String(), personaLimit.String()),
						Suggestion: fmt.Sprintf("Set memory limit to at most %s", persona.Spec.Resources.Limits.Memory),
					})
				}
			}
		}

		// Check that requests are set
		if container.Resources.Requests.Cpu().IsZero() && container.Resources.Requests.Memory().IsZero() {
			issues = append(issues, dorguv1.ValidationIssue{
				Severity:   "warning",
				Field:      fmt.Sprintf("containers[%s].resources.requests", container.Name),
				Message:    "No resource requests set on container",
				Suggestion: "Set resource requests for predictable scheduling",
			})
		}
	}

	return issues
}

// validateReplicas checks deployment replica count against persona min/max.
func validateReplicas(persona *dorguv1.ApplicationPersona, deploy *appsv1.Deployment) []dorguv1.ValidationIssue {
	if persona.Spec.Scaling == nil {
		return nil
	}

	var issues []dorguv1.ValidationIssue

	replicas := int32(1)
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}

	if persona.Spec.Scaling.MinReplicas != nil && replicas < *persona.Spec.Scaling.MinReplicas {
		issues = append(issues, dorguv1.ValidationIssue{
			Severity:   "error",
			Field:      "spec.replicas",
			Message:    fmt.Sprintf("Deployment replicas (%d) below persona minimum (%d)", replicas, *persona.Spec.Scaling.MinReplicas),
			Suggestion: fmt.Sprintf("Set replicas to at least %d", *persona.Spec.Scaling.MinReplicas),
		})
	}
	if persona.Spec.Scaling.MaxReplicas != nil && replicas > *persona.Spec.Scaling.MaxReplicas {
		issues = append(issues, dorguv1.ValidationIssue{
			Severity:   "warning",
			Field:      "spec.replicas",
			Message:    fmt.Sprintf("Deployment replicas (%d) above persona maximum (%d)", replicas, *persona.Spec.Scaling.MaxReplicas),
			Suggestion: fmt.Sprintf("Set replicas to at most %d", *persona.Spec.Scaling.MaxReplicas),
		})
	}

	return issues
}

// validateHealthProbes checks liveness and readiness probe configuration.
func validateHealthProbes(persona *dorguv1.ApplicationPersona, deploy *appsv1.Deployment) []dorguv1.ValidationIssue {
	if persona.Spec.Health == nil {
		return nil
	}

	var issues []dorguv1.ValidationIssue

	for _, container := range deploy.Spec.Template.Spec.Containers {
		// Check liveness probe
		if persona.Spec.Health.LivenessPath != "" {
			if container.LivenessProbe == nil || container.LivenessProbe.HTTPGet == nil {
				issues = append(issues, dorguv1.ValidationIssue{
					Severity:   "warning",
					Field:      fmt.Sprintf("containers[%s].livenessProbe", container.Name),
					Message:    "Persona specifies livenessPath but container has no HTTP liveness probe",
					Suggestion: fmt.Sprintf("Add a liveness probe at %s", persona.Spec.Health.LivenessPath),
				})
			} else if container.LivenessProbe.HTTPGet.Path != persona.Spec.Health.LivenessPath {
				issues = append(issues, dorguv1.ValidationIssue{
					Severity:   "info",
					Field:      fmt.Sprintf("containers[%s].livenessProbe.httpGet.path", container.Name),
					Message:    fmt.Sprintf("Liveness path (%s) differs from persona (%s)", container.LivenessProbe.HTTPGet.Path, persona.Spec.Health.LivenessPath),
					Suggestion: fmt.Sprintf("Consider aligning to persona path: %s", persona.Spec.Health.LivenessPath),
				})
			}
		}

		// Check readiness probe
		if persona.Spec.Health.ReadinessPath != "" && container.ReadinessProbe == nil {
			issues = append(issues, dorguv1.ValidationIssue{
				Severity:   "warning",
				Field:      fmt.Sprintf("containers[%s].readinessProbe", container.Name),
				Message:    "Persona specifies readinessPath but container has no readiness probe",
				Suggestion: fmt.Sprintf("Add a readiness probe at %s", persona.Spec.Health.ReadinessPath),
			})
		}
	}

	return issues
}

// validateSecurityContext checks security policies against persona constraints.
func validateSecurityContext(persona *dorguv1.ApplicationPersona, deploy *appsv1.Deployment) []dorguv1.ValidationIssue {
	if persona.Spec.Policies == nil || persona.Spec.Policies.Security == nil {
		return nil
	}

	var issues []dorguv1.ValidationIssue
	sec := persona.Spec.Policies.Security

	podSec := deploy.Spec.Template.Spec.SecurityContext

	if sec.RunAsNonRoot != nil && *sec.RunAsNonRoot {
		if podSec == nil || podSec.RunAsNonRoot == nil || !*podSec.RunAsNonRoot {
			issues = append(issues, dorguv1.ValidationIssue{
				Severity:   "error",
				Field:      "spec.template.spec.securityContext.runAsNonRoot",
				Message:    "Persona requires runAsNonRoot but Deployment does not enforce it",
				Suggestion: "Set spec.template.spec.securityContext.runAsNonRoot: true",
			})
		}
	}

	for _, container := range deploy.Spec.Template.Spec.Containers {
		cSec := container.SecurityContext

		if sec.ReadOnlyRootFilesystem != nil && *sec.ReadOnlyRootFilesystem {
			if cSec == nil || cSec.ReadOnlyRootFilesystem == nil || !*cSec.ReadOnlyRootFilesystem {
				issues = append(issues, dorguv1.ValidationIssue{
					Severity:   "warning",
					Field:      fmt.Sprintf("containers[%s].securityContext.readOnlyRootFilesystem", container.Name),
					Message:    "Persona requires readOnlyRootFilesystem but container does not set it",
					Suggestion: "Set readOnlyRootFilesystem: true on the container security context",
				})
			}
		}

		if sec.AllowPrivilegeEscalation != nil && !*sec.AllowPrivilegeEscalation {
			if cSec != nil && cSec.AllowPrivilegeEscalation != nil && *cSec.AllowPrivilegeEscalation {
				issues = append(issues, dorguv1.ValidationIssue{
					Severity:   "error",
					Field:      fmt.Sprintf("containers[%s].securityContext.allowPrivilegeEscalation", container.Name),
					Message:    "Persona forbids privilege escalation but container allows it",
					Suggestion: "Set allowPrivilegeEscalation: false",
				})
			}
		}
	}

	return issues
}

// countSeverity counts validation issues of a given severity level.
func countSeverity(issues []dorguv1.ValidationIssue, severity string) int {
	count := 0
	for _, i := range issues {
		if i.Severity == severity {
			count++
		}
	}
	return count
}
