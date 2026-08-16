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
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	// DefaultMaxRemediationsPerHour is the default rate limit per persona.
	DefaultMaxRemediationsPerHour = 5

	// FailedCooldown is the cooldown period after a failed remediation.
	FailedCooldown = 30 * time.Minute

	// MaxBlastRadiusMultiplier caps resource increases to 2x.
	MaxBlastRadiusMultiplier = 2.0

	// MaxResourceDecrease caps resource decreases to 50%.
	MaxResourceDecrease = 0.5

	// defaultDenyNamespaces that are always excluded from remediation.
	defaultDenyKubeSystem     = "kube-system"
	defaultDenyOperatorSystem = "dorgu-operator-system"

	// RemediationAction phase constants for safety checks.
	phaseApproved  = "Approved"
	phaseApplying  = "Applying"
	phaseVerifying = "Verifying"
	phaseFailed    = "Failed"
	phasePending   = "Pending"

	// conditionApplied mirrors the controller's Applied condition type, whose
	// reason distinguishes a rejected-before-apply action from a real failure.
	conditionApplied = "Applied"
)

// SafetyCheckerImpl implements the SafetyChecker interface with S1-S4 guardrails.
type SafetyCheckerImpl struct {
	client client.Client
	logger logr.Logger
}

// NewSafetyChecker creates a new SafetyCheckerImpl.
func NewSafetyChecker(c client.Client, logger logr.Logger) *SafetyCheckerImpl {
	return &SafetyCheckerImpl{
		client: c,
		logger: logger.WithName("safety-checker"),
	}
}

// Check validates a proposed RemediationAction against all safety guardrails.
// Returns all violations found (does not short-circuit).
func (s *SafetyCheckerImpl) Check(ctx context.Context, action *dorguv1.RemediationAction) (*SafetyResult, error) {
	var violations []SafetyViolation

	// S1: Rate limiting.
	if v, err := s.checkRateLimit(ctx, action); err != nil {
		return nil, fmt.Errorf("checking rate limit: %w", err)
	} else if v != nil {
		violations = append(violations, *v)
	}

	// S1b: Concurrent remediation check.
	if v, err := s.checkConcurrent(ctx, action); err != nil {
		return nil, fmt.Errorf("checking concurrent: %w", err)
	} else if v != nil {
		violations = append(violations, *v)
	}

	// S1c: Failed cooldown check.
	if v, err := s.checkFailedCooldown(ctx, action); err != nil {
		return nil, fmt.Errorf("checking failed cooldown: %w", err)
	} else if v != nil {
		violations = append(violations, *v)
	}

	// S2: Blast radius caps.
	if v := s.checkBlastRadius(action); v != nil {
		violations = append(violations, *v)
	}

	// S4: Namespace deny list.
	if v, err := s.checkDenyList(ctx, action); err != nil {
		return nil, fmt.Errorf("checking deny list: %w", err)
	} else if v != nil {
		violations = append(violations, *v)
	}

	return &SafetyResult{
		Allowed:    len(violations) == 0,
		Violations: violations,
	}, nil
}

// checkRateLimit enforces max remediations per persona per hour (S1).
func (s *SafetyCheckerImpl) checkRateLimit(ctx context.Context, action *dorguv1.RemediationAction) (*SafetyViolation, error) {
	maxPerHour := s.getMaxRemediationsPerHour(ctx, action)

	existing, err := s.listRecentActions(ctx, action, 1*time.Hour)
	if err != nil {
		return nil, err
	}

	if len(existing) >= int(maxPerHour) {
		return &SafetyViolation{
			Rule:    "rate-limit",
			Message: fmt.Sprintf("rate limit exceeded: %d remediations for persona %s/%s in last hour (max %d)", len(existing), action.Spec.PersonaRef.Namespace, action.Spec.PersonaRef.Name, maxPerHour),
		}, nil
	}

	return nil, nil
}

// checkConcurrent ensures no other remediation is actively running for this persona (S1).
func (s *SafetyCheckerImpl) checkConcurrent(ctx context.Context, action *dorguv1.RemediationAction) (*SafetyViolation, error) {
	actionList, err := s.listPersonaActions(ctx, action)
	if err != nil {
		return nil, err
	}

	for i := range actionList {
		phase := actionList[i].Status.Phase
		if phase == phaseApproved || phase == phaseApplying || phase == phaseVerifying {
			return &SafetyViolation{
				Rule:    "concurrent",
				Message: fmt.Sprintf("concurrent remediation %s is in phase %s for persona %s/%s", actionList[i].Name, phase, action.Spec.PersonaRef.Namespace, action.Spec.PersonaRef.Name),
			}, nil
		}
	}

	return nil, nil
}

