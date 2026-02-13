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
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	requeueInterval = 60 * time.Second

	// Condition types
	conditionTypeReady     = "Ready"
	conditionTypeValidated = "Validated"
)

// ApplicationPersonaReconciler reconciles an ApplicationPersona object.
type ApplicationPersonaReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=dorgu.dorgu.io,resources=applicationpersonas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dorgu.dorgu.io,resources=applicationpersonas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.dorgu.io,resources=applicationpersonas/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list

// Reconcile validates Deployments against ApplicationPersona constraints
// and updates the persona status accordingly.
func (r *ApplicationPersonaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the ApplicationPersona
	persona := &dorguv1.ApplicationPersona{}
	if err := r.Get(ctx, req.NamespacedName, persona); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil // Deleted, nothing to do
		}
		return ctrl.Result{}, err
	}

	log.Info("Reconciling ApplicationPersona", "name", persona.Spec.Name)

	// 2. Find matching Deployments by label
	deployments := &appsv1.DeploymentList{}
	selector := labels.SelectorFromSet(labels.Set{
		"app.kubernetes.io/name": persona.Spec.Name,
	})
	if err := r.List(ctx, deployments, &client.ListOptions{
		Namespace:     req.Namespace,
		LabelSelector: selector,
	}); err != nil {
		log.Error(err, "Failed to list deployments")
		return ctrl.Result{}, err
	}

	now := metav1.Now()

	// 3. No matching Deployment found -> Pending
	if len(deployments.Items) == 0 {
		persona.Status.Phase = "Pending"
		persona.Status.LastUpdated = &now
		persona.Status.Health = &dorguv1.HealthStatus{
			Status:    "Unknown",
			LastCheck: &now,
			Message:   "No matching Deployment found",
		}
		persona.Status.Validation = &dorguv1.ValidationStatus{
			Passed:      true,
			LastChecked: &now,
		}

		setCondition(&persona.Status.Conditions, conditionTypeReady, metav1.ConditionFalse,
			"NoDeployment", "No Deployment with label app.kubernetes.io/name="+persona.Spec.Name)
		setCondition(&persona.Status.Conditions, conditionTypeValidated, metav1.ConditionTrue,
			"Skipped", "No Deployment to validate")

		if err := r.Status().Update(ctx, persona); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// 4. Deployment found -> Validate
	deploy := deployments.Items[0] // Use first match
	var issues []dorguv1.ValidationIssue

	issues = append(issues, validateResources(persona, &deploy)...)
	issues = append(issues, validateReplicas(persona, &deploy)...)
	issues = append(issues, validateHealthProbes(persona, &deploy)...)
	issues = append(issues, validateSecurityContext(persona, &deploy)...)

	// 5. Update validation status
	hasErrors := false
	for _, issue := range issues {
		if issue.Severity == "error" {
			hasErrors = true
			break
		}
	}
	persona.Status.Validation = &dorguv1.ValidationStatus{
		Passed:      !hasErrors,
		LastChecked: &now,
		Issues:      issues,
	}

	// 6. Update health from deployment conditions
	persona.Status.Health = deriveHealthFromDeployment(&deploy, &now)

	// 7. Update deployment tracking
	image := ""
	if len(deploy.Spec.Template.Spec.Containers) > 0 {
		image = deploy.Spec.Template.Spec.Containers[0].Image
	}
	persona.Status.Deployments = &dorguv1.DeploymentTracking{
		Current: image,
	}

	// 8. Set phase
	if persona.Status.Health.Status == "Healthy" && !hasErrors {
		persona.Status.Phase = "Active"
	} else if hasErrors {
		persona.Status.Phase = "Degraded"
	} else if persona.Status.Health.Status == "Unhealthy" {
		persona.Status.Phase = "Failed"
	} else {
		persona.Status.Phase = "Degraded"
	}
	persona.Status.LastUpdated = &now

	// 9. Set conditions
	if persona.Status.Phase == "Active" {
		setCondition(&persona.Status.Conditions, conditionTypeReady, metav1.ConditionTrue,
			"Active", "Deployment is healthy and passes all validations")
	} else {
		setCondition(&persona.Status.Conditions, conditionTypeReady, metav1.ConditionFalse,
			"Issues", fmt.Sprintf("Phase: %s", persona.Status.Phase))
	}
	if hasErrors {
		setCondition(&persona.Status.Conditions, conditionTypeValidated, metav1.ConditionFalse,
			"ValidationFailed", fmt.Sprintf("%d validation error(s) found", countSeverity(issues, "error")))
	} else {
		setCondition(&persona.Status.Conditions, conditionTypeValidated, metav1.ConditionTrue,
			"Passed", "All validations passed")
	}

	// 10. Persist status
	if err := r.Status().Update(ctx, persona); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Reconciliation complete",
		"phase", persona.Status.Phase,
		"validationPassed", persona.Status.Validation.Passed,
		"issues", len(issues))

	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApplicationPersonaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dorguv1.ApplicationPersona{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(
			r.deploymentToPersona,
		)).
		Named("applicationpersona").
		Complete(r)
}

