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
	// Retained for back-compat: when Steps is empty, Action is the plan of record.
	Action RemediationActionDetail `json:"action"`

	// Steps is an ordered, multi-step remediation plan. When non-empty it is the
	// plan of record (and supersedes the single Action for planning purposes).
	// NOTE (this sprint): Steps is populated and validated but not yet executed;
	// the controller/executor continue to apply the single Action.
	// +optional
	// +listType=atomic
	Steps []RemediationStep `json:"steps,omitempty"`

	// PlanSource records how the plan was produced.
	// +kubebuilder:validation:Enum=rule-based;ai-anthropic
	// +optional
	PlanSource string `json:"planSource,omitempty"`

	// PlanSummary is the AI root-cause / plan explanation for the ordered plan.
	// +optional
	PlanSummary string `json:"planSummary,omitempty"`

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

// RemediationStep is a single ordered action within a multi-step remediation plan.
//
// Step-safety invariant: only persona-update steps may be AutoExecutable. All
// other step types are advisory — recorded for a human/CLI/platform to apply —
// which preserves the operator's non-negotiable guarantee that it never writes
// workloads. See ValidateAutoExecutable.
// +kubebuilder:validation:XValidation:rule="!self.autoExecutable || self.type == 'persona-update'",message="only persona-update steps may be autoExecutable"
type RemediationStep struct {
	// Order is the 1-based execution order of this step within the plan.
	// +kubebuilder:validation:Minimum=1
	Order int32 `json:"order"`

	// ID is a stable, per-plan identifier for the step.
	ID string `json:"id"`

	// Type classifies the step action.
	// +kubebuilder:validation:Enum=persona-update;workload-apply;restart;scale;config-change;manual
	Type string `json:"type"`

	// Description is a human-readable summary of the action.
	Description string `json:"description"`

	// Rationale explains why this step is proposed (AI reasoning).
	// +optional
	Rationale string `json:"rationale,omitempty"`

	// Risk is the assessed risk level of applying this step.
	// +kubebuilder:validation:Enum=low;medium;high
	// +optional
	Risk string `json:"risk,omitempty"`

	// AutoExecutable indicates the operator may apply this step without external
	// action. v1 invariant: this is true ONLY for persona-update steps.
	AutoExecutable bool `json:"autoExecutable"`

	// Patch is the JSON merge patch to apply to the Persona spec (persona-update steps).
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
	// Not yet implemented — no controller reads this field, so setting it grants
	// nothing. Every remediation requires human approval. Deferred to Phase 2c
	// (founder decision #7).
	// +optional
	AutoApproveRule *AutoApproveRule `json:"autoApproveRule,omitempty"`
}

// AutoApproveRule configures automatic approval graduation.
//
// Not yet implemented. The type exists in the CRD schema so the field can be set
// without a future API break, but no controller reads it: auto-approval never
// happens, whatever these values say. Deferred to Phase 2c.
type AutoApproveRule struct {
	// Enabled activates auto-approve graduation.
	// Not yet implemented — see AutoApproveRule.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// AfterSuccessfulOccurrences is the number of successful identical remediations
	// required before auto-approval is granted.
	// Not yet implemented — see AutoApproveRule.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	AfterSuccessfulOccurrences int32 `json:"afterSuccessfulOccurrences"`
}

// RemediationRollbackSpec configures automatic rollback behavior for a single remediation action.
// This is intentionally separate from RollbackPolicy (cluster-level default in ClusterPersona)
// because per-action overrides may diverge from cluster defaults as features evolve.
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

	// CurrentStep is the 1-based order of the step currently being executed.
	// Schema only this sprint — not yet driven by the controller.
	// +optional
	CurrentStep int32 `json:"currentStep,omitempty"`

	// StepStatuses tracks per-step execution outcome for the ordered plan.
	// Schema only this sprint — not yet driven by the controller.
	// +optional
	// +listType=atomic
	StepStatuses []StepStatus `json:"stepStatuses,omitempty"`
}

// StepStatus tracks the execution outcome of a single RemediationStep.
type StepStatus struct {
	// Order is the 1-based order of the step this status refers to.
	// +kubebuilder:validation:Minimum=1
	Order int32 `json:"order"`

	// Phase tracks the step lifecycle.
	// +kubebuilder:validation:Enum=Pending;Applied;Verified;Failed;Skipped
	// +optional
	Phase string `json:"phase,omitempty"`

	// AppliedAt is when the step was applied.
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`

	// VerificationResult is the outcome of post-apply verification for this step.
	// +optional
	VerificationResult string `json:"verificationResult,omitempty"`
}

// ============================================================================
// Root types
// ============================================================================

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ra
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.action.type`
// +kubebuilder:printcolumn:name="Plan",type=string,JSONPath=`.spec.planSource`
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
