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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

const (
	requeueInterval = 60 * time.Second

	// Condition types
	conditionTypeReady     = "Ready"
	conditionTypeValidated = "Validated"

	// Phase values for ApplicationPersona status
	phasePending  = "Pending"
	phaseActive   = "Active"
	phaseDegraded = "Degraded"
	phaseFailed   = "Failed"

	// Health status values
	healthStatusHealthy   = "Healthy"
	healthStatusDegraded  = "Degraded"
	healthStatusUnhealthy = "Unhealthy"
	healthStatusUnknown   = "Unknown"
)

// ApplicationPersonaReconciler reconciles an ApplicationPersona object.
type ApplicationPersonaReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	PrometheusURL string // Optional Prometheus URL for metrics baseline
}

// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas/finalizers,verbs=update
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

	// 2. Find matching Deployments by label (try app.kubernetes.io/name first, fall back to app)
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

	// Fallback: try common "app" label if no match with recommended label
	if len(deployments.Items) == 0 {
		fallbackSelector := labels.SelectorFromSet(labels.Set{
			"app": persona.Spec.Name,
		})
		if err := r.List(ctx, deployments, &client.ListOptions{
			Namespace:     req.Namespace,
			LabelSelector: fallbackSelector,
		}); err != nil {
			log.Error(err, "Failed to list deployments with fallback label")
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()

	// 3. No matching Deployment found -> Pending
	if len(deployments.Items) == 0 {
		persona.Status.Phase = phasePending
		persona.Status.LastUpdated = &now
		persona.Status.Health = &dorguv1.HealthStatus{
			Status:    healthStatusUnknown,
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

	// 6.5. Query pod failures if health is not Healthy
	if persona.Status.Health.Status != healthStatusHealthy {
		podFailures, err := r.getPodFailures(ctx, &deploy)
		if err != nil {
			log.V(1).Info("Could not get pod failures", "error", err.Error())
		} else if len(podFailures) > 0 {
			persona.Status.Health.PodFailures = podFailures
			// Enhance the health message with failure reasons
			reasons := make(map[string]bool)
			for _, pf := range podFailures {
				reasons[pf.Reason] = true
			}
			var reasonList []string
			for reason := range reasons {
				reasonList = append(reasonList, reason)
			}
			if len(reasonList) > 0 {
				persona.Status.Health.Message = fmt.Sprintf("%s; failures: %s",
					persona.Status.Health.Message, strings.Join(reasonList, ", "))
			}
		}
	}

	// 6.7. Detect OOM incidents and create IncidentMemory + RemediationAction.
	if err := r.detectAndRecordOOMIncidents(ctx, persona, &deploy); err != nil {
		log.V(1).Info("OOM incident detection failed", "error", err.Error())
	}

	// 7. Update deployment tracking
	image := ""
	if len(deploy.Spec.Template.Spec.Containers) > 0 {
		image = deploy.Spec.Template.Spec.Containers[0].Image
	}
	persona.Status.Deployments = &dorguv1.DeploymentTracking{
		Current: image,
	}

	// 7.5. Query Prometheus for resource baseline (if configured)
	if r.PrometheusURL != "" {
		promClient := metrics.NewPrometheusClient(r.PrometheusURL)
		if baseline, err := promClient.GetResourceBaseline(ctx, req.Namespace, persona.Spec.Name); err == nil {
			if persona.Status.Learned == nil {
				persona.Status.Learned = &dorguv1.LearnedPatterns{}
			}
			persona.Status.Learned.ResourceBaseline = baseline
			log.V(1).Info("Updated resource baseline from Prometheus",
				"avgCPU", baseline.AvgCPU,
				"avgMemory", baseline.AvgMemory)
		} else {
			log.V(1).Info("Could not get Prometheus metrics", "error", err.Error())
		}
	}

	// 8. Set phase
	if persona.Status.Health.Status == healthStatusHealthy && !hasErrors {
		persona.Status.Phase = phaseActive
	} else if hasErrors {
		persona.Status.Phase = phaseDegraded
	} else if persona.Status.Health.Status == healthStatusUnhealthy {
		persona.Status.Phase = phaseFailed
	} else {
		persona.Status.Phase = phaseDegraded
	}
	persona.Status.LastUpdated = &now

	// 9. Set conditions
	if persona.Status.Phase == phaseActive {
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

	// Check both label keys (recommended + common)
	appName := deploy.Labels["app.kubernetes.io/name"]
	if appName == "" {
		appName = deploy.Labels["app"]
	}
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
