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
// Shared reference types (used by DorguEvent and RemediationAction)
// ============================================================================

// IncidentReference references an IncidentMemory resource.
type IncidentReference struct {
	// Name of the IncidentMemory.
	Name string `json:"name"`

	// Namespace of the IncidentMemory.
	Namespace string `json:"namespace"`
}

// ============================================================================
// DorguEvent Spec types
// ============================================================================

// DorguEventSpec defines the desired state of DorguEvent.
type DorguEventSpec struct {
	// Severity of the event.
	// +kubebuilder:validation:Enum=info;warning;critical
	Severity string `json:"severity"`

	// Category classifies the event.
	// +kubebuilder:validation:Enum=resource;scaling;health;security;deployment;dependency;node;controlplane
	Category string `json:"category"`

	// Source identifies the detector that generated this event.
	Source string `json:"source"`

	// Message is a human-readable description.
	Message string `json:"message"`

	// InvolvedObject references the K8s resource this event relates to.
	InvolvedObject ResourceReference `json:"involvedObject"`

	// PersonaRef links this event to a Persona (if applicable).
	// +optional
	PersonaRef *PersonaReference `json:"personaRef,omitempty"`

	// IncidentRef links this event to an IncidentMemory (if correlated).
	// +optional
	IncidentRef *IncidentReference `json:"incidentRef,omitempty"`

	// EventTime is when the original K8s event occurred.
	EventTime metav1.Time `json:"eventTime"`

	// K8sEventRef stores the original K8s Event UID for deduplication.
	// +optional
	K8sEventRef string `json:"k8sEventRef,omitempty"`

	// TTL defines how long this event should be retained (default: 24h).
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// ============================================================================
// Root types
// ============================================================================

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=de
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.spec.severity`
// +kubebuilder:printcolumn:name="Category",type=string,JSONPath=`.spec.category`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.spec.message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DorguEvent is the Schema for the dorguevents API.
// It is a write-once classified event record with TTL-based cleanup.
// DorguEvent has no status subresource — it is an immutable event record.
type DorguEvent struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Spec defines the event data.
	// +required
	Spec DorguEventSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// DorguEventList contains a list of DorguEvent.
type DorguEventList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DorguEvent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DorguEvent{}, &DorguEventList{})
}
