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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ============================================================================
// validateResources tests
// ============================================================================

func TestValidateResources_NoSpec(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name:      "test-app",
			Resources: nil,
		},
	}
	deploy := &appsv1.Deployment{}

	issues := validateResources(persona, deploy)
	assert.Empty(t, issues)
}

func TestValidateResources_CPUExceedsLimit(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{
					CPU: "500m",
				},
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("100m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1000m"),
								},
							},
						},
					},
				},
			},
		},
	}

	issues := validateResources(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "warning", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "CPU limit")
	assert.Contains(t, issues[0].Message, "exceeds")
}

func TestValidateResources_MemoryExceedsLimit(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{
					Memory: "512Mi",
				},
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		},
	}

	issues := validateResources(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "warning", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "memory limit")
	assert.Contains(t, issues[0].Message, "exceeds")
}

func TestValidateResources_NoRequests(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{
					CPU:    "500m",
					Memory: "512Mi",
				},
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:      "app",
							Resources: corev1.ResourceRequirements{},
						},
					},
				},
			},
		},
	}

	issues := validateResources(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "warning", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "No resource requests")
}

func TestValidateResources_AllValid(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{
					CPU:    "1000m",
					Memory: "1Gi",
				},
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	issues := validateResources(persona, deploy)
	assert.Empty(t, issues)
}

// ============================================================================
// validateReplicas tests
// ============================================================================

func TestValidateReplicas_NoSpec(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name:    "test-app",
			Scaling: nil,
		},
	}
	deploy := &appsv1.Deployment{}

	issues := validateReplicas(persona, deploy)
	assert.Empty(t, issues)
}

func TestValidateReplicas_BelowMin(t *testing.T) {
	minReplicas := int32(3)
	replicas := int32(1)
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Scaling: &dorguv1.ScalingSpec{
				MinReplicas: &minReplicas,
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
	}

	issues := validateReplicas(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "error", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "below persona minimum")
}

func TestValidateReplicas_AboveMax(t *testing.T) {
	maxReplicas := int32(5)
	replicas := int32(10)
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Scaling: &dorguv1.ScalingSpec{
				MaxReplicas: &maxReplicas,
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
	}

	issues := validateReplicas(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "warning", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "above persona maximum")
}

func TestValidateReplicas_WithinRange(t *testing.T) {
	minReplicas := int32(2)
	maxReplicas := int32(10)
	replicas := int32(5)
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Scaling: &dorguv1.ScalingSpec{
				MinReplicas: &minReplicas,
				MaxReplicas: &maxReplicas,
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
	}

	issues := validateReplicas(persona, deploy)
	assert.Empty(t, issues)
}

func TestValidateReplicas_DefaultReplicas(t *testing.T) {
	minReplicas := int32(2)
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Scaling: &dorguv1.ScalingSpec{
				MinReplicas: &minReplicas,
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Replicas: nil,
		},
	}

	issues := validateReplicas(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "error", issues[0].Severity)
}

// ============================================================================
// validateHealthProbes tests
// ============================================================================

func TestValidateHealthProbes_NoSpec(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name:   "test-app",
			Health: nil,
		},
	}
	deploy := &appsv1.Deployment{}

	issues := validateHealthProbes(persona, deploy)
	assert.Empty(t, issues)
}

func TestValidateHealthProbes_MissingLiveness(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Health: &dorguv1.HealthSpec{
				LivenessPath: "/health",
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:          "app",
							LivenessProbe: nil,
						},
					},
				},
			},
		},
	}

	issues := validateHealthProbes(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "warning", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "livenessPath")
}

func TestValidateHealthProbes_PathMismatch(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Health: &dorguv1.HealthSpec{
				LivenessPath: "/health",
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(8080),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	issues := validateHealthProbes(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "info", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "differs from persona")
}

func TestValidateHealthProbes_MissingReadiness(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Health: &dorguv1.HealthSpec{
				ReadinessPath: "/ready",
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:           "app",
							ReadinessProbe: nil,
						},
					},
				},
			},
		},
	}

	issues := validateHealthProbes(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "warning", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "readinessPath")
}

func TestValidateHealthProbes_AllValid(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Health: &dorguv1.HealthSpec{
				LivenessPath:  "/health",
				ReadinessPath: "/ready",
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/health",
										Port: intstr.FromInt(8080),
									},
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/ready",
										Port: intstr.FromInt(8080),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	issues := validateHealthProbes(persona, deploy)
	assert.Empty(t, issues)
}

