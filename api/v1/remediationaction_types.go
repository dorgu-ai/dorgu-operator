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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ============================================================================
// RemediationAction Spec types
// ============================================================================

// RemediationActionSpec defines the desired state of RemediationAction.
type RemediationActionSpec struct {
	// IncidentRef references the IncidentMemory that triggered this remediation.
	IncidentRef IncidentReference `json:"incidentRef"`

	// PersonaRef references the Persona to be remediated.
	PersonaRef PersonaReference `json:"personaRef"`

	// TrustLevel is the minimum trust level required to execute this remediation.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=5
	TrustLevel int32 `json:"trustLevel"`

	// Action describes the remediation to apply.
	Action RemediationActionDetail `json:"action"`

	// Explanation provides a human-readable description of why this remediation is proposed.
	Explanation string `json:"explanation"`

	// Confidence is the diagnosis confidence score as a decimal string (e.g., "0.85").
	// +kubebuilder:validation:Pattern=`^(0(\.\d+)?|1(\.0+)?)$`
	Confidence string `json:"confidence"`

	// Approval configures the approval requirements for this remediation.
	// +optional
	Approval *ApprovalSpec `json:"approval,omitempty"`

	// Rollback configures automatic rollback behavior.
	// +optional
	Rollback *RemediationRollbackSpec `json:"rollback,omitempty"`
}

// RemediationActionDetail describes the specific remediation to apply.
type RemediationActionDetail struct {
	// Type classifies the remediation action.
	// +kubebuilder:validation:Enum=persona-update;notification;git-pr
	Type string `json:"type"`

	// Patch is the JSON merge patch to apply to the Persona spec.
	// +optional
	Patch *apiextensionsv1.JSON `json:"patch,omitempty"`

	// PrePatchState is the snapshot of fields before the patch, for rollback.
	// +optional
	PrePatchState *apiextensionsv1.JSON `json:"prePatchState,omitempty"`
}

// ApprovalSpec configures the approval requirements.
type ApprovalSpec struct {
	// Required indicates whether human approval is needed.
	// +kubebuilder:default=true
	Required bool `json:"required"`

	// Deadline is when the pending remediation expires if not approved.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`

	// AutoApproveRule configures automatic approval after repeated successes.
	// This field exists but controllers ignore it until Phase 2c (founder decision #7).
	// +optional
	AutoApproveRule *AutoApproveRule `json:"autoApproveRule,omitempty"`
}

// AutoApproveRule configures automatic approval graduation.
// Deferred to Phase 2c — field exists in CRD but is not executed by controllers.
type AutoApproveRule struct {
	// Enabled activates auto-approve graduation.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// AfterSuccessfulOccurrences is the number of successful identical remediations
	// required before auto-approval is granted.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	AfterSuccessfulOccurrences int32 `json:"afterSuccessfulOccurrences"`
}

// RemediationRollbackSpec configures automatic rollback behavior for a remediation.
type RemediationRollbackSpec struct {
	// Enabled activates automatic rollback on degradation.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// HealthCheckAfter is the duration to wait before verifying remediation health.
	// +optional
	HealthCheckAfter *metav1.Duration `json:"healthCheckAfter,omitempty"`

	// MaxRetries limits rollback attempts.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	MaxRetries int32 `json:"maxRetries"`
}

// ============================================================================
// RemediationAction Status types
// ============================================================================

// RemediationActionStatus defines the observed state of RemediationAction.
type RemediationActionStatus struct {
	// Phase tracks the remediation lifecycle.
	// +kubebuilder:validation:Enum=Pending;Approved;Applying;Verifying;Completed;RolledBack;Failed;Rejected;Expired
	// +optional
	Phase string `json:"phase,omitempty"`

	// ApprovedBy identifies who approved the remediation.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`

	// ApprovedAt is when the remediation was approved.
	// +optional
	ApprovedAt *metav1.Time `json:"approvedAt,omitempty"`

	// AppliedAt is when the remediation patch was applied.
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`

	// VerificationResult is the outcome of post-apply health verification.
	// +kubebuilder:validation:Enum=Healthy;Degraded;Unknown
	// +optional
	VerificationResult string `json:"verificationResult,omitempty"`

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
// +kubebuilder:resource:shortName=ra
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.action.type`
// +kubebuilder:printcolumn:name="Confidence",type=string,JSONPath=`.spec.confidence`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RemediationAction is the Schema for the remediationactions API.
// It represents a proposed or applied remediation for a detected incident.
type RemediationAction struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Spec defines the desired state of RemediationAction.
	// +required
	Spec RemediationActionSpec `json:"spec"`

	// Status defines the observed state of RemediationAction.
	// +optional
	Status RemediationActionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RemediationActionList contains a list of RemediationAction.
type RemediationActionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RemediationAction `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RemediationAction{}, &RemediationActionList{})
}
