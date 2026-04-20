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
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas,verbs=get;list;watch;create;update;patch

// AppDiscoveryReconciler watches Deployments and StatefulSets and creates
// skeleton ApplicationPersonas for workloads not yet managed by Dorgu.
type AppDiscoveryReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Logger            logr.Logger
	ExcludeNamespaces map[string]bool
}

func (r *AppDiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.WithValues("namespace", req.Namespace, "name", req.Name)

	// Gate: skip system and excluded namespaces.
	if r.ExcludeNamespaces[req.Namespace] {
		return ctrl.Result{}, nil
	}

	// Try Deployment first, then StatefulSet.
	var workload client.Object
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deploy); err == nil {
		workload = deploy
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	} else {
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, req.NamespacedName, sts); err != nil {
			if errors.IsNotFound(err) {
				return r.handleWorkloadDeleted(ctx, log, req)
			}
			return ctrl.Result{}, err
		}
		workload = sts
	}

	// Check if a matching ApplicationPersona already exists.
	existing := &dorguv1.ApplicationPersona{}
	err := r.Get(ctx, req.NamespacedName, existing)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if err == nil {
		// Persona exists — only sync resources if it's auto-discovered.
		return r.syncIfAutoDiscovered(ctx, log, existing, workload)
	}

	// No persona yet — create skeleton.
	persona := r.buildSkeletonPersona(ctx, workload)
	if createErr := r.Create(ctx, persona); createErr != nil {
		if errors.IsAlreadyExists(createErr) {
			return ctrl.Result{}, nil // race: another replica created it
		}
		return ctrl.Result{}, createErr
	}

	log.Info("auto-discovered workload, created skeleton persona",
		"kind", persona.Labels[LabelWorkloadKind])
	return ctrl.Result{}, nil
}

// handleWorkloadDeleted annotates the auto-discovered persona when its workload is gone.
// Does NOT delete the persona — user may have added data to it.
func (r *AppDiscoveryReconciler) handleWorkloadDeleted(
	ctx context.Context, log logr.Logger, req ctrl.Request,
) (ctrl.Result, error) {
	persona := &dorguv1.ApplicationPersona{}
	if err := r.Get(ctx, req.NamespacedName, persona); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if persona.Labels[LabelSource] != LabelSourceAutoDiscovered {
		return ctrl.Result{}, nil // user-defined persona — don't touch
	}
	base := persona.DeepCopy()
	if persona.Annotations == nil {
		persona.Annotations = map[string]string{}
	}
	persona.Annotations[LabelWorkloadDeleted] = "true"
	if err := r.Patch(ctx, persona, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Status subresource must be updated separately.
	statusBase := persona.DeepCopy()
	persona.Status.Phase = dorguv1.PhaseUnmanaged
	if err := r.Status().Patch(ctx, persona, client.MergeFrom(statusBase)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("workload deleted — annotated persona", "persona", persona.Name)
	return ctrl.Result{}, nil
}

// syncIfAutoDiscovered updates resource fields on auto-discovered personas when the
// underlying workload's resource requests/limits change. User-defined personas are never touched.
func (r *AppDiscoveryReconciler) syncIfAutoDiscovered(
	ctx context.Context, log logr.Logger,
	persona *dorguv1.ApplicationPersona, workload client.Object,
) (ctrl.Result, error) {
	if persona.Labels[LabelSource] != LabelSourceAutoDiscovered {
		return ctrl.Result{}, nil
	}
	updated := r.buildSkeletonPersona(ctx, workload)
	// Only patch if resources actually changed.
	if resourcesEqual(persona.Spec.Resources, updated.Spec.Resources) {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(persona.DeepCopy())
	persona.Spec.Resources = updated.Spec.Resources
	persona.Spec.Scaling = updated.Spec.Scaling
	if err := r.Patch(ctx, persona, patch); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("synced resources on auto-discovered persona")
	return ctrl.Result{}, nil
}

// buildSkeletonPersona creates a skeleton ApplicationPersona for the given workload.
func (r *AppDiscoveryReconciler) buildSkeletonPersona(ctx context.Context, workload client.Object) *dorguv1.ApplicationPersona {
	managed := false
	kind := "Deployment"
	if _, ok := workload.(*appsv1.StatefulSet); ok {
		kind = "StatefulSet"
	}

	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workload.GetName(),
			Namespace: workload.GetNamespace(),
			Labels: map[string]string{
				LabelSource:       LabelSourceAutoDiscovered,
				LabelWorkloadKind: kind,
				LabelWorkloadName: workload.GetName(),
			},
			Annotations: map[string]string{
				AnnotationDiscoveryTimestamp: time.Now().UTC().Format(time.RFC3339),
				AnnotationWorkloadImage:      extractImage(workload),
			},
		},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name:      workload.GetName(),
			Type:      "worker",
			Managed:   &managed,
			Resources: extractResources(workload),
			Scaling:   r.extractScaling(ctx, workload),
			Health:    extractHealthProbe(workload),
		},
	}
}

// extractImage returns the first container's image, or empty string.
func extractImage(workload client.Object) string {
	containers := extractContainers(workload)
	if len(containers) == 0 {
		return ""
	}
	return containers[0].Image
}

