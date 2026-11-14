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

package intake

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

const (
	// maxNameLength is the longest name a RemediationAction may have.
	maxNameLength = 253

	// derivedTrustLevel is the trust level of an action built from a bare proposal.
	// It matches a rule-based action. Nothing reads it (see P2 notes on never-read
	// fields).
	derivedTrustLevel = 2

	// fallbackConfidence is the confidence of an action converted from a proposal
	// whose incident carries no diagnosis. The field is required and means "how
	// sure the diagnosis was": zero says nobody measured it.
	fallbackConfidence = "0"
)

// InstallDecision applies what the gate allows to the action: the plan it let
// through, and the verification and rollback settings the operator owns. The
// plan's own settings were requests; these are the operator's.
func InstallDecision(action *dorguv1.RemediationAction, dec *gate.Decision) {
	action.Spec.Steps = dec.Steps
	action.Spec.Action = dec.Action
	if len(dec.Steps) > 0 {
		// steps[] is the only plan; the single-action form follows from it.
		action.Spec.Action = gate.DeriveAction(dec.Steps)
	}
	verify := dec.Verify
	action.Spec.Rollback = &verify
}

// sourceOf is the proposal's source, defaulting the way the CRD does.
func sourceOf(prop *v1alpha1.RemediationProposal) string {
	if prop.Spec.Source == "" {
		return dorguv1.ProposalSourceAgent
	}
	return prop.Spec.Source
}

// provenanceFor is the record the operator writes on the action a proposal became.
func provenanceFor(prop *v1alpha1.RemediationProposal) *dorguv1.RemediationProvenance {
	return &dorguv1.RemediationProvenance{
		Proposal: prop.Name,
		Proposer: prop.Spec.Proposer,
		Source:   sourceOf(prop),
		Agent:    prop.Spec.Agent.DeepCopy(),
	}
}

// personaRefOf is the persona a proposal targets.
func personaRefOf(prop *v1alpha1.RemediationProposal) dorguv1.PersonaReference {
	return dorguv1.PersonaReference{
		Kind:      prop.Spec.Target.Kind,
		Name:      prop.Spec.Target.Name,
		Namespace: prop.Spec.Target.Namespace,
	}
}

// actionName is the name of the action a proposal becomes when nothing else
// names it. It is derived from the proposal, so a retry after a partial failure
// finds the action the first attempt made.
func actionName(prop *v1alpha1.RemediationProposal) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%s", prop.Namespace, prop.Name, prop.UID))
	suffix := fmt.Sprintf("%x", sum[:6])
	persona := strings.Trim(prop.Spec.Target.Name, "-.")
	if limit := maxNameLength - len("ra--") - len(suffix); len(persona) > limit {
		persona = strings.Trim(persona[:limit], "-.")
	}
	return fmt.Sprintf("ra-%s-%s", persona, suffix)
}

// hintMatches reports whether a hint describes this proposal: the draft names the
// proposal's target and carries exactly its steps. A hint that does not is stale or
// wrong and is not used.
func hintMatches(h *Hint, prop *v1alpha1.RemediationProposal) bool {
	if h == nil || h.Draft == nil {
		return false
	}
	d := h.Draft
	persona := d.Spec.PersonaRef
	if persona.Namespace == "" {
		persona.Namespace = d.Namespace
	}
	if d.Namespace != prop.Namespace || persona != personaRefOf(prop) {
		return false
	}
	return stepsEqual(d.Spec.Steps, prop.Spec.Steps)
}

// stepsEqual compares two plans as JSON values. The proposal went through the API
// server, which re-serialises embedded JSON (key order, whitespace), so a byte
// comparison of the draft's patches would call equal plans different.
func stepsEqual(a, b []dorguv1.RemediationStep) bool {
	if len(a) != len(b) {
		return false
	}
	return jsonEqual(a, b)
}

func jsonEqual(a, b any) bool {
	var av, bv any
	ar, err := json.Marshal(a)
	if err != nil {
		return false
	}
	br, err := json.Marshal(b)
	if err != nil {
		return false
	}
	if json.Unmarshal(ar, &av) != nil || json.Unmarshal(br, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// derivedAction builds the action a proposal becomes when the planner gave no hint:
// everything comes from the proposal and from the incident the operator recorded.
func derivedAction(prop *v1alpha1.RemediationProposal, incident *dorguv1.IncidentMemory) *dorguv1.RemediationAction {
	persona := personaRefOf(prop)
	confidence := fallbackConfidence
	if incident != nil && incident.Spec.RootCause != nil && incident.Spec.RootCause.Confidence != "" {
		confidence = incident.Spec.RootCause.Confidence
	}
	annotations := map[string]string{}
	if incident != nil && incident.Spec.Detection.Signal != "" {
		annotations[gate.AnnotationAnswersSignal] = incident.Spec.Detection.Signal
	}

	steps := make([]dorguv1.RemediationStep, len(prop.Spec.Steps))
	for i := range prop.Spec.Steps {
		steps[i] = *prop.Spec.Steps[i].DeepCopy()
	}

	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      actionName(prop),
			Namespace: prop.Namespace,
			Labels: map[string]string{
				"dorgu.io/persona-kind":      persona.Kind,
				"dorgu.io/persona-name":      persona.Name,
				"dorgu.io/persona-namespace": persona.Namespace,
			},
			Annotations: annotations,
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: incidentRefFor(prop, incident),
			PersonaRef:  persona,
			TrustLevel:  derivedTrustLevel,
			// The rationale is untrusted text. It is displayed as the action's
			// explanation and read by nothing else.
			Explanation: prop.Spec.Rationale,
			Confidence:  confidence,
			Steps:       steps,
			Approval:    &dorguv1.ApprovalSpec{Required: true},
		},
	}
}

// incidentRefFor is the incident an action answers.
func incidentRefFor(prop *v1alpha1.RemediationProposal, incident *dorguv1.IncidentMemory) dorguv1.IncidentReference {
	if incident != nil {
		return dorguv1.IncidentReference{Name: incident.Name, Namespace: incident.Namespace}
	}
	if prop.Spec.IncidentRef != nil {
		return *prop.Spec.IncidentRef
	}
	return dorguv1.IncidentReference{}
}

// candidateFor is the gate input for a drafted action.
func candidateFor(action *dorguv1.RemediationAction, facts gate.Facts) gate.Candidate {
	return gate.Candidate{
		Target:          action.Spec.PersonaRef,
		Namespace:       action.Namespace,
		Name:            action.Name,
		Source:          action.Spec.PlanSource,
		IncidentRef:     action.Spec.IncidentRef,
		Steps:           action.Spec.Steps,
		Action:          action.Spec.Action,
		RequestedVerify: action.Spec.Rollback,
		Final:           true,
		Facts:           facts,
	}
}
