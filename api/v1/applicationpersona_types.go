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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ============================================================================
// Spec types (owned by CLI / GitOps)
// ============================================================================

// ApplicationPersonaSpec defines the desired state of an application.
type ApplicationPersonaSpec struct {
	// name is the application name.
	// +required
	Name string `json:"name"`

	// version is the persona schema version.
	// +optional
	Version string `json:"version,omitempty"`

	// type classifies the workload.
	// +kubebuilder:validation:Enum=api;web;worker;cron;daemon
	// +required
	Type string `json:"type"`

	// tier indicates criticality.
	// +kubebuilder:validation:Enum=critical;standard;best-effort
	// +kubebuilder:default=standard
	// +optional
	Tier string `json:"tier,omitempty"`

	// technical contains language, framework, and description.
	// +optional
	Technical *TechnicalProfile `json:"technical,omitempty"`

	// resources defines CPU/memory constraints.
	// +optional
	Resources *ResourceConstraints `json:"resources,omitempty"`

	// scaling defines HPA bounds and behavior.
	// +optional
	Scaling *ScalingSpec `json:"scaling,omitempty"`

	// health defines probe paths and ports.
	// +optional
	Health *HealthSpec `json:"health,omitempty"`

	// dependencies lists required backing services.
	// +optional
	Dependencies []DependencySpec `json:"dependencies,omitempty"`

	// networking describes ports and ingress.
	// +optional
	Networking *NetworkingSpec `json:"networking,omitempty"`

	// ownership captures team and contact info.
	// +optional
	Ownership *OwnershipSpec `json:"ownership,omitempty"`

	// policies defines security, deployment, and maintenance rules.
	// +optional
	Policies *PoliciesSpec `json:"policies,omitempty"`
}

// TechnicalProfile describes the application's tech stack.
type TechnicalProfile struct {
	// +optional
	Language string `json:"language,omitempty"`
	// +optional
	Framework string `json:"framework,omitempty"`
	// +optional
	Description string `json:"description,omitempty"`
}

// ResourceConstraints defines compute resource bounds.
type ResourceConstraints struct {
	// +optional
	Requests *ResourceValues `json:"requests,omitempty"`
	// +optional
	Limits *ResourceValues `json:"limits,omitempty"`
	// profile is a named resource profile.
	// +kubebuilder:validation:Enum=minimal;standard;compute-heavy;memory-heavy
	// +optional
	Profile string `json:"profile,omitempty"`
}

// ResourceValues holds CPU and memory strings.
type ResourceValues struct {
	// +optional
	CPU string `json:"cpu,omitempty"`
	// +optional
	Memory string `json:"memory,omitempty"`
}