// checkFailedCooldown enforces cooldown after failed remediation (S1).
//
// Only remediations that actually went wrong count. A plan the executor refused
// before touching the cluster changed nothing, so counting it blacked out the app
// for 30 minutes over a self-inflicted non-failure (F-03).
func (s *SafetyCheckerImpl) checkFailedCooldown(ctx context.Context, action *dorguv1.RemediationAction) (*SafetyViolation, error) {
	actionList, err := s.listPersonaActions(ctx, action)
	if err != nil {
		return nil, err
	}

	for i := range actionList {
		if actionList[i].Status.Phase != phaseFailed {
			continue
		}
		if rejectedBeforeApply(&actionList[i]) {
			s.logger.V(1).Info("ignoring a rejected-before-apply remediation for the failure cooldown",
				"action", actionList[i].Name)
			continue
		}
		// Check if the failed action is within cooldown period.
		failedAt := actionList[i].CreationTimestamp.Time
		if actionList[i].Status.AppliedAt != nil {
			failedAt = actionList[i].Status.AppliedAt.Time
		}
		if time.Since(failedAt) < FailedCooldown {
			return &SafetyViolation{
				Rule:    "rate-limit",
				Message: fmt.Sprintf("failed remediation %s within %s cooldown period for persona %s/%s", actionList[i].Name, FailedCooldown, action.Spec.PersonaRef.Namespace, action.Spec.PersonaRef.Name),
			}, nil
		}
	}

	return nil, nil
}

// rejectedBeforeApply reports whether a Failed action was refused by the executor
// without anything being written to the cluster, as recorded on its Applied
// condition by the remediation controller.
func rejectedBeforeApply(action *dorguv1.RemediationAction) bool {
	if action.Status.AppliedAt != nil {
		return false
	}
	for _, c := range action.Status.Conditions {
		if c.Type == conditionApplied && c.Reason == dorguv1.ReasonPreconditionRejected {
			return true
		}
	}
	return false
}

// checkBlastRadius validates resource change magnitude (S2).
func (s *SafetyCheckerImpl) checkBlastRadius(action *dorguv1.RemediationAction) *SafetyViolation {
	if action.Spec.Action.Type != "persona-update" {
		return nil
	}
	if action.Spec.Action.Patch == nil || action.Spec.Action.PrePatchState == nil {
		return nil
	}

	// Parse patch and pre-patch state to compare resource values.
	patchValues, err := parseResourcePatch(action.Spec.Action.Patch.Raw)
	if err != nil {
		s.logger.V(1).Info("failed to parse patch for blast radius check", "error", err)
		return nil
	}

	prePatchValues, err := parseResourcePatch(action.Spec.Action.PrePatchState.Raw)
	if err != nil {
		s.logger.V(1).Info("failed to parse pre-patch state for blast radius check", "error", err)
		return nil
	}

	for field, newVal := range patchValues {
		oldVal, ok := prePatchValues[field]
		if !ok || oldVal.IsZero() {
			continue
		}

		newQty := newVal.AsApproximateFloat64()
		oldQty := oldVal.AsApproximateFloat64()

		if oldQty <= 0 {
			continue
		}

		ratio := newQty / oldQty

		// Check increase cap: max 2x.
		if ratio > MaxBlastRadiusMultiplier {
			return &SafetyViolation{
				Rule:    "blast-radius",
				Message: fmt.Sprintf("resource change for %s exceeds maximum: %.1fx increase (max %.1fx)", field, ratio, MaxBlastRadiusMultiplier),
			}
		}

		// Check decrease cap: max 50% reduction.
		if ratio < MaxResourceDecrease {
			return &SafetyViolation{
				Rule:    "blast-radius",
				Message: fmt.Sprintf("resource change for %s exceeds maximum: %.0f%% decrease (max %.0f%%)", field, (1-ratio)*100, MaxResourceDecrease*100),
			}
		}

		// Validation: new value must be > 0.
		if newQty <= 0 {
			return &SafetyViolation{
				Rule:    "blast-radius",
				Message: fmt.Sprintf("resource value for %s must be positive, got %s", field, newVal.String()),
			}
		}
	}

	return nil
}