// ============================================================================
// validateSecurityContext tests
// ============================================================================

func TestValidateSecurityContext_NoSpec(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name:     "test-app",
			Policies: nil,
		},
	}
	deploy := &appsv1.Deployment{}

	issues := validateSecurityContext(persona, deploy)
	assert.Empty(t, issues)
}

func TestValidateSecurityContext_RunAsNonRootMissing(t *testing.T) {
	runAsNonRoot := true
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Policies: &dorguv1.PoliciesSpec{
				Security: &dorguv1.SecurityPolicy{
					RunAsNonRoot: &runAsNonRoot,
				},
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					SecurityContext: nil,
					Containers: []corev1.Container{
						{Name: "app"},
					},
				},
			},
		},
	}

	issues := validateSecurityContext(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "error", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "runAsNonRoot")
}

func TestValidateSecurityContext_ReadOnlyFSMissing(t *testing.T) {
	readOnlyFS := true
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Policies: &dorguv1.PoliciesSpec{
				Security: &dorguv1.SecurityPolicy{
					ReadOnlyRootFilesystem: &readOnlyFS,
				},
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "app",
							SecurityContext: nil,
						},
					},
				},
			},
		},
	}

	issues := validateSecurityContext(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "warning", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "readOnlyRootFilesystem")
}

func TestValidateSecurityContext_PrivilegeEscalation(t *testing.T) {
	allowPrivEsc := false
	containerAllowPrivEsc := true
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Policies: &dorguv1.PoliciesSpec{
				Security: &dorguv1.SecurityPolicy{
					AllowPrivilegeEscalation: &allowPrivEsc,
				},
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: &containerAllowPrivEsc,
							},
						},
					},
				},
			},
		},
	}

	issues := validateSecurityContext(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Equal(t, "error", issues[0].Severity)
	assert.Contains(t, issues[0].Message, "privilege escalation")
}

func TestValidateSecurityContext_AllValid(t *testing.T) {
	runAsNonRoot := true
	readOnlyFS := true
	allowPrivEsc := false
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Policies: &dorguv1.PoliciesSpec{
				Security: &dorguv1.SecurityPolicy{
					RunAsNonRoot:             &runAsNonRoot,
					ReadOnlyRootFilesystem:   &readOnlyFS,
					AllowPrivilegeEscalation: &allowPrivEsc,
				},
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
					},
					Containers: []corev1.Container{
						{
							Name: "app",
							SecurityContext: &corev1.SecurityContext{
								ReadOnlyRootFilesystem:   &readOnlyFS,
								AllowPrivilegeEscalation: &allowPrivEsc,
							},
						},
					},
				},
			},
		},
	}

	issues := validateSecurityContext(persona, deploy)
	assert.Empty(t, issues)
}

// ============================================================================
// countSeverity tests
// ============================================================================

func TestCountSeverity(t *testing.T) {
	issues := []dorguv1.ValidationIssue{
		{Severity: "error", Message: "Error 1"},
		{Severity: "warning", Message: "Warning 1"},
		{Severity: "error", Message: "Error 2"},
		{Severity: "info", Message: "Info 1"},
		{Severity: "error", Message: "Error 3"},
	}

	assert.Equal(t, 3, countSeverity(issues, "error"))
	assert.Equal(t, 1, countSeverity(issues, "warning"))
	assert.Equal(t, 1, countSeverity(issues, "info"))
	assert.Equal(t, 0, countSeverity(issues, "critical"))
}

func TestCountSeverity_Empty(t *testing.T) {
	issues := []dorguv1.ValidationIssue{}
	assert.Equal(t, 0, countSeverity(issues, "error"))
}

// ============================================================================
// Multiple containers tests
// ============================================================================

func TestValidateResources_MultipleContainers(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "test-app",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{
					CPU: "500m",
				},
			},
		},
	}
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("100m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1000m"),
								},
							},
						},
						{
							Name: "sidecar",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("50m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("200m"),
								},
							},
						},
					},
				},
			},
		},
	}

	issues := validateResources(persona, deploy)
	assert.Len(t, issues, 1)
	assert.Contains(t, issues[0].Field, "app")
}