// ScalingSpec defines autoscaling parameters.
type ScalingSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetCPU *int32 `json:"targetCPU,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetMemory *int32 `json:"targetMemory,omitempty"`
	// +kubebuilder:validation:Enum=conservative;balanced;aggressive
	// +kubebuilder:default=balanced
	// +optional
	Behavior string `json:"behavior,omitempty"`
}

// HealthSpec defines liveness/readiness probe configuration.
type HealthSpec struct {
	// +optional
	LivenessPath string `json:"livenessPath,omitempty"`
	// +optional
	ReadinessPath string `json:"readinessPath,omitempty"`
	// +optional
	Port *int32 `json:"port,omitempty"`
	// +kubebuilder:default="30s"
	// +optional
	StartupGracePeriod string `json:"startupGracePeriod,omitempty"`
}

// DependencySpec describes a single dependency.
type DependencySpec struct {
	// +required
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=database;cache;queue;service;external
	// +optional
	Type string `json:"type,omitempty"`
	// +kubebuilder:default=true
	// +optional
	Required *bool `json:"required,omitempty"`
	// +optional
	HealthCheck string `json:"healthCheck,omitempty"`
}

// NetworkingSpec describes ports and ingress.
type NetworkingSpec struct {
	// +optional
	Ports []PortSpec `json:"ports,omitempty"`
	// +optional
	Ingress *IngressSpec `json:"ingress,omitempty"`
}

// PortSpec defines a single exposed port.
type PortSpec struct {
	Port int32 `json:"port"`
	// +kubebuilder:default=TCP
	// +optional
	Protocol string `json:"protocol,omitempty"`
	// +optional
	Purpose string `json:"purpose,omitempty"`
}

// IngressSpec defines external access.
type IngressSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +optional
	Host string `json:"host,omitempty"`
	// +optional
	Paths []string `json:"paths,omitempty"`
	// +optional
	TLSEnabled bool `json:"tlsEnabled,omitempty"`
}

// OwnershipSpec captures team and contact details.
type OwnershipSpec struct {
	// +optional
	Team string `json:"team,omitempty"`
	// +optional
	Owner string `json:"owner,omitempty"`
	// +optional
	Repository string `json:"repository,omitempty"`
	// +optional
	OnCall string `json:"oncall,omitempty"`
	// +optional
	Runbook string `json:"runbook,omitempty"`
}

// PoliciesSpec groups security, deployment, and maintenance rules.
type PoliciesSpec struct {
	// +optional
	Security *SecurityPolicy `json:"security,omitempty"`
	// +optional
	Deployment *DeploymentPolicy `json:"deployment,omitempty"`
	// +optional
	Maintenance *MaintenancePolicy `json:"maintenance,omitempty"`
}

// SecurityPolicy defines container security constraints.
type SecurityPolicy struct {
	// +kubebuilder:default=true
	// +optional
	RunAsNonRoot *bool `json:"runAsNonRoot,omitempty"`
	// +kubebuilder:default=true
	// +optional
	ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem,omitempty"`
	// +kubebuilder:default=false
	// +optional
	AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation,omitempty"`
}

// DeploymentPolicy defines rollout strategy.
type DeploymentPolicy struct {
	// +kubebuilder:validation:Enum=RollingUpdate;Recreate;BlueGreen;Canary
	// +kubebuilder:default=RollingUpdate
	// +optional
	Strategy string `json:"strategy,omitempty"`
	// +kubebuilder:default="25%"
	// +optional
	MaxSurge string `json:"maxSurge,omitempty"`
	// +kubebuilder:default="25%"
	// +optional
	MaxUnavailable string `json:"maxUnavailable,omitempty"`
}

// MaintenancePolicy defines maintenance behavior.
type MaintenancePolicy struct {
	// +optional
	Window string `json:"window,omitempty"`
	// +kubebuilder:default=false
	// +optional
	AutoRestart *bool `json:"autoRestart,omitempty"`
}

// ============================================================================
// Status types (owned by operator)
// ============================================================================

