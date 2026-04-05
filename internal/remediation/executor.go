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
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Executor applies approved RemediationAction patches to ApplicationPersona CRDs.
type Executor struct {
	client client.Client
	logger logr.Logger
}

// NewExecutor creates a new Executor.
func NewExecutor(c client.Client, logger logr.Logger) *Executor {
	return &Executor{
		client: c,
		logger: logger.WithName("executor"),
	}
}

// Apply validates preconditions and applies the JSON merge patch to the target ApplicationPersona.
func (e *Executor) Apply(ctx context.Context, action *dorguv1.RemediationAction) error {
	if err := e.validatePreconditions(action); err != nil {
		return fmt.Errorf("precondition failed: %w", err)
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
	if err := e.applyPatch(ctx, persona, action.Spec.Action.Patch.Raw); err != nil {
		return fmt.Errorf("applying patch: %w", err)
	}

	e.logger.Info("successfully applied remediation patch",
		"action", action.Name,
		"persona", fmt.Sprintf("%s/%s", persona.Namespace, persona.Name),
	)

	return nil
}

// validatePreconditions checks that the action is ready to be applied.
func (e *Executor) validatePreconditions(action *dorguv1.RemediationAction) error {
	if action.Status.Phase != phaseApproved {
		return fmt.Errorf("action must be in Approved phase, got %q", action.Status.Phase)
	}

	if action.Spec.Action.Type != "persona-update" {
		return fmt.Errorf("unsupported action type %q, only persona-update is supported", action.Spec.Action.Type)
	}

	if action.Spec.Action.Patch == nil || len(action.Spec.Action.Patch.Raw) == 0 {
		return fmt.Errorf("patch must not be nil or empty")
	}

	if action.Spec.Action.PrePatchState == nil || len(action.Spec.Action.PrePatchState.Raw) == 0 {
		return fmt.Errorf("prePatchState must not be nil or empty (required for rollback)")
	}

	// Validate patch is valid JSON.
	var patchData map[string]interface{}
	if err := json.Unmarshal(action.Spec.Action.Patch.Raw, &patchData); err != nil {
		return fmt.Errorf("patch is not valid JSON: %w", err)
	}

	return nil
}

// getTargetPersona fetches the ApplicationPersona referenced by the action.
func (e *Executor) getTargetPersona(ctx context.Context, action *dorguv1.RemediationAction) (*dorguv1.ApplicationPersona, error) {
	if action.Spec.PersonaRef.Kind != "ApplicationPersona" {
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
	var patchWrapper map[string]interface{}
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
	var targetMap map[string]interface{}
	if err := json.Unmarshal(target, &targetMap); err != nil {
		return nil, fmt.Errorf("unmarshalling target: %w", err)
	}

	var patchMap map[string]interface{}
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil, fmt.Errorf("unmarshalling patch: %w", err)
	}

	merged := mergeMaps(targetMap, patchMap)

	return json.Marshal(merged)
}

// mergeMaps recursively merges patch into target following JSON merge patch semantics.
func mergeMaps(target, patch map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(target))
	for k, v := range target {
		result[k] = v
	}

	for k, patchVal := range patch {
		if patchVal == nil {
			delete(result, k)
			continue
		}

		patchMap, patchIsMap := patchVal.(map[string]interface{})
		if !patchIsMap {
			result[k] = patchVal
			continue
		}

		targetVal, targetExists := result[k]
		if !targetExists {
			result[k] = patchVal
			continue
		}

		targetMap, targetIsMap := targetVal.(map[string]interface{})
		if !targetIsMap {
			result[k] = patchVal
			continue
		}

		result[k] = mergeMaps(targetMap, patchMap)
	}

	return result
}