// deploymentToPersona maps a Deployment event to the matching ApplicationPersona(s).
func (r *ApplicationPersonaReconciler) deploymentToPersona(ctx context.Context, obj client.Object) []reconcile.Request {
	deploy, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}

	appName := deploy.Labels["app.kubernetes.io/name"]
	if appName == "" {
		return nil
	}

	// Find personas in the same namespace with matching spec.name
	personas := &dorguv1.ApplicationPersonaList{}
	if err := r.List(ctx, personas, &client.ListOptions{Namespace: deploy.Namespace}); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, p := range personas.Items {
		if p.Spec.Name == appName {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      p.Name,
					Namespace: p.Namespace,
				},
			})
		}
	}
	return requests
}

// ============================================================================
// Validation helpers
// ============================================================================

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

// ============================================================================
// Status helpers
// ============================================================================

func deriveHealthFromDeployment(deploy *appsv1.Deployment, now *metav1.Time) *dorguv1.HealthStatus {
	hs := &dorguv1.HealthStatus{
		LastCheck: now,
	}

	available := false
	progressing := false
	for _, cond := range deploy.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionTrue {
			available = true
		}
		if cond.Type == appsv1.DeploymentProgressing && cond.Status == corev1.ConditionTrue {
			progressing = true
		}
	}

	switch {
	case available && deploy.Status.ReadyReplicas == deploy.Status.Replicas:
		hs.Status = "Healthy"
		hs.Message = fmt.Sprintf("%d/%d replicas ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	case available && deploy.Status.ReadyReplicas > 0:
		hs.Status = "Degraded"
		hs.Message = fmt.Sprintf("%d/%d replicas ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	case progressing:
		hs.Status = "Unknown"
		hs.Message = "Deployment is progressing"
	default:
		hs.Status = "Unhealthy"
		hs.Message = fmt.Sprintf("%d/%d replicas ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	}

	return hs
}

func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range *conditions {
		if c.Type == condType {
			if c.Status != status || c.Reason != reason {
				(*conditions)[i].Status = status
				(*conditions)[i].Reason = reason
				(*conditions)[i].Message = message
				(*conditions)[i].LastTransitionTime = now
			}
			return
		}
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

func countSeverity(issues []dorguv1.ValidationIssue, severity string) int {
	count := 0
	for _, i := range issues {
		if i.Severity == severity {
			count++
		}
	}
	return count
}

// parseCPUMillis is used during resource validation. Keeping it here avoids
// an external dependency but the helpers are intentionally minimal.
func parseCPUMillis(s string) int64 {
	if strings.HasSuffix(s, "m") {
		v, _ := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		return v
	}
	v, _ := strconv.ParseFloat(s, 64)
	return int64(v * 1000)
}
