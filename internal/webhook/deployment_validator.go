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

package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ValidationMode determines whether the webhook only warns or can reject.
type ValidationMode string

const (
	// ModeAdvisory always allows the request but adds warnings.
	ModeAdvisory ValidationMode = "advisory"
	// ModeEnforcing rejects if validation errors are found.
	ModeEnforcing ValidationMode = "enforcing"
)

// DeploymentValidator validates Deployment create/update against ApplicationPersona.
type DeploymentValidator struct {
	Client  client.Client
	Mode    ValidationMode
	decoder admission.Decoder
}

// Handle implements admission.Handler.
func (v *DeploymentValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithName("deployment-validator")

	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("no opinion on delete/connect")
	}

	deploy := &appsv1.Deployment{}
	if err := v.decoder.Decode(req, deploy); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Look up the app name from labels
	appName := deploy.Labels["app.kubernetes.io/name"]
	if appName == "" {
		return admission.Allowed("no app.kubernetes.io/name label; skipping persona validation")
	}

	// Find matching ApplicationPersona(s) in the same namespace
	personas := &dorguv1.ApplicationPersonaList{}
	if err := v.Client.List(ctx, personas, &client.ListOptions{Namespace: deploy.Namespace}); err != nil {
		log.Error(err, "Failed to list personas")
		// Don't block deploys on operator errors
		return admission.Allowed("failed to list personas; allowing by default")
	}

	var matchingPersona *dorguv1.ApplicationPersona
	for i := range personas.Items {
		if personas.Items[i].Spec.Name == appName {
			matchingPersona = &personas.Items[i]
			break
		}
	}

	if matchingPersona == nil {
		return admission.Allowed("no matching ApplicationPersona found")
	}

	// Run validations
	var warnings []string
	var errors []string

	validateWebhookResources(matchingPersona, deploy, &warnings)
	validateWebhookReplicas(matchingPersona, deploy, &warnings, &errors)
	validateWebhookSecurity(matchingPersona, deploy, &errors)
	validateWebhookProbes(matchingPersona, deploy, &warnings)

	log.Info("Webhook validation complete",
		"app", appName,
		"mode", v.Mode,
		"warnings", len(warnings),
		"errors", len(errors),
	)

	// Advisory mode: always allow, attach warnings
	if v.Mode == ModeAdvisory {
		allWarnings := append(warnings, errors...)
		if len(allWarnings) > 0 {
			return admission.Allowed("advisory mode").WithWarnings(allWarnings...)
		}
		return admission.Allowed("all persona checks passed")
	}

	// Enforcing mode: reject on errors
	if len(errors) > 0 {
		msg := fmt.Sprintf("Deployment violates ApplicationPersona '%s': %s",
			matchingPersona.Name, strings.Join(errors, "; "))
		resp := admission.Denied(msg)
		if len(warnings) > 0 {
			resp = resp.WithWarnings(warnings...)
		}
		return resp
	}

	if len(warnings) > 0 {
		return admission.Allowed("passed with warnings").WithWarnings(warnings...)
	}
	return admission.Allowed("all persona checks passed")
}

// InjectDecoder injects the admission decoder.
func (v *DeploymentValidator) InjectDecoder(d admission.Decoder) error {
	v.decoder = d
	return nil
}

// ============================================================================
// Webhook-specific validation (simpler than controller; returns string slices)
// ============================================================================

func validateWebhookResources(persona *dorguv1.ApplicationPersona, deploy *appsv1.Deployment, warnings *[]string) {
	if persona.Spec.Resources == nil || persona.Spec.Resources.Limits == nil {
		return
	}

	for _, container := range deploy.Spec.Template.Spec.Containers {
		if persona.Spec.Resources.Limits.CPU != "" {
			pLimit := resource.MustParse(persona.Spec.Resources.Limits.CPU)
			cLimit := container.Resources.Limits[corev1.ResourceCPU]
			if !cLimit.IsZero() && cLimit.Cmp(pLimit) > 0 {
				*warnings = append(*warnings,
					fmt.Sprintf("[%s] CPU limit %s exceeds persona limit %s",
						container.Name, cLimit.String(), pLimit.String()))
			}
		}
		if persona.Spec.Resources.Limits.Memory != "" {
			pLimit := resource.MustParse(persona.Spec.Resources.Limits.Memory)
			cLimit := container.Resources.Limits[corev1.ResourceMemory]
			if !cLimit.IsZero() && cLimit.Cmp(pLimit) > 0 {
				*warnings = append(*warnings,
					fmt.Sprintf("[%s] Memory limit %s exceeds persona limit %s",
						container.Name, cLimit.String(), pLimit.String()))
			}
		}
	}
}

func validateWebhookReplicas(persona *dorguv1.ApplicationPersona, deploy *appsv1.Deployment, warnings, errors *[]string) {
	if persona.Spec.Scaling == nil {
		return
	}

	replicas := int32(1)
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}

	if persona.Spec.Scaling.MinReplicas != nil && replicas < *persona.Spec.Scaling.MinReplicas {
		*errors = append(*errors,
			fmt.Sprintf("replicas (%d) below persona minimum (%d)", replicas, *persona.Spec.Scaling.MinReplicas))
	}
	if persona.Spec.Scaling.MaxReplicas != nil && replicas > *persona.Spec.Scaling.MaxReplicas {
		*warnings = append(*warnings,
			fmt.Sprintf("replicas (%d) above persona maximum (%d)", replicas, *persona.Spec.Scaling.MaxReplicas))
	}
}

func validateWebhookSecurity(persona *dorguv1.ApplicationPersona, deploy *appsv1.Deployment, errors *[]string) {
	if persona.Spec.Policies == nil || persona.Spec.Policies.Security == nil {
		return
	}
	sec := persona.Spec.Policies.Security

	podSec := deploy.Spec.Template.Spec.SecurityContext
	if sec.RunAsNonRoot != nil && *sec.RunAsNonRoot {
		if podSec == nil || podSec.RunAsNonRoot == nil || !*podSec.RunAsNonRoot {
			*errors = append(*errors, "persona requires runAsNonRoot but Deployment does not enforce it")
		}
	}

	for _, container := range deploy.Spec.Template.Spec.Containers {
		cSec := container.SecurityContext
		if sec.AllowPrivilegeEscalation != nil && !*sec.AllowPrivilegeEscalation {
			if cSec != nil && cSec.AllowPrivilegeEscalation != nil && *cSec.AllowPrivilegeEscalation {
				*errors = append(*errors,
					fmt.Sprintf("[%s] privilege escalation enabled but persona forbids it", container.Name))
			}
		}
	}
}

func validateWebhookProbes(persona *dorguv1.ApplicationPersona, deploy *appsv1.Deployment, warnings *[]string) {
	if persona.Spec.Health == nil {
		return
	}

	for _, container := range deploy.Spec.Template.Spec.Containers {
		if persona.Spec.Health.LivenessPath != "" {
			if container.LivenessProbe == nil {
				*warnings = append(*warnings,
					fmt.Sprintf("[%s] no liveness probe; persona expects path %s",
						container.Name, persona.Spec.Health.LivenessPath))
			}
		}
		if persona.Spec.Health.ReadinessPath != "" {
			if container.ReadinessProbe == nil {
				*warnings = append(*warnings,
					fmt.Sprintf("[%s] no readiness probe; persona expects path %s",
						container.Name, persona.Spec.Health.ReadinessPath))
			}
		}
	}
}
