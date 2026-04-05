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
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Rollback reverses a remediation by applying the prePatchState as a merge patch.
type Rollback struct {
	client client.Client
	logger logr.Logger
}

// NewRollback creates a new Rollback handler.
func NewRollback(c client.Client, logger logr.Logger) *Rollback {
	return &Rollback{
		client: c,
		logger: logger.WithName("rollback"),
	}
}

// Execute reverses the remediation patch by applying prePatchState to restore original values.
func (r *Rollback) Execute(ctx context.Context, action *dorguv1.RemediationAction) error {
	// Validate rollback is enabled.
	if action.Spec.Rollback == nil || !action.Spec.Rollback.Enabled {
		return fmt.Errorf("rollback is not enabled for action %s", action.Name)
	}

	// Validate prePatchState exists.
	if action.Spec.Action.PrePatchState == nil || len(action.Spec.Action.PrePatchState.Raw) == 0 {
		return fmt.Errorf("prePatchState is nil or empty for action %s, cannot rollback", action.Name)
	}

	// Fetch the target persona.
	persona, err := r.getTargetPersona(ctx, action)
	if err != nil {
		return fmt.Errorf("getting target persona for rollback: %w", err)
	}

	// Create an executor to reuse the patch logic — prePatchState is the reverse patch.
	exec := &Executor{client: r.client, logger: r.logger}
	if err := exec.applyPatch(ctx, persona, action.Spec.Action.PrePatchState.Raw); err != nil {
		return fmt.Errorf("applying rollback patch: %w", err)
	}

	r.logger.Info("successfully rolled back remediation",
		"action", action.Name,
		"persona", fmt.Sprintf("%s/%s", persona.Namespace, persona.Name),
	)

	return nil
}

// getTargetPersona fetches the ApplicationPersona referenced by the action.
func (r *Rollback) getTargetPersona(ctx context.Context, action *dorguv1.RemediationAction) (*dorguv1.ApplicationPersona, error) {
	if action.Spec.PersonaRef.Kind != "ApplicationPersona" {
		return nil, fmt.Errorf("unsupported persona kind %q", action.Spec.PersonaRef.Kind)
	}

	namespace := action.Spec.PersonaRef.Namespace
	if namespace == "" {
		namespace = action.Namespace
	}

	var persona dorguv1.ApplicationPersona
	key := client.ObjectKey{Name: action.Spec.PersonaRef.Name, Namespace: namespace}
	if err := r.client.Get(ctx, key, &persona); err != nil {
		return nil, fmt.Errorf("getting ApplicationPersona %s/%s: %w", namespace, action.Spec.PersonaRef.Name, err)
	}

	return &persona, nil
}
