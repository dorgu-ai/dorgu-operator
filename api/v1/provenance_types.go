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

// Proposal sources. A source says which kind of proposer wrote a proposal. It is
// recorded for people and reports; no control flow branches on it.
const (
	// ProposalSourceDorguPlanner marks a proposal Dorgu's own planner wrote, as the
	// operator ServiceAccount.
	ProposalSourceDorguPlanner = "dorgu-planner"

	// ProposalSourceAgent marks a proposal an external agent or tool wrote. It is the
	// default for a proposal that names no source.
	ProposalSourceAgent = "agent"

	// ProposalSourceRollback is reserved for a ticketed rollback (rollbackOf). The
	// reconciler does not honour it yet.
	ProposalSourceRollback = "rollback"
)

// ProposerIdentity names who wrote a proposal. Username is the Kubernetes
// username the API server authenticated, for example
// "system:serviceaccount:dorgu-system:dorgu-operator" or "alice@example.com".
//
// A later admission policy requires it to equal the authenticated requester.
// Until that policy is installed it is a claim, not a proof.
type ProposerIdentity struct {
	// Username is the authenticated Kubernetes username of the proposer.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Username string `json:"username"`
}

// AgentInfo is informational detail about the agent behind a proposal. Nothing
// reads it to decide anything.
type AgentInfo struct {
	// Name of the agent or tool.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Name string `json:"name,omitempty"`

	// Version of the agent or tool.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Version string `json:"version,omitempty"`

	// RunID identifies the agent run that produced the proposal.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	RunID string `json:"runID,omitempty"`
}

// RemediationProvenance records, on a RemediationAction, which proposal it was
// converted from and who proposed it. The operator writes it when it converts a
// proposal, and it never changes afterwards.
//
// A RemediationAction created before v0.14 has none. Such an action is treated as
// proposed by the operator ServiceAccount (see RemediationActionSpec.ProposerOrDefault).
type RemediationProvenance struct {
	// Proposal is the name of the RemediationProposal, in the action's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Proposal string `json:"proposal"`

	// Proposer is who wrote the proposal.
	Proposer ProposerIdentity `json:"proposer"`

	// Source is the proposal's source (see the ProposalSource* constants).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Source string `json:"source"`

	// RequestedBy is the human on whose behalf the proposal was made, when one
	// exists. A ticketed rollback records its requester here.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`

	// Agent is informational detail about the agent behind the proposal.
	// +optional
	Agent *AgentInfo `json:"agent,omitempty"`
}

// ProposerOrDefault returns the username that proposed this action. An action with
// no provenance predates the proposal intake and is the operator's own, so it
// returns operatorUsername.
func (s *RemediationActionSpec) ProposerOrDefault(operatorUsername string) string {
	if s == nil || s.Provenance == nil || s.Provenance.Proposer.Username == "" {
		return operatorUsername
	}
	return s.Provenance.Proposer.Username
}
