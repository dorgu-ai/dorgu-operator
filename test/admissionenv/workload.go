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

package admissionenv

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Names and values of the workload fixture MakeWorkload creates.
const (
	// AppName names the persona, its Deployment and its container's app label.
	AppName = "web"
	// WorkloadIncident names the OOMKilled incident.
	WorkloadIncident = "web-oomkilled"
	// BaselineMemory is the memory limit the persona and the Deployment start with.
	BaselineMemory = "256Mi"
	// ContainerName is the Deployment's one container.
	ContainerName = "app"
)

// MakeWorkload creates, as the admin, what a remediation of a memory problem needs to bind to:
// an ApplicationPersona with a 256Mi memory limit, its live Deployment, and an OOMKilled
// IncidentMemory the way Dorgu's detection writes one (no dorgu.io labels: those are
// operator-only).
func (e *Env) MakeWorkload(ctx context.Context, ns string) error {
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: AppName, Namespace: ns},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: AppName, Type: "api", Tier: "standard",
			Resources: &dorguv1.ResourceConstraints{Limits: &dorguv1.ResourceValues{Memory: BaselineMemory, CPU: "250m"}},
		},
	}
	replicas := int32(1)
	labels := map[string]string{"app": AppName}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: AppName, Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: ContainerName, Image: "nginx:1.27",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse(BaselineMemory),
						corev1.ResourceCPU:    resource.MustParse("250m"),
					}},
				}}},
			},
		},
	}
	incident := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: WorkloadIncident, Namespace: ns},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: AppName, Namespace: ns},
			Category:   "resource", Severity: "critical",
			Detection: dorguv1.DetectionInfo{
				Signal: "OOMKilled", Source: "pod-failure-detector",
				FirstSeen: metav1.NewTime(time.Now().Add(-10 * time.Minute)), LastSeen: metav1.Now(),
				AffectedResources: []dorguv1.ResourceReference{{Kind: "Pod", Name: AppName + "-abc", Namespace: ns}},
			},
		},
	}
	if err := e.Admin.Create(ctx, persona); err != nil {
		return fmt.Errorf("creating the persona: %w", err)
	}
	if err := e.Admin.Create(ctx, deploy); err != nil {
		return fmt.Errorf("creating the deployment: %w", err)
	}
	if err := e.Admin.Create(ctx, incident); err != nil {
		return fmt.Errorf("creating the incident: %w", err)
	}
	return nil
}
