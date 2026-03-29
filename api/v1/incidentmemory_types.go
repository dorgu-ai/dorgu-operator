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
// Shared reference types (used by IncidentMemory, RemediationAction, DorguEvent)
// ============================================================================

// PersonaReference references a Persona resource.
type PersonaReference struct {
	// Kind is ApplicationPersona or ClusterPersona.
	// +kubebuilder:validation:Enum=ApplicationPersona;ClusterPersona
	Kind string `json:"kind"`

	// Name of the referenced Persona.
	Name string `json:"name"`

	// Namespace of the referenced Persona (empty for ClusterPersona).
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ResourceReference identifies a Kubernetes resource involved in an incident.
type ResourceReference struct {
	// Kind of the K8s resource (Pod, Node, Deployment, Service, etc.).
	Kind string `json:"kind"`

	// Name of the resource.
	Name string `json:"name"`

	// Namespace of the resource (empty for cluster-scoped).
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Role describes this resource's relationship to the incident (e.g., "affected", "dependency", "upstream", "downstream").
	// +optional
	Role string `json:"role,omitempty"`
}

// RemediationReference references a RemediationAction resource.
type RemediationReference struct {
	// Name of the RemediationAction.
	Name string `json:"name"`

	// Namespace of the RemediationAction.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ============================================================================
// IncidentMemory Spec types
// ============================================================================

// IncidentMemorySpec defines the desired state of IncidentMemory.
type IncidentMemorySpec struct {
	// PersonaRef references the affected Persona.
	PersonaRef PersonaReference `json:"personaRef"`

	// Category classifies the incident type.
	// +kubebuilder:validation:Enum=resource;scaling;health;security;deployment;dependency;node;controlplane
	Category string `json:"category"`

	// Severity indicates the impact level.
	// +kubebuilder:validation:Enum=info;warning;critical
	Severity string `json:"severity"`

	// Detection contains how the incident was discovered.
	Detection DetectionInfo `json:"detection"`

	// RootCause contains the diagnosis result.
	// +optional
	RootCause *RootCauseInfo `json:"rootCause,omitempty"`

	// RelatedResources tracks cross-namespace dependencies involved in this incident.
	// Designed for future cross-namespace correlation (e.g., app depends on db in another namespace).
	// +optional
	RelatedResources []ResourceReference `json:"relatedResources,omitempty"`

	// Resolution contains how the incident was resolved (populated by Phase 2b remediation).
	// +optional
	Resolution *ResolutionInfo `json:"resolution,omitempty"`
}

// DetectionInfo describes how an incident was discovered.
type DetectionInfo struct {
	// Signal is the primary detection signal (e.g., OOMKilled, CrashLoopBackOff, NodeNotReady).
	Signal string `json:"signal"`

	// Source identifies which detector raised this (e.g., pod-failure-detector, node-health-checker).
	Source string `json:"source"`

	// FirstSeen is when the issue was first detected.
	FirstSeen metav1.Time `json:"firstSeen"`

	// LastSeen is when the issue was last observed.
	LastSeen metav1.Time `json:"lastSeen"`

	// AffectedResources lists the K8s resources involved.
	AffectedResources []ResourceReference `json:"affectedResources"`
}

// RootCauseInfo describes the diagnosed root cause of an incident.
type RootCauseInfo struct {
	// Summary is a human-readable explanation of the root cause.
	Summary string `json:"summary"`

	// Confidence is the diagnosis confidence score as a decimal string (e.g., "0.85").
	// +kubebuilder:validation:Pattern=`^(0(\.\d+)?|1(\.0+)?)$`
	Confidence string `json:"confidence"`

	// Provider identifies the diagnosis source (e.g., "rule-engine", "ai-enhanced").
	Provider string `json:"provider"`

	// Contributing lists signals that contributed to the diagnosis.
	// +optional
	Contributing []ContributingSignal `json:"contributing,omitempty"`
}

// ContributingSignal is a signal that contributed to a diagnosis.
type ContributingSignal struct {
	// Signal is the contributing signal name.
	Signal string `json:"signal"`

	// Detail provides context about this signal's contribution.
	Detail string `json:"detail"`
}

// ResolutionInfo describes how an incident was resolved.
type ResolutionInfo struct {
	// Action describes what remediation was applied.
	Action string `json:"action"`

	// RemediationRef references the RemediationAction that resolved this.
	// +optional
	RemediationRef *RemediationReference `json:"remediationRef,omitempty"`

	// AppliedAt is when the remediation was applied.
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`

	// Outcome is the result of the remediation.
	// +kubebuilder:validation:Enum=resolved;partial;failed;rollback
	// +optional
	Outcome string `json:"outcome,omitempty"`

	// Duration is how long from detection to resolution.
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`
}

// ============================================================================
// IncidentMemory Status types
// ============================================================================

// IncidentMemoryStatus defines the observed state of IncidentMemory.
type IncidentMemoryStatus struct {
	// Phase tracks the incident lifecycle.
	// +kubebuilder:validation:Enum=Detected;Investigating;Resolved;Recurring
	// +optional
	Phase string `json:"phase,omitempty"`

	// OccurrenceCount tracks how many times this pattern has been seen.
	// +optional
	OccurrenceCount int32 `json:"occurrenceCount,omitempty"`

	// LastOccurrence is the timestamp of the most recent occurrence.
	// +optional
	LastOccurrence *metav1.Time `json:"lastOccurrence,omitempty"`

	// Conditions are standard K8s conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ============================================================================
// Root types
// ============================================================================

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=im
// +kubebuilder:printcolumn:name="Category",type=string,JSONPath=`.spec.category`
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.spec.severity`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Signal",type=string,JSONPath=`.spec.detection.signal`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IncidentMemory is the Schema for the incidentmemories API.
// It records a detected incident, its diagnosis, and resolution for organizational learning.
type IncidentMemory struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Spec defines the desired state of IncidentMemory.
	// +required
	Spec IncidentMemorySpec `json:"spec"`

	// Status defines the observed state of IncidentMemory.
	// +optional
	Status IncidentMemoryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// IncidentMemoryList contains a list of IncidentMemory.
type IncidentMemoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []IncidentMemory `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IncidentMemory{}, &IncidentMemoryList{})
}
