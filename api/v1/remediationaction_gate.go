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

// GateStatus records what the operator's gate established about a
// RemediationAction. Every field here is written by the operator, from the live
// cluster, never taken from whoever authored the spec: it is the facts the
// decision was made on, and the verdicts it reached.
//
// The spec fields it overlaps (workloadRef, steps[].safety, action.prePatchState)
// stay readable for one release. Where the two disagree, this is the one to trust.
type GateStatus struct {
	// Phase is the gate phase that produced this record: Proposal when the
	// operator proposed the action, ApprovalRecheck when approval re-ran it on
	// live state.
	// +kubebuilder:validation:Enum=Proposal;ApprovalRecheck
	Phase string `json:"phase"`

	// Result is the overall decision: Pass, Clamp (the plan was narrowed and
	// proceeds) or Refuse.
	// +kubebuilder:validation:Enum=Pass;Clamp;Refuse
	Result string `json:"result"`

	// EvaluatedAt is when the gate ran.
	EvaluatedAt metav1.Time `json:"evaluatedAt"`

	// Observed is what the gate read from the cluster.
	// +optional
	Observed *GateObserved `json:"observed,omitempty"`

	// Ownership says who owns the live workload, as resolved by the operator.
	// +optional
	Ownership *GateOwnership `json:"ownership,omitempty"`

	// Verdicts is every guardrail the gate ran and what each one decided.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	Verdicts []GateVerdict `json:"verdicts,omitempty"`

	// PrePatchState is the baseline the guardrails measured the change against,
	// at the paths the plan touches: the LIVE workload's values when the workload
	// resolved, otherwise the persona's. It is computed by the operator and
	// never accepted from the plan's author.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	PrePatchState *apiextensionsv1.JSON `json:"prePatchState,omitempty"`
}

// GateObserved is the live state the gate decided on.
type GateObserved struct {
	// WorkloadUID is the UID of the live Deployment that was read. A later
	// re-check refuses when it differs: the workload was replaced.
	// +optional
	WorkloadUID string `json:"workloadUID,omitempty"`

	// WorkloadResourceVersion is the resourceVersion of that Deployment when it
	// was read.
	// +optional
	WorkloadResourceVersion string `json:"workloadResourceVersion,omitempty"`

	// PersonaGeneration is the generation of the ApplicationPersona that was
	// read.
	// +optional
	PersonaGeneration int64 `json:"personaGeneration,omitempty"`

	// LiveValues is the live container's resource block.
	// +optional
	LiveValues *ObservedResources `json:"liveValues,omitempty"`

	// ObservedAt is when the cluster was read.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// GateOwnership is the resolved owner of the live workload.
type GateOwnership struct {
	// ManagedBy is the owner class. Anything but "unmanaged" means Dorgu
	// recommends and does not write the workload.
	// +kubebuilder:validation:Enum=helm;argocd;flux;kustomize;unmanaged;unknown
	ManagedBy string `json:"managedBy"`

	// Detail names the specific owner when one was identified.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Detail string `json:"detail,omitempty"`

	// Resolved is false when no live Deployment could be read, in which case
	// ManagedBy is "unknown" and treated as owned.
	Resolved bool `json:"resolved"`
}

// Gate verdict outcomes.
const (
	GateVerdictPass   = "pass"
	GateVerdictClamp  = "clamp"
	GateVerdictRefuse = "refuse"
)

// GateVerdict is one guardrail's decision.
type GateVerdict struct {
	// Stage is the pipeline stage: Validate, Ground, Ownership, Guardrails,
	// ModePolicy or DedupCooldown.
	Stage string `json:"stage"`

	// Guardrail names the check within the stage, e.g. blast-radius,
	// rate-limit, deny-list.
	Guardrail string `json:"guardrail"`

	// Verdict is pass, clamp or refuse.
	// +kubebuilder:validation:Enum=pass;clamp;refuse
	Verdict string `json:"verdict"`

	// Reason is a sentence safe to show a human. It never contains model text.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`

	// Numbers carries the inputs of the verdict (baseline, requested, ratio,
	// ceiling) as data, so no client has to parse them out of Reason.
	// +optional
	Numbers map[string]string `json:"numbers,omitempty"`
}
