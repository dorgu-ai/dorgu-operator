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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// deriveHealthFromDeployment maps deployment conditions to a HealthStatus.
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
		hs.Status = healthStatusHealthy
		hs.Message = fmt.Sprintf("%d/%d replicas ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	case available && deploy.Status.ReadyReplicas > 0:
		hs.Status = healthStatusDegraded
		hs.Message = fmt.Sprintf("%d/%d replicas ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	case progressing:
		hs.Status = healthStatusUnknown
		hs.Message = "Deployment is progressing"
	default:
		hs.Status = healthStatusUnhealthy
		hs.Message = fmt.Sprintf("%d/%d replicas ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	}

	return hs
}

// getPodFailures queries pods owned by the deployment and extracts failure reasons.
func (r *ApplicationPersonaReconciler) getPodFailures(ctx context.Context, deploy *appsv1.Deployment) ([]dorguv1.PodFailure, error) {
	// Get the pod selector from the deployment
	selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("invalid deployment selector: %w", err)
	}

	// List pods matching the deployment's selector
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, &client.ListOptions{
		Namespace:     deploy.Namespace,
		LabelSelector: selector,
	}); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	var failures []dorguv1.PodFailure

	for _, pod := range pods.Items {
		// Check init container statuses
		for _, cs := range pod.Status.InitContainerStatuses {
			if failure := extractContainerFailure(pod.Name, cs); failure != nil {
				failures = append(failures, *failure)
			}
		}

		// Check container statuses
		for _, cs := range pod.Status.ContainerStatuses {
			if failure := extractContainerFailure(pod.Name, cs); failure != nil {
				failures = append(failures, *failure)
			}
		}
	}

	return failures, nil
}

// extractContainerFailure extracts failure information from a container status.
func extractContainerFailure(podName string, cs corev1.ContainerStatus) *dorguv1.PodFailure {
	// Check if container is waiting with a failure reason
	if cs.State.Waiting != nil {
		reason := cs.State.Waiting.Reason
		if isFailureReason(reason) {
			return &dorguv1.PodFailure{
				PodName:   podName,
				Container: cs.Name,
				Reason:    reason,
				Message:   cs.State.Waiting.Message,
			}
		}
	}

	// Check if container terminated with an error
	if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
		reason := cs.State.Terminated.Reason
		if reason == "" {
			reason = fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode)
		}
		return &dorguv1.PodFailure{
			PodName:   podName,
			Container: cs.Name,
			Reason:    reason,
			Message:   cs.State.Terminated.Message,
		}
	}

	// Check last termination state for crash loops
	if cs.LastTerminationState.Terminated != nil && cs.RestartCount > 0 {
		return &dorguv1.PodFailure{
			PodName:   podName,
			Container: cs.Name,
			Reason:    "CrashLoopBackOff",
			Message:   fmt.Sprintf("Restarted %d times; last exit code: %d", cs.RestartCount, cs.LastTerminationState.Terminated.ExitCode),
		}
	}

	return nil
}

// isFailureReason returns true if the waiting reason indicates a failure.
func isFailureReason(reason string) bool {
	failureReasons := map[string]bool{
		"CrashLoopBackOff":           true,
		"ImagePullBackOff":           true,
		"ErrImagePull":               true,
		"CreateContainerConfigError": true,
		"CreateContainerError":       true,
		"InvalidImageName":           true,
		"RunContainerError":          true,
		"ContainerCannotRun":         true,
		"OOMKilled":                  true,
	}
	return failureReasons[reason]
}
