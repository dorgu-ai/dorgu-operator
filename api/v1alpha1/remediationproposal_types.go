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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Proposal limits.
const (
	// MaxRationaleLength bounds rationale. It is untrusted text: it is shown to
	// people and never read by any decision.
	MaxRationaleLength = 4000

	// MaxSteps bounds the number of steps in one proposal.
	MaxSteps = 64

	// MaxEvidence bounds the number of evidence references in one proposal.
	MaxEvidence = 20
)

// ProposalPhase is where a proposal is in its life.
type ProposalPhase string

const (
	// PhaseReceived means the operator has seen the proposal and not yet decided.
	PhaseReceived ProposalPhase = "Received"

	// PhaseConverted means the proposal became a RemediationAction.
	PhaseConverted ProposalPhase = "Converted"

	// PhaseRejected means the proposal was refused. status.rejection says where and why.
	PhaseRejected ProposalPhase = "Rejected"
)

// StageIntake is the rejection stage for a refusal made before the gate runs. A
// refusal by the gate carries the gate's own stage name (Validate, Ground,
// Ownership, Guardrails, ModePolicy, DedupCooldown).
const StageIntake = "Intake"

// Rejection reasons the intake reconciler gives at StageIntake. A refusal by the
// gate carries the gate's own guardrail code as its reason.
const (
	// ReasonRateLimited: the proposer has already had its allowance of proposals
	// received in the last hour.
	ReasonRateLimited = "RateLimited"

	// ReasonDuplicate: an earlier proposal in this namespace from the same proposer
	// has the same idempotencyKey.
	ReasonDuplicate = "DuplicateIdempotencyKey"

	// ReasonIncidentNotFound: spec.incidentRef names an incident that does not exist.
	ReasonIncidentNotFound = "IncidentNotFound"

	// ReasonNotYetSupported: the proposal uses a field the operator does not honour
	// yet (rollbackOf).
	ReasonNotYetSupported = "NotYetSupported"

	// ReasonNamespaceMismatch: the proposal's namespace is not its target's namespace.
	ReasonNamespaceMismatch = "TargetNamespaceMismatch"

	// ReasonIncidentUnavailable: the incident the proposal needs could not be read
	// or created. Unlike the other intake reasons it is not final: the operator
	// retries.
	ReasonIncidentUnavailable = "IncidentUnavailable"
)

// ConditionConverted is the condition the reconciler keeps on a proposal. It is
// True once the proposal became a RemediationAction and False once it was
// rejected; its reason and message say why.
const ConditionConverted = "Converted"

// ReasonConverted is the Converted condition's reason on success.
const ReasonConverted = "Converted"

