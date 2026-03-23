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
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ============================================================================
// deriveHealthFromDeployment tests
// ============================================================================

func TestDeriveHealth_Healthy(t *testing.T) {
	deploy := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Replicas:      3,
			ReadyReplicas: 3,
			Conditions: []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: corev1.ConditionTrue,
				},
				{
					Type:   appsv1.DeploymentProgressing,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	now := metav1.Now()

	health := deriveHealthFromDeployment(deploy, &now)
	assert.Equal(t, healthStatusHealthy, health.Status)
	assert.Contains(t, health.Message, "3/3")
}

func TestDeriveHealth_Degraded(t *testing.T) {
	deploy := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Replicas:      3,
			ReadyReplicas: 2,
			Conditions: []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	now := metav1.Now()

	health := deriveHealthFromDeployment(deploy, &now)
	assert.Equal(t, healthStatusDegraded, health.Status)
	assert.Contains(t, health.Message, "2/3")
}

func TestDeriveHealth_Progressing(t *testing.T) {
	deploy := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Replicas:      3,
			ReadyReplicas: 0,
			Conditions: []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentProgressing,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	now := metav1.Now()

	health := deriveHealthFromDeployment(deploy, &now)
	assert.Equal(t, healthStatusUnknown, health.Status)
	assert.Contains(t, health.Message, "progressing")
}

func TestDeriveHealth_Unhealthy(t *testing.T) {
	deploy := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Replicas:      3,
			ReadyReplicas: 0,
			Conditions: []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}
	now := metav1.Now()

	health := deriveHealthFromDeployment(deploy, &now)
	assert.Equal(t, healthStatusUnhealthy, health.Status)
}

// ============================================================================
// extractContainerFailure tests
// ============================================================================

func TestExtractContainerFailure_Waiting(t *testing.T) {
	cs := corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "ImagePullBackOff",
				Message: "Back-off pulling image",
			},
		},
	}

	failure := extractContainerFailure("test-pod", cs)
	assert.NotNil(t, failure)
	assert.Equal(t, "test-pod", failure.PodName)
	assert.Equal(t, "app", failure.Container)
	assert.Equal(t, "ImagePullBackOff", failure.Reason)
	assert.Equal(t, "Back-off pulling image", failure.Message)
}

func TestExtractContainerFailure_Terminated(t *testing.T) {
	cs := corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
				Reason:   "Error",
				Message:  "Container failed",
			},
		},
	}

	failure := extractContainerFailure("test-pod", cs)
	assert.NotNil(t, failure)
	assert.Equal(t, "test-pod", failure.PodName)
	assert.Equal(t, "app", failure.Container)
	assert.Equal(t, "Error", failure.Reason)
}

func TestExtractContainerFailure_TerminatedNoReason(t *testing.T) {
	cs := corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 137,
				Reason:   "",
			},
		},
	}

	failure := extractContainerFailure("test-pod", cs)
	assert.NotNil(t, failure)
	assert.Equal(t, "ExitCode:137", failure.Reason)
}

func TestExtractContainerFailure_CrashLoop(t *testing.T) {
	cs := corev1.ContainerStatus{
		Name:         "app",
		RestartCount: 5,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
			},
		},
	}

	failure := extractContainerFailure("test-pod", cs)
	assert.NotNil(t, failure)
	assert.Equal(t, "CrashLoopBackOff", failure.Reason)
	assert.Contains(t, failure.Message, "Restarted 5 times")
}

func TestExtractContainerFailure_Running(t *testing.T) {
	cs := corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{},
		},
	}

	failure := extractContainerFailure("test-pod", cs)
	assert.Nil(t, failure)
}

func TestExtractContainerFailure_WaitingNonFailure(t *testing.T) {
	cs := corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason: "ContainerCreating",
			},
		},
	}

	failure := extractContainerFailure("test-pod", cs)
	assert.Nil(t, failure)
}

// ============================================================================
// isFailureReason tests
// ============================================================================

func TestIsFailureReason(t *testing.T) {
	failureReasons := []string{
		"CrashLoopBackOff",
		"ImagePullBackOff",
		"ErrImagePull",
		"CreateContainerConfigError",
		"CreateContainerError",
		"InvalidImageName",
		"RunContainerError",
		"ContainerCannotRun",
		"OOMKilled",
	}

	for _, reason := range failureReasons {
		t.Run(reason, func(t *testing.T) {
			assert.True(t, isFailureReason(reason))
		})
	}
}

func TestIsFailureReason_NonFailure(t *testing.T) {
	nonFailureReasons := []string{
		"ContainerCreating",
		"PodInitializing",
		"Running",
		"Completed",
		"",
	}

	for _, reason := range nonFailureReasons {
		t.Run(reason, func(t *testing.T) {
			assert.False(t, isFailureReason(reason))
		})
	}
}