// extractResources returns ResourceConstraints from containers[0], or nil.
func extractResources(workload client.Object) *dorguv1.ResourceConstraints {
	containers := extractContainers(workload)
	if len(containers) == 0 {
		return nil
	}
	res := containers[0].Resources
	result := &dorguv1.ResourceConstraints{}
	if res.Requests != nil {
		result.Requests = &dorguv1.ResourceValues{}
		if cpu := res.Requests.Cpu(); cpu != nil && !cpu.IsZero() {
			result.Requests.CPU = cpu.String()
		}
		if mem := res.Requests.Memory(); mem != nil && !mem.IsZero() {
			result.Requests.Memory = mem.String()
		}
	}
	if res.Limits != nil {
		result.Limits = &dorguv1.ResourceValues{}
		if cpu := res.Limits.Cpu(); cpu != nil && !cpu.IsZero() {
			result.Limits.CPU = cpu.String()
		}
		if mem := res.Limits.Memory(); mem != nil && !mem.IsZero() {
			result.Limits.Memory = mem.String()
		}
	}
	if result.Requests == nil && result.Limits == nil {
		return nil
	}
	return result
}

// extractScaling returns scaling config derived from HPA if found, else from workload replicas.
func (r *AppDiscoveryReconciler) extractScaling(ctx context.Context, workload client.Object) *dorguv1.ScalingSpec {
	// Try to find a matching HPA.
	hpaList := &autoscalingv2.HorizontalPodAutoscalerList{}
	if err := r.List(ctx, hpaList, client.InNamespace(workload.GetNamespace())); err != nil {
		r.Logger.Error(err, "failed to list HPAs, falling back to replica count",
			"namespace", workload.GetNamespace())
	} else {
		kind := "Deployment"
		if _, ok := workload.(*appsv1.StatefulSet); ok {
			kind = "StatefulSet"
		}
		for _, hpa := range hpaList.Items {
			ref := hpa.Spec.ScaleTargetRef
			if ref.Kind == kind && ref.Name == workload.GetName() && ref.APIVersion == "apps/v1" {
				scaling := &dorguv1.ScalingSpec{
					MinReplicas: hpa.Spec.MinReplicas,
					MaxReplicas: &hpa.Spec.MaxReplicas,
				}
				for _, metric := range hpa.Spec.Metrics {
					if metric.Type == autoscalingv2.ResourceMetricSourceType && metric.Resource != nil {
						if metric.Resource.Name == corev1.ResourceCPU &&
							metric.Resource.Target.AverageUtilization != nil {
							val := *metric.Resource.Target.AverageUtilization
							scaling.TargetCPU = &val
						}
						if metric.Resource.Name == corev1.ResourceMemory &&
							metric.Resource.Target.AverageUtilization != nil {
							val := *metric.Resource.Target.AverageUtilization
							scaling.TargetMemory = &val
						}
					}
				}
				return scaling
			}
		}
	}

	// Fall back to workload replicas.
	switch w := workload.(type) {
	case *appsv1.Deployment:
		if w.Spec.Replicas != nil {
			replicas := *w.Spec.Replicas
			return &dorguv1.ScalingSpec{
				MinReplicas: &replicas,
				MaxReplicas: &replicas,
			}
		}
	case *appsv1.StatefulSet:
		if w.Spec.Replicas != nil {
			replicas := *w.Spec.Replicas
			return &dorguv1.ScalingSpec{
				MinReplicas: &replicas,
				MaxReplicas: &replicas,
			}
		}
	}
	return nil
}

// extractHealthProbe returns health config from containers[0] probes, or nil.
func extractHealthProbe(workload client.Object) *dorguv1.HealthSpec {
	containers := extractContainers(workload)
	if len(containers) == 0 {
		return nil
	}
	c := containers[0]
	health := &dorguv1.HealthSpec{}
	found := false

	if c.ReadinessProbe != nil && c.ReadinessProbe.HTTPGet != nil {
		health.ReadinessPath = c.ReadinessProbe.HTTPGet.Path
		port := c.ReadinessProbe.HTTPGet.Port.IntVal
		if port > 0 {
			health.Port = &port
		}
		found = true
	}
	if c.LivenessProbe != nil && c.LivenessProbe.HTTPGet != nil {
		health.LivenessPath = c.LivenessProbe.HTTPGet.Path
		if health.Port == nil {
			port := c.LivenessProbe.HTTPGet.Port.IntVal
			if port > 0 {
				health.Port = &port
			}
		}
		found = true
	}

	if !found {
		return nil
	}
	return health
}

// extractContainers returns the containers from a Deployment or StatefulSet PodTemplateSpec.
func extractContainers(workload client.Object) []corev1.Container {
	switch w := workload.(type) {
	case *appsv1.Deployment:
		return w.Spec.Template.Spec.Containers
	case *appsv1.StatefulSet:
		return w.Spec.Template.Spec.Containers
	}
	return nil
}

// resourcesEqual returns true if r1 and r2 have the same CPU/memory requests and limits.
func resourcesEqual(r1, r2 *dorguv1.ResourceConstraints) bool {
	if r1 == nil && r2 == nil {
		return true
	}
	if r1 == nil || r2 == nil {
		return false
	}
	return resourceValuesEqual(r1.Requests, r2.Requests) &&
		resourceValuesEqual(r1.Limits, r2.Limits)
}

func resourceValuesEqual(v1, v2 *dorguv1.ResourceValues) bool {
	if v1 == nil && v2 == nil {
		return true
	}
	if v1 == nil || v2 == nil {
		return false
	}
	return v1.CPU == v2.CPU && v1.Memory == v2.Memory
}

func (r *AppDiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		Watches(
			&appsv1.StatefulSet{},
			&handler.EnqueueRequestForObject{},
		).
		Complete(r)
}