// checkDenyList checks if the target namespace is in the deny list (S4).
func (s *SafetyCheckerImpl) checkDenyList(ctx context.Context, action *dorguv1.RemediationAction) (*SafetyViolation, error) {
	targetNamespace := action.Spec.PersonaRef.Namespace
	if targetNamespace == "" {
		targetNamespace = action.Namespace
	}

	// Always deny kube-system and dorgu-operator-system.
	if targetNamespace == defaultDenyKubeSystem || targetNamespace == defaultDenyOperatorSystem {
		return &SafetyViolation{
			Rule:    "deny-list",
			Message: fmt.Sprintf("namespace %s is in the default deny list", targetNamespace),
		}, nil
	}

	// Check ClusterPersona excludeNamespaces.
	excludedNamespaces, err := s.getExcludedNamespaces(ctx)
	if err != nil {
		s.logger.V(1).Info("failed to read ClusterPersona excluded namespaces", "error", err)
		// Don't fail the check — default deny list is already enforced.
		return nil, nil
	}

	for _, ns := range excludedNamespaces {
		if targetNamespace == ns {
			return &SafetyViolation{
				Rule:    "deny-list",
				Message: fmt.Sprintf("namespace %s is excluded by ClusterPersona policy", targetNamespace),
			}, nil
		}
	}

	return nil, nil
}

// getMaxRemediationsPerHour reads the rate limit from ClusterPersona, defaulting to 5.
func (s *SafetyCheckerImpl) getMaxRemediationsPerHour(ctx context.Context, action *dorguv1.RemediationAction) int32 {
	var clusterPersonas dorguv1.ClusterPersonaList
	if err := s.client.List(ctx, &clusterPersonas); err != nil {
		s.logger.V(1).Info("failed to list ClusterPersonas for rate limit", "error", err)
		return DefaultMaxRemediationsPerHour
	}

	for i := range clusterPersonas.Items {
		cp := &clusterPersonas.Items[i]
		if cp.Spec.Policies != nil && cp.Spec.Policies.SelfHealing != nil && cp.Spec.Policies.SelfHealing.MaxRemediationsPerHour > 0 {
			return cp.Spec.Policies.SelfHealing.MaxRemediationsPerHour
		}
	}

	return DefaultMaxRemediationsPerHour
}

// getExcludedNamespaces reads excluded namespaces from all ClusterPersonas.
func (s *SafetyCheckerImpl) getExcludedNamespaces(ctx context.Context) ([]string, error) {
	var clusterPersonas dorguv1.ClusterPersonaList
	if err := s.client.List(ctx, &clusterPersonas); err != nil {
		return nil, fmt.Errorf("listing ClusterPersonas: %w", err)
	}

	var excluded []string
	for i := range clusterPersonas.Items {
		cp := &clusterPersonas.Items[i]
		if cp.Spec.Policies != nil && cp.Spec.Policies.SelfHealing != nil {
			excluded = append(excluded, cp.Spec.Policies.SelfHealing.ExcludeNamespaces...)
		}
	}

	return excluded, nil
}

// listRecentActions returns RemediationActions for the same persona created within the given window.
func (s *SafetyCheckerImpl) listRecentActions(ctx context.Context, action *dorguv1.RemediationAction, window time.Duration) ([]dorguv1.RemediationAction, error) {
	allActions, err := s.listPersonaActions(ctx, action)
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-window)
	var recent []dorguv1.RemediationAction
	for i := range allActions {
		if allActions[i].CreationTimestamp.Time.After(cutoff) {
			recent = append(recent, allActions[i])
		}
	}

	return recent, nil
}

// listPersonaActions returns all RemediationActions for the same persona.
func (s *SafetyCheckerImpl) listPersonaActions(ctx context.Context, action *dorguv1.RemediationAction) ([]dorguv1.RemediationAction, error) {
	labelSelector := labels.SelectorFromSet(labels.Set{
		"dorgu.io/persona-kind": action.Spec.PersonaRef.Kind,
		"dorgu.io/persona-name": action.Spec.PersonaRef.Name,
	})

	var list dorguv1.RemediationActionList
	opts := []client.ListOption{
		client.MatchingLabelsSelector{Selector: labelSelector},
	}
	if action.Spec.PersonaRef.Namespace != "" {
		opts = append(opts, client.InNamespace(action.Spec.PersonaRef.Namespace))
	}

	if err := s.client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing RemediationActions: %w", err)
	}

	return list.Items, nil
}

// parseResourcePatch extracts resource field→quantity mappings from a JSON patch.
// Expected format: {"spec":{"resources":{"limits":{"memory":"512Mi","cpu":"500m"}}}}
func parseResourcePatch(raw []byte) (map[string]resource.Quantity, error) {
	var patch map[string]interface{}
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, fmt.Errorf("unmarshalling patch: %w", err)
	}

	result := make(map[string]resource.Quantity)
	extractResources(patch, "", result)
	return result, nil
}

// extractResources recursively walks the patch structure to find resource values.
func extractResources(obj map[string]interface{}, prefix string, result map[string]resource.Quantity) {
	for key, val := range obj {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}

		switch v := val.(type) {
		case map[string]interface{}:
			extractResources(v, fullKey, result)
		case string:
			qty, err := resource.ParseQuantity(v)
			if err == nil {
				result[fullKey] = qty
			}
		}
	}
}