// ProposalTarget is what the proposal wants changed. It is an ApplicationPersona:
// the operator's apply path writes persona changes, and everything else a plan can
// say is advice for a person.
type ProposalTarget struct {
	// Kind of the target. Only ApplicationPersona can carry an applied change.
	// +kubebuilder:validation:Enum=ApplicationPersona;ClusterPersona
	Kind string `json:"kind"`

	// Namespace of the target. It must equal the proposal's own namespace. A CRD
	// validation rule cannot read metadata.namespace, so the operator enforces this
	// (a proposal that breaks it is Rejected at Intake with TargetNamespaceMismatch)
	// and an admission policy can enforce it at the API server.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`

	// Name of the target.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// EvidenceRef points at something the proposer looked at: an object, a metric,
// a log. It is a reference, never the evidence itself.
type EvidenceRef struct {
	// Kind of the referenced thing, for example "Pod" or "PrometheusQuery".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`

	// Name or identifier of the referenced thing.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace of the referenced thing, when it has one.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ActionRef names a RemediationAction in the proposal's namespace.
type ActionRef struct {
	// Name of the RemediationAction.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// RemediationProposalSpec is what a proposer asks for. It is immutable: a proposer
// that wants something else writes a new proposal.
type RemediationProposalSpec struct {
	// Proposer is who wrote this proposal.
	Proposer dorguv1.ProposerIdentity `json:"proposer"`

	// Source says what kind of proposer this is: "dorgu-planner" for Dorgu's own
	// planner, "agent" (the default) for anything external. It is recorded and
	// reported; no decision reads it.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,62}$`
	// +kubebuilder:default=agent
	// +optional
	Source string `json:"source,omitempty"`

	// Agent is informational detail about the agent behind the proposal.
	// +optional
	Agent *dorguv1.AgentInfo `json:"agent,omitempty"`

	// IdempotencyKey makes a retried proposal harmless. A proposal with the same
	// proposer and key as an earlier one in the same namespace is rejected as a
	// duplicate, and does not become a second RemediationAction.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	IdempotencyKey string `json:"idempotencyKey"`

	// IncidentRef is the IncidentMemory this proposal answers. When absent, the
	// operator creates an incident for the proposal. When present, the incident
	// must exist.
	// +optional
	IncidentRef *dorguv1.IncidentReference `json:"incidentRef,omitempty"`

	// Target is what the proposal changes.
	Target ProposalTarget `json:"target"`

	// Steps is the ordered plan, in the same typed schema as a RemediationAction.
	// The operator's gate validates every step and may narrow or refuse the plan. A
	// proposal with no steps is refused by the gate; the field is optional only
	// because a rollback proposal derives its own steps.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +optional
	Steps []dorguv1.RemediationStep `json:"steps,omitempty"`

	// Rationale is why the proposer wants this. It is UNTRUSTED plain text: it is
	// shown to people as machine-written text, and no decision, guardrail or branch
	// in the operator reads it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4000
	Rationale string `json:"rationale"`

	// Evidence references what the proposer looked at.
	// +kubebuilder:validation:MaxItems=20
	// +listType=atomic
	// +optional
	Evidence []EvidenceRef `json:"evidence,omitempty"`

	// RollbackOf is RESERVED for a ticketed rollback of a RemediationAction. The
	// operator does not honour it yet: a proposal that sets it is rejected with
	// reason NotYetSupported.
	// +optional
	RollbackOf *ActionRef `json:"rollbackOf,omitempty"`
}

// ProposalRejection says where in the intake a proposal was refused and why.
type ProposalRejection struct {
	// Stage is "Intake" for a refusal before the gate, or the gate stage that
	// refused: Validate, Ground, Ownership, Guardrails, ModePolicy or DedupCooldown.
	// +kubebuilder:validation:MaxLength=63
	Stage string `json:"stage"`

	// Reason is a machine-readable token. At Intake it is one of RateLimited,
	// DuplicateIdempotencyKey, IncidentNotFound, NotYetSupported or
	// TargetNamespaceMismatch. At a gate stage it is the guardrail that refused.
	// +kubebuilder:validation:MaxLength=63
	Reason string `json:"reason"`
}

// RemediationProposalStatus is the operator's answer.
type RemediationProposalStatus struct {
	// Phase is Received, Converted or Rejected.
	// +kubebuilder:validation:Enum=Received;Converted;Rejected
	// +optional
	Phase ProposalPhase `json:"phase,omitempty"`

	// Rejection is set when Phase is Rejected.
	// +optional
	Rejection *ProposalRejection `json:"rejection,omitempty"`

	// RemediationActionRef names the RemediationAction this proposal became. It is
	// set when Phase is Converted.
	// +optional
	RemediationActionRef *ActionRef `json:"remediationActionRef,omitempty"`

	// IncidentRef is the incident the proposal is answered against: the one the
	// proposal named, or the one the operator created for it.
	// +optional
	IncidentRef *dorguv1.IncidentReference `json:"incidentRef,omitempty"`

	// ObservedGeneration is the proposal generation the operator last decided.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carry the human-readable message for the phase.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rprop
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Proposer",type=string,JSONPath=`.spec.proposer.username`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.status.remediationActionRef.name`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.rejection.reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RemediationProposal is a request for a change. Anything may propose; the
// operator's gate decides whether the proposal becomes a RemediationAction, and a
// human approves that action.
type RemediationProposal struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable: write a new proposal instead"
	Spec   RemediationProposalSpec   `json:"spec"`
	Status RemediationProposalStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RemediationProposalList contains a list of RemediationProposal.
type RemediationProposalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RemediationProposal `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RemediationProposal{}, &RemediationProposalList{})
}
