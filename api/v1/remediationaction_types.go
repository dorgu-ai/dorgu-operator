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

	// WorkloadRef records the live workload this remediation concerns, who owns
	// it, and what its resources actually were when the plan was made.
	//
	// Absent means the operator could not observe a workload. Consumers must
	// then treat the workload as owned (see WorkloadRef.ManagedBy).
	// +optional
	WorkloadRef *WorkloadRef `json:"workloadRef,omitempty"`

	// Approval configures the approval requirements for this remediation.
	// +optional
	Approval *ApprovalSpec `json:"approval,omitempty"`

	// Rollback configures automatic rollback behavior.
	// +optional
	Rollback *RemediationRollbackSpec `json:"rollback,omitempty"`
}

// ManagedBy values for WorkloadRef.ManagedBy. Everything except
// ManagedByUnmanaged means some other system owns the workload's desired state.
const (
	// ManagedByHelm marks a workload owned by a Helm release.
	ManagedByHelm = "helm"
	// ManagedByArgoCD marks a workload owned by an ArgoCD Application.
	ManagedByArgoCD = "argocd"
	// ManagedByFlux marks a workload reconciled by a Flux controller.
	ManagedByFlux = "flux"
	// ManagedByKustomize marks a workload declared through kustomize.
	ManagedByKustomize = "kustomize"
	// ManagedByUnmanaged marks a workload nothing reconciles: it is only ever
	// changed by a human with kubectl. This is the one value under which the
	// CLI may patch the Deployment.
	ManagedByUnmanaged = "unmanaged"
	// ManagedByUnknown marks a workload whose owner could not be determined,
	// including the case where the Deployment could not be read at all. It is
	// deliberately the default: unknown is treated as owned, so Dorgu explains
	// rather than writes.
	ManagedByUnknown = "unknown"
)

// WorkloadRef records the live workload a remediation concerns and who owns it.
//
// It exists because the ApplicationPersona is a point-in-time import that
// drifts from the running Deployment. Every fact Dorgu states, and every cap it
// computes, is grounded in these observed values rather than in the persona.
//
// Two different writes hang off this record, and they are NOT the same thing:
//
//   - A persona-update step patches the ApplicationPersona. The operator does
//     that itself, it is always safe, and ManagedBy has no bearing on it. Its
//     autoExecutable semantics are unchanged.
//   - The CLI patching the Deployment with the user's credentials is the write
//     that makes the next `helm upgrade` hard-fail on a field-manager conflict.
//     ManagedBy governs only that: the CLI heals the workload only when
//     ManagedBy is "unmanaged".
type WorkloadRef struct {
	// Kind is the workload kind. Only Deployment is resolved today.
	// +kubebuilder:validation:Enum=Deployment
	Kind string `json:"kind"`

	// Name is the live workload's metadata.name. It is not the persona name:
	// the two differ in most brownfield clusters (persona "frontend" resolving
	// to Deployment "frontend-podinfo").
	Name string `json:"name"`

	// Namespace is the live workload's namespace.
	Namespace string `json:"namespace"`

	// Container is the container within the pod template whose resources were
	// observed and which a fix would target.
	// +optional
	Container string `json:"container,omitempty"`

	// ManagedBy is derived from server-side-apply field managers plus
	// labels/annotations (app.kubernetes.io/managed-by, meta.helm.sh/*,
	// argocd.argoproj.io/*, *.toolkit.fluxcd.io/*).
	//
	// Only "unmanaged" permits the CLI to patch the Deployment. Everything
	// else, including "unknown", means Dorgu recommends and does not write.
	// +kubebuilder:validation:Enum=helm;argocd;flux;kustomize;unmanaged;unknown
	// +kubebuilder:default=unknown
	ManagedBy string `json:"managedBy"`

	// ManagedByDetail names the specific owner when one was identified, e.g.
	// `Helm release "frontend" in namespace apps`. It exists so a refusal can
	// name what owns the workload instead of saying "something does".
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ManagedByDetail string `json:"managedByDetail,omitempty"`

	// ObservedResources is the LIVE container resource block at proposal time.
	//
	// An empty CPU or Memory string means the workload does not set that key.
	// That distinction is load-bearing: a remediation may only change a key the
	// workload already has, so approving a memory fix can never silently add a
	// CPU limit.
	// +optional
	ObservedResources *ObservedResources `json:"observedResources,omitempty"`

	// ObservedImage is the container's live image reference, including its tag.
	// It is the only image Dorgu has actually read, and the planner is
	// instructed never to assert a version it has not read.
	// +optional
	ObservedImage string `json:"observedImage,omitempty"`

	// ObservedAt is when the live workload was read.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// ObservedResources is a live container's resource block, split the same way
// the container spec splits it.
//
// It deliberately mirrors the workload rather than the persona's
// ResourceConstraints: a key absent here is absent on the running pod, which is
// what callers need in order to avoid introducing one.
type ObservedResources struct {
	// Requests are the container's live resource requests. An empty field means
	// the container does not set that request.
	// +optional
	Requests *ResourceValues `json:"requests,omitempty"`

	// Limits are the container's live resource limits. An empty field means the
	// container does not set that limit.
	// +optional
	Limits *ResourceValues `json:"limits,omitempty"`
}

// IsOwned reports whether some other system owns this workload's desired state,
// which is true for every ManagedBy value except "unmanaged".
//
// A nil ref is owned: no observation means no evidence that patching is safe.
// This governs the CLI patching the Deployment only. Persona writes by the
// operator are always safe and are not gated on it.
func (w *WorkloadRef) IsOwned() bool {
	return w == nil || w.ManagedBy != ManagedByUnmanaged
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
// +kubebuilder:validation:XValidation:rule="!has(self.command) || self.command.startsWith('kubectl ')",message="step command must be a kubectl invocation"
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

	// Command is a ready-to-run kubectl command that carries out this advisory
	// step, when a single command can. It exists so a correct diagnosis turns
	// into an actual fix instead of prose the reader has to translate.
	//
	// It is never executed: not by the operator (which never writes workloads)
	// and not by the CLI. It is printed for a human to read, check, and run.
	// Because it can originate from a model, it is filtered through
	// SanitizeStepCommand before being persisted: anything that is not a
	// single-line kubectl invocation free of shell metacharacters is dropped.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Command string `json:"command,omitempty"`

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
	// Acknowledged is the terminal phase for an approved plan with nothing to
	// apply: its steps are advisory, so approval records the decision and the
	// operator changes nothing. It is not a failure and does not trip the
	// failure cooldown.
	// +kubebuilder:validation:Enum=Pending;Approved;Applying;Verifying;Completed;Acknowledged;RolledBack;Failed;Rejected;Expired
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
