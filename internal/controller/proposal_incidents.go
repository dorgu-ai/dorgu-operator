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

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

const (
	// proposalIncidentSignal is the detection signal on an incident the operator
	// opened for a proposal that named none. Nothing detected it: a proposer
	// reported it.
	proposalIncidentSignal = "ProposerReported"

	// proposalIncidentSource is the detection source of such an incident.
	proposalIncidentSource = "remediation-proposal"
)

// ProposalIncidents opens the IncidentMemory a RemediationProposal that names no
// incident is answered against. It implements intake.IncidentOpener.
type ProposalIncidents struct {
	Client client.Client
}

// Open returns the incident for the proposal, creating it the first time. The name
// comes from the proposal's namespace, target, proposer and idempotency key, so
// asking again, after a retry, finds the incident the first attempt made.
func (o *ProposalIncidents) Open(ctx context.Context, prop *dorguv1alpha1.RemediationProposal) (*dorguv1.IncidentMemory, error) {
	persona := dorguv1.PersonaReference{
		Kind: prop.Spec.Target.Kind, Name: prop.Spec.Target.Name, Namespace: prop.Spec.Target.Namespace,
	}
	sum := sha256.Sum256([]byte(prop.Spec.Proposer.Username + "\x00" + prop.Spec.IdempotencyKey))
	name := generateIncidentName(prop.Namespace, persona.Name, fmt.Sprintf("%s-%x", proposalIncidentSignal, sum[:4]))
	key := client.ObjectKey{Namespace: prop.Namespace, Name: name}

	var existing dorguv1.IncidentMemory
	switch err := o.Client.Get(ctx, key, &existing); {
	case err == nil:
		return &existing, nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("reading incident %s: %w", name, err)
	}

	now := metav1.Now()
	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: prop.Namespace,
			Labels: map[string]string{
				LabelPersonaKind:      persona.Kind,
				LabelPersonaName:      persona.Name,
				LabelPersonaNamespace: persona.Namespace,
				LabelCategory:         proposalIncidentCategory(prop),
				LabelSeverity:         "warning",
				LabelSignal:           proposalIncidentSignal,
				LabelPhase:            PhaseDetected,
				LabelAttribution:      AttributionPersona,
			},
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef:  persona,
			Attribution: AttributionPersona,
			Category:    proposalIncidentCategory(prop),
			Severity:    "warning",
			Detection: dorguv1.DetectionInfo{
				Signal:    proposalIncidentSignal,
				Source:    proposalIncidentSource,
				FirstSeen: now,
				LastSeen:  now,
				AffectedResources: []dorguv1.ResourceReference{{
					Kind: persona.Kind, Name: persona.Name, Namespace: persona.Namespace, Role: "affected",
				}},
			},
		},
	}
	if err := o.Client.Create(ctx, im); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := o.Client.Get(ctx, key, &existing); getErr != nil {
				return nil, fmt.Errorf("reading incident %s: %w", name, getErr)
			}
			return &existing, nil
		}
		return nil, fmt.Errorf("creating incident %s: %w", name, err)
	}

	im.Status.Phase = PhaseDetected
	im.Status.OccurrenceCount = 1
	im.Status.LastOccurrence = &now
	if err := retryIncidentWrite(func(attempt int) error {
		if attempt > 0 {
			if err := o.Client.Get(ctx, key, im); err != nil {
				return err
			}
			im.Status.Phase = PhaseDetected
			im.Status.OccurrenceCount = 1
			im.Status.LastOccurrence = &now
		}
		return o.Client.Status().Update(ctx, im)
	}); err != nil {
		return nil, fmt.Errorf("setting the status of incident %s: %w", name, err)
	}
	return im, nil
}

// proposalIncidentCategory classifies the incident from the shape of the plan, not
// from anything the proposer wrote about it: a plan that changes a persona's
// resources is a resource incident, anything else a health one.
func proposalIncidentCategory(prop *dorguv1alpha1.RemediationProposal) string {
	for i := range prop.Spec.Steps {
		if prop.Spec.Steps[i].Type == dorguv1.StepTypePersonaUpdate {
			return "resource"
		}
	}
	return "health"
}
