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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

// Names the fixtures use.
const (
	PersonaName  = "web"
	IncidentName = "web-oomkilled"
	// ProbeNamespace exists in every cluster.
	ProbeNamespace = "default"
)

// manualStep is the smallest valid step: advice, applying nothing.
func manualStep() dorguv1.RemediationStep {
	return dorguv1.RemediationStep{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "look at the heap"}
}

// NewAction is the smallest valid RemediationAction in ns. With a proposer it carries
// provenance naming it (what the operator writes when it converts a proposal); without,
// it reads as an action made before provenance existed.
func NewAction(ns, name, proposer string) *dorguv1.RemediationAction {
	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{Name: IncidentName, Namespace: ns},
			PersonaRef:  dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: PersonaName, Namespace: ns},
			TrustLevel:  2,
			Explanation: "Look at the heap",
			Confidence:  "0.5",
			PlanSource:  "rule-based",
			Action:      dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification},
			Steps:       []dorguv1.RemediationStep{manualStep()},
			Approval:    &dorguv1.ApprovalSpec{Required: true},
		},
	}
	if proposer != "" {
		ra.Spec.Provenance = &dorguv1.RemediationProvenance{
			Proposal: name,
			Proposer: dorguv1.ProposerIdentity{Username: proposer},
			Source:   dorguv1.ProposalSourceAgent,
		}
	}
	return ra
}

// NewProposal is the smallest valid RemediationProposal in ns, written by username.
func NewProposal(ns, name, username string) *dorguv1alpha1.RemediationProposal {
	return &dorguv1alpha1.RemediationProposal{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: dorguv1alpha1.RemediationProposalSpec{
			Proposer:       dorguv1.ProposerIdentity{Username: username},
			IdempotencyKey: name,
			Target:         dorguv1alpha1.ProposalTarget{Kind: "ApplicationPersona", Namespace: ns, Name: PersonaName},
			Steps:          []dorguv1.RemediationStep{manualStep()},
			Rationale:      "the heap looks full",
		},
	}
}

// ProbeProposal is a proposal the policies refuse for anyone who is not "probe-nobody".
func ProbeProposal() *dorguv1alpha1.RemediationProposal {
	return NewProposal(ProbeNamespace, "probe", "probe-nobody")
}

// ProbeAction is an action only the operator may create.
func ProbeAction() *dorguv1.RemediationAction { return NewAction(ProbeNamespace, "probe", "") }

// NewNamespace creates a namespace for one case.
func (e *Env) NewNamespace(ctx context.Context, prefix string) (string, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-"}}
	if err := e.Admin.Create(ctx, ns); err != nil {
		return "", err
	}
	return ns.Name, nil
}

// MakePending creates ra as the operator and writes the status the intake writes: phase
// Pending, a Proposed condition. It is what a human then approves.
func (e *Env) MakePending(ctx context.Context, ra *dorguv1.RemediationAction) error {
	if err := e.Operator.Create(ctx, ra); err != nil {
		return fmt.Errorf("creating the action as the operator: %w", err)
	}
	ra.Status = dorguv1.RemediationActionStatus{
		Phase: "Pending",
		Conditions: []metav1.Condition{{
			Type: dorguv1.ConditionProposed, Status: metav1.ConditionTrue, Reason: dorguv1.ReasonGateCleared,
			ObservedGeneration: ra.Generation, LastTransitionTime: metav1.Now(),
		}},
	}
	if err := e.Operator.Status().Update(ctx, ra); err != nil {
		return fmt.Errorf("writing the Pending status as the operator: %w", err)
	}
	return nil
}

// PatchStatus applies a JSON merge patch to the status subresource, the way
// `kubectl patch --type merge --subresource status` does (and so the way the CLI's
// approve, reject and heal record do), as c.
func PatchStatus(ctx context.Context, c client.Client, ra *dorguv1.RemediationAction, status map[string]any) error {
	body, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return err
	}
	target := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: ra.Name, Namespace: ra.Namespace}}
	return c.Status().Patch(ctx, target, client.RawPatch(types.MergePatchType, body))
}

// Approve is the approval patch the CLI writes.
func Approve(ctx context.Context, c client.Client, ra *dorguv1.RemediationAction, approvedBy string) error {
	return PatchStatus(ctx, c, ra, map[string]any{
		"phase": "Approved", "approvedBy": approvedBy, "approvedAt": metav1.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// Reject is the rejection patch the CLI writes (it records who decided in approvedBy).
func Reject(ctx context.Context, c client.Client, ra *dorguv1.RemediationAction, rejectedBy string) error {
	return PatchStatus(ctx, c, ra, map[string]any{"phase": "Rejected", "approvedBy": rejectedBy})
}