// ApplicationPersonaStatus defines the observed state of ApplicationPersona.
type ApplicationPersonaStatus struct {
	// phase summarises the overall persona lifecycle.
	// +kubebuilder:validation:Enum=Pending;Active;Degraded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// lastUpdated records when the operator last reconciled.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// deployments tracks deployment versions.
	// +optional
	Deployments *DeploymentTracking `json:"deployments,omitempty"`

	// health captures pod/container health.
	// +optional
	Health *HealthStatus `json:"health,omitempty"`

	// validation contains the latest check results.
	// +optional
	Validation *ValidationStatus `json:"validation,omitempty"`

	// learned stores patterns detected over time.
	// +optional
	Learned *LearnedPatterns `json:"learned,omitempty"`

	// recommendations suggests improvements.
	// +optional
	Recommendations []Recommendation `json:"recommendations,omitempty"`

	// argoCD contains ArgoCD sync status if ArgoCD is detected.
	// +optional
	ArgoCD *ArgoCDStatus `json:"argoCD,omitempty"`

	// conditions follow the standard Kubernetes condition pattern.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ArgoCDStatus contains ArgoCD Application sync status.
type ArgoCDStatus struct {
	// syncStatus is the ArgoCD sync status.
	// +kubebuilder:validation:Enum=Synced;OutOfSync;Unknown
	// +optional
	SyncStatus string `json:"syncStatus,omitempty"`

	// healthStatus is the ArgoCD health status.
	// +kubebuilder:validation:Enum=Healthy;Degraded;Progressing;Suspended;Missing;Unknown
	// +optional
	HealthStatus string `json:"healthStatus,omitempty"`

	// lastSyncTime is when ArgoCD last synced.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// revision is the Git revision that was synced.
	// +optional
	Revision string `json:"revision,omitempty"`

	// applicationName is the name of the ArgoCD Application.
	// +optional
	ApplicationName string `json:"applicationName,omitempty"`

	// applicationNamespace is the namespace of the ArgoCD Application.
	// +optional
	ApplicationNamespace string `json:"applicationNamespace,omitempty"`
}

// DeploymentTracking records deployment history.
type DeploymentTracking struct {
	// +optional
	Current string `json:"current,omitempty"`
	// +optional
	LastSuccessful string `json:"lastSuccessful,omitempty"`
	// +optional
	LastFailed string `json:"lastFailed,omitempty"`
	// +optional
	History []DeploymentRecord `json:"history,omitempty"`
}

// DeploymentRecord is one entry in deployment history.
type DeploymentRecord struct {
	Version     string `json:"version"`
	Timestamp   string `json:"timestamp"`
	Status      string `json:"status"`
	TriggeredBy string `json:"triggeredBy,omitempty"`
}

// HealthStatus captures current health information.
type HealthStatus struct {
	// +kubebuilder:validation:Enum=Healthy;Degraded;Unhealthy;Unknown
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	LastCheck *metav1.Time `json:"lastCheck,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// ValidationStatus contains the latest validation results.
type ValidationStatus struct {
	// +optional
	Passed bool `json:"passed,omitempty"`
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`
	// +optional
	Issues []ValidationIssue `json:"issues,omitempty"`
}

// ValidationIssue represents a single validation finding.
type ValidationIssue struct {
	// +kubebuilder:validation:Enum=error;warning;info
	Severity string `json:"severity"`
	// +optional
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
	// +optional
	Suggestion string `json:"suggestion,omitempty"`
}

// LearnedPatterns stores operator-observed patterns.
type LearnedPatterns struct {
	// +optional
	ResourceBaseline *ResourceBaseline `json:"resourceBaseline,omitempty"`
	// +optional
	IncidentCount int32 `json:"incidentCount,omitempty"`
	// +optional
	LastIncident string `json:"lastIncident,omitempty"`
	// +optional
	Patterns []Pattern `json:"patterns,omitempty"`
}

// ResourceBaseline captures average and peak resource usage.
type ResourceBaseline struct {
	// +optional
	AvgCPU string `json:"avgCPU,omitempty"`
	// +optional
	AvgMemory string `json:"avgMemory,omitempty"`
	// +optional
	PeakCPU string `json:"peakCPU,omitempty"`
	// +optional
	PeakMemory string `json:"peakMemory,omitempty"`
}

// Pattern is a single learned pattern.
type Pattern struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	// confidence is a value between 0 and 1, serialised as a string (e.g. "0.92").
	// +optional
	Confidence string `json:"confidence,omitempty"`
}

// Recommendation is a single improvement suggestion.
type Recommendation struct {
	// +kubebuilder:validation:Enum=resource;scaling;security;cost;performance
	Type string `json:"type"`
	// +kubebuilder:validation:Enum=high;medium;low
	Priority string `json:"priority"`
	Message  string `json:"message"`
	// +optional
	Action string `json:"action,omitempty"`
}

// ============================================================================
// Root types
// ============================================================================

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health.status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ApplicationPersona is the Schema for the applicationpersonas API.
// It represents the living identity of an application in a Kubernetes cluster.
type ApplicationPersona struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ApplicationPersona.
	// +required
	Spec ApplicationPersonaSpec `json:"spec"`

	// status defines the observed state of ApplicationPersona.
	// +optional
	Status ApplicationPersonaStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ApplicationPersonaList contains a list of ApplicationPersona.
type ApplicationPersonaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ApplicationPersona `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ApplicationPersona{}, &ApplicationPersonaList{})
}
