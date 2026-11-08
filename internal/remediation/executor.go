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

package remediation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// PreconditionError reports that a RemediationAction was refused before the
// executor touched the cluster: the action was not fit to apply. It is distinct
// from a failure to apply, which leaves the cluster half-changed and is what the
// failure cooldown exists to protect against (F-03).
type PreconditionError struct {
	Reason string

	// Guardrail is the machine-readable reason (a gate.Guardrail* value) when the
	// gate refused; empty for a phase or wiring problem. Callers branch on it,
	// never on Reason.
	Guardrail string
}

func (e *PreconditionError) Error() string {
	return fmt.Sprintf("precondition failed: %s", e.Reason)
}

// Executor applies the apply ticket of an approved RemediationAction to its
// ApplicationPersona.
//
// It applies exactly status.applyTicket and nothing else: spec.steps[] is the
// plan a human approved, and the ticket is that plan after the gate re-measured
// it on live state at approval. spec.action is deprecated and never read here.
type Executor struct {
	client client.Client
	gate   *gate.Gate
	logger logr.Logger
}

// NewExecutor creates a new Executor. The gate owns the check that a ticket is
// still good to apply (CheckTicket); an Executor with no gate applies nothing.
func NewExecutor(c client.Client, g *gate.Gate, logger logr.Logger) *Executor {
	return &Executor{
		client: c,
		gate:   g,
		logger: logger.WithName("executor"),
	}
}

// Apply applies the ticket's patch to the target ApplicationPersona, and only
// when the gate says the ticket stands: present, well-formed, unexpired, and
// its workload still the one it was measured against.
//
// A refusal is returned as a *PreconditionError: nothing was written, so callers
// can distinguish it from a real apply failure. An unreadable workload is a
// refusal too, never a pass.
func (e *Executor) Apply(ctx context.Context, action *dorguv1.RemediationAction) error {
	if err := e.validatePreconditions(ctx, action); err != nil {
		var problem *gate.TicketProblem
		if errors.As(err, &problem) {
			return &PreconditionError{Reason: problem.Reason, Guardrail: problem.Guardrail}
		}
		return &PreconditionError{Reason: err.Error()}
	}

	// Fetch target Persona.
	persona, err := e.getTargetPersona(ctx, action)
	if err != nil {
		return fmt.Errorf("getting target persona: %w", err)
	}

	// Verify persona is not being deleted.
	if persona.DeletionTimestamp != nil {
		return fmt.Errorf("target persona %s/%s is being deleted", persona.Namespace, persona.Name)
	}

	// Apply JSON merge patch.
	if err := e.applyPatch(ctx, persona, action.Status.ApplyTicket.Patch.Raw); err != nil {
		return fmt.Errorf("applying patch: %w", err)
	}

	e.logger.Info("successfully applied remediation patch",
		"action", action.Name,
		"persona", fmt.Sprintf("%s/%s", persona.Namespace, persona.Name),
	)

	return nil
}

// validatePreconditions checks that the action may be applied: it is Ticketed
// and the gate honours its ticket. Nothing here judges the change; the gate did
// that at approval, and this only confirms the clearance it wrote still stands.
func (e *Executor) validatePreconditions(ctx context.Context, action *dorguv1.RemediationAction) error {
	if action.Status.Phase != phaseTicketed {
		return fmt.Errorf("action must be in Ticketed phase, got %q", action.Status.Phase)
	}

	problem, err := e.gate.CheckTicket(ctx, action)
	if err != nil {
		return fmt.Errorf("the apply ticket could not be checked against the live workload: %w", err)
	}
	if problem != nil {
		return problem
	}
	return nil
}

// getTargetPersona fetches the ApplicationPersona referenced by the action.
func (e *Executor) getTargetPersona(ctx context.Context, action *dorguv1.RemediationAction) (*dorguv1.ApplicationPersona, error) {
	if action.Spec.PersonaRef.Kind != kindApplicationPersona {
		return nil, fmt.Errorf("unsupported persona kind %q, only ApplicationPersona is supported", action.Spec.PersonaRef.Kind)
	}

	namespace := action.Spec.PersonaRef.Namespace
	if namespace == "" {
		namespace = action.Namespace
	}

	var persona dorguv1.ApplicationPersona
	key := client.ObjectKey{Name: action.Spec.PersonaRef.Name, Namespace: namespace}
	if err := e.client.Get(ctx, key, &persona); err != nil {
		return nil, fmt.Errorf("getting ApplicationPersona %s/%s: %w", namespace, action.Spec.PersonaRef.Name, err)
	}

	return &persona, nil
}

// applyPatch applies a JSON merge patch to the ApplicationPersona spec.
func (e *Executor) applyPatch(ctx context.Context, persona *dorguv1.ApplicationPersona, patchRaw []byte) error {
	// Marshal the current persona to JSON, apply the merge patch, and unmarshal back.
	currentJSON, err := json.Marshal(persona.Spec)
	if err != nil {
		return fmt.Errorf("marshalling current spec: %w", err)
	}

	// The patch is structured as {"spec": {...}} — extract the inner spec portion.
	var patchWrapper map[string]any
	if err := json.Unmarshal(patchRaw, &patchWrapper); err != nil {
		return fmt.Errorf("unmarshalling patch: %w", err)
	}

	specPatch, ok := patchWrapper["spec"]
	if !ok {
		return fmt.Errorf("patch must contain a 'spec' key")
	}

	specPatchJSON, err := json.Marshal(specPatch)
	if err != nil {
		return fmt.Errorf("marshalling spec patch: %w", err)
	}

	// Merge the patch into the current spec.
	mergedJSON, err := jsonMergePatch(currentJSON, specPatchJSON)
	if err != nil {
		return fmt.Errorf("merging patch: %w", err)
	}

	var mergedSpec dorguv1.ApplicationPersonaSpec
	if err := json.Unmarshal(mergedJSON, &mergedSpec); err != nil {
		return fmt.Errorf("unmarshalling merged spec: %w", err)
	}

	// Update the persona with the new spec.
	persona.Spec = mergedSpec
	if err := e.client.Update(ctx, persona); err != nil {
		return fmt.Errorf("updating ApplicationPersona: %w", err)
	}

	return nil
}

// jsonMergePatch applies a JSON merge patch (RFC 7396) to a target JSON document.
func jsonMergePatch(target, patch []byte) ([]byte, error) {
	var targetMap map[string]any
	if err := json.Unmarshal(target, &targetMap); err != nil {
		return nil, fmt.Errorf("unmarshalling target: %w", err)
	}

	var patchMap map[string]any
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil, fmt.Errorf("unmarshalling patch: %w", err)
	}

	merged := gate.MergeMaps(targetMap, patchMap)

	return json.Marshal(merged)
}
