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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

const (
	// Resource increase multipliers by severity.
	memoryIncreaseWarning  = 1.5
	memoryIncreaseCritical = 2.0
	cpuIncreaseWarning     = 1.25
	cpuIncreaseCritical    = 1.5

	// maxResourceMultiplier caps all resource increases.
	maxResourceMultiplier = 2.0

	// maxActionNameLength is the maximum length for RemediationAction names.
	maxActionNameLength = 253

	// actionNameOverhead: "ra-" (3) + 3 separators + hash (12) = 18.
	actionNameOverhead = 18

	// Default rollback health check duration.
	defaultHealthCheckAfter = 10 * time.Minute

	// labelPersonaName is the label every proposal path stamps with the target
	// persona name; the dedup List is scoped by it.
	labelPersonaName = "dorgu.io/persona-name"
)

// activeRemediationPhases are the RemediationAction phases that mean a
// remediation is already in flight for an incident, so proposing another would
// duplicate it. Terminal phases (Completed/Failed/RolledBack/Rejected/Expired)
// do NOT block a fresh proposal — e.g. a recurrence after a prior fix failed.
// The empty phase covers a just-created action whose status write has not yet
// landed.
var activeRemediationPhases = map[string]struct{}{
	"":             {},
	phasePending:   {},
	phaseApproved:  {},
	phaseApplying:  {},
	phaseVerifying: {},
}

// Resource-limit patch paths a remediation may target. Used both to classify an
// incoming diagnosis's intended fix and to read what an existing RemediationAction
// changes, so two incidents that resolve to the same fix collapse to one action.
const (
	resourcePathMemory = "spec.resources.limits.memory"
	resourcePathCPU    = "spec.resources.limits.cpu"
)

// activeRemediationExists reports whether a non-terminal RemediationAction
// already makes this proposal redundant, returning a human-readable reason
// (empty when none). The List is scoped by the persona-name label and namespace,
// so every candidate already shares the persona. A candidate blocks the proposal
// when EITHER:
//   - it targets the same incident (per-cycle dedup — the health-check reconciler
//     re-proposes every cycle), OR
//   - it already remediates the same target (WS9 per-persona+target dedup): a
//     second incident for the same root cause (e.g. CrashLoopBackOff trailing an
//     OOMKill, both fixed by raising the memory limit) finds the first incident's
//     remediation and stands down. Terminal-phase actions never block a fresh
//     recurrence.
func (p *Proposer) activeRemediationExists(ctx context.Context, diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) (string, error) {
	namespace := incident.Namespace
	if namespace == "" && diag.PersonaRef != nil {
		namespace = diag.PersonaRef.Namespace
	}

	opts := make([]client.ListOption, 0, 2)
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if diag.PersonaRef != nil && diag.PersonaRef.Name != "" {
		opts = append(opts, client.MatchingLabels{labelPersonaName: diag.PersonaRef.Name})
	}

	var list dorguv1.RemediationActionList
	if err := p.client.List(ctx, &list, opts...); err != nil {
		return "", fmt.Errorf("listing existing RemediationActions for incident %s: %w", incident.Name, err)
	}

	targetPath := diagnosisTargetPath(diag)

	for i := range list.Items {
		ra := &list.Items[i]
		if _, active := activeRemediationPhases[ra.Status.Phase]; !active {
			continue
		}
		if ra.Spec.IncidentRef.Name == incident.Name {
			return fmt.Sprintf("active remediation already exists for incident %s", incident.Name), nil
		}
		if targetPath != "" && remediationTargetsPath(ra, targetPath) {
			return fmt.Sprintf("active remediation already exists for persona %s targeting %s (via %s)",
				diag.PersonaRef.Name, targetPath, ra.Name), nil
		}
	}
	return "", nil
}

// diagnosisTargetPath predicts the persona-spec path an incoming diagnosis's
// resource remediation will patch, mirroring calculateResourceChange's
// signal->dimension mapping. It is the incident-independent identity of the fix,
// so distinct incidents sharing a root cause (OOMKilled, or CrashLoopBackOff with
// OOM correlation -> memory) map to the same path. Returns "" when no stable
// resource target can be derived (non-resource categories keep per-incident dedup
// only).
func diagnosisTargetPath(diag diagnosis.Diagnosis) string {
	switch primarySignalType(diag) {
	case detection.SignalOOMKilled, detection.SignalMemorySaturationCrit, detection.SignalMemorySaturationHigh:
		return resourcePathMemory
	case detection.SignalCPUSaturationHigh, detection.SignalCPUSaturationCritical:
		return resourcePathCPU
	case detection.SignalCrashLoopBackOff:
		if hasOOMCorrelation(diag) {
			return resourcePathMemory
		}
	}
	return ""
}

// remediationTargetsPath reports whether an existing RemediationAction changes
// the given persona-spec path, inspecting both the back-compat single Action
// patch (rule-based path) and every step patch (AI plan path).
func remediationTargetsPath(ra *dorguv1.RemediationAction, target string) bool {
	if ra.Spec.Action.Patch != nil && patchTouchesPath(ra.Spec.Action.Patch.Raw, target) {
		return true
	}
	for i := range ra.Spec.Steps {
		if step := &ra.Spec.Steps[i]; step.Patch != nil && patchTouchesPath(step.Patch.Raw, target) {
			return true
		}
	}
	return false
}

// patchTouchesPath reports whether a merge patch sets a value at the given
// dot-joined leaf path, e.g. {"spec":{"resources":{"limits":{"memory":"384Mi"}}}}
// touches "spec.resources.limits.memory".
func patchTouchesPath(raw []byte, target string) bool {
	for _, p := range patchLeafPaths(raw) {
		if p == target {
			return true
		}
	}
	return false
}

// patchLeafPaths walks a JSON merge patch and returns the dot-joined path of
// every leaf (non-object) value it sets. Returns nil for empty or invalid JSON.
func patchLeafPaths(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	var paths []string
	var walk func(prefix string, node map[string]interface{})
	walk = func(prefix string, node map[string]interface{}) {
		for key, val := range node {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if child, ok := val.(map[string]interface{}); ok {
				walk(path, child)
				continue
			}
			paths = append(paths, path)
		}
	}
	walk("", root)
	return paths
}

// Proposer generates RemediationAction CRDs from diagnoses.
type Proposer struct {
	client  client.Client
	safety  SafetyChecker
	logger  logr.Logger
	planner planner.Planner
}

// ProposerOption configures an optional Proposer dependency.
type ProposerOption func(*Proposer)

// WithPlanner injects an AI remediation planner. When set, Propose first tries
// the AI planning path and falls back to the deterministic rules on any failure.
func WithPlanner(p planner.Planner) ProposerOption {
	return func(pr *Proposer) { pr.planner = p }
}

// NewProposer creates a new Proposer. Pass WithPlanner to enable AI planning.
func NewProposer(c client.Client, safety SafetyChecker, logger logr.Logger, opts ...ProposerOption) *Proposer {
	p := &Proposer{
		client: c,
		safety: safety,
		logger: logger.WithName("proposer"),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Propose generates a RemediationAction from a diagnosis, applying safety checks.
func (p *Proposer) Propose(ctx context.Context, diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) (*ProposalResult, error) {
	if diag.PersonaRef == nil {
		return &ProposalResult{SkipReason: "diagnosis has no persona reference"}, nil
	}

	// Dedup: skip if an active RemediationAction already covers this proposal —
	// either the same incident (per-cycle re-proposal) or the same
	// persona+target (a second incident for one root cause, e.g. CrashLoop after
	// OOM, both fixed by the same memory bump). This runs ahead of both the AI
	// and rule-based paths so one persona + one resource fix yields one action.
	if incident != nil {
		reason, err := p.activeRemediationExists(ctx, diag, incident)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return &ProposalResult{SkipReason: reason}, nil
		}
	}

	// AI path: when a planner is configured, try it first across all signal
	// types. Any failure degrades gracefully to the deterministic rules below
	// (mirroring diagnosis/ai.go's degrade-to-rules behavior).
	if p.planner != nil {
		result, err := p.proposeWithPlanner(ctx, diag, incident)
		if err != nil {
			p.logger.V(0).Info("AI remediation planning failed, falling back to rules",
				"error", err, "category", diag.Category)
		} else if result != nil {
			return result, nil
		}
	}

	switch diag.SuggestedAction {
	case "resource-adjustment":
		return p.proposeResourceAdjustment(ctx, diag, incident)
	case "restart":
		return &ProposalResult{SkipReason: "restart remediation not automated in Phase 2b"}, nil
	default:
		return &ProposalResult{SkipReason: fmt.Sprintf("unsupported action type: %s", diag.SuggestedAction)}, nil
	}
}

// proposeResourceAdjustment creates a RemediationAction for resource limit changes.
func (p *Proposer) proposeResourceAdjustment(ctx context.Context, diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) (*ProposalResult, error) {
	// Read the current ApplicationPersona to get resource values.
	persona, err := p.getApplicationPersona(ctx, diag.PersonaRef)
	if err != nil {
		return nil, fmt.Errorf("getting application persona: %w", err)
	}

	if persona.Spec.Resources == nil || persona.Spec.Resources.Limits == nil {
		return &ProposalResult{SkipReason: "persona has no resource limits configured"}, nil
	}

	// Determine which resource to adjust and by how much based on signals.
	patchMap, prePatchMap, explanation, err := p.calculateResourceChange(diag, persona)
	if err != nil {
		return nil, fmt.Errorf("calculating resource change: %w", err)
	}
	if patchMap == nil {
		return &ProposalResult{SkipReason: "no applicable resource adjustment"}, nil
	}

	patchJSON, err := json.Marshal(patchMap)
	if err != nil {
		return nil, fmt.Errorf("marshalling patch: %w", err)
	}

	prePatchJSON, err := json.Marshal(prePatchMap)
	if err != nil {
		return nil, fmt.Errorf("marshalling pre-patch state: %w", err)
	}

	namespace := diag.PersonaRef.Namespace
	if namespace == "" {
		namespace = "default"
	}

	healthCheckDuration := metav1.Duration{Duration: defaultHealthCheckAfter}

	action := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateActionName(incident, diag.SuggestedAction),
			Namespace: namespace,
			Labels: map[string]string{
				"dorgu.io/persona-kind":      diag.PersonaRef.Kind,
				"dorgu.io/persona-name":      diag.PersonaRef.Name,
				"dorgu.io/persona-namespace": namespace,
			},
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{
				Name:      incident.Name,
				Namespace: incident.Namespace,
			},
			PersonaRef:  *diag.PersonaRef,
			TrustLevel:  2,
			Explanation: explanation,
			Confidence:  fmt.Sprintf("%.2f", diag.Confidence),
			Action: dorguv1.RemediationActionDetail{
				Type:          "persona-update",
				Patch:         &apiextensionsv1.JSON{Raw: patchJSON},
				PrePatchState: &apiextensionsv1.JSON{Raw: prePatchJSON},
			},
			Approval: &dorguv1.ApprovalSpec{
				Required: true,
			},
			Rollback: &dorguv1.RemediationRollbackSpec{
				Enabled:          true,
				HealthCheckAfter: &healthCheckDuration,
				MaxRetries:       1,
			},
		},
	}

	// Run safety checks.
	safetyResult, err := p.safety.Check(ctx, action)
	if err != nil {
		return nil, fmt.Errorf("safety check: %w", err)
	}

	if !safetyResult.Allowed {
		reasons := make([]string, 0, len(safetyResult.Violations))
		for _, v := range safetyResult.Violations {
			reasons = append(reasons, fmt.Sprintf("[%s] %s", v.Rule, v.Message))
		}
		return &ProposalResult{
			SkipReason: fmt.Sprintf("safety check failed: %v", reasons),
		}, nil
	}

	// Create the RemediationAction CRD.
	if err := p.client.Create(ctx, action); err != nil {
		return nil, fmt.Errorf("creating RemediationAction: %w", err)
	}

	// Set initial status.
	action.Status = dorguv1.RemediationActionStatus{
		Phase: phasePending,
	}
	if err := p.client.Status().Update(ctx, action); err != nil {
		p.logger.Error(err, "RemediationAction created but status update failed",
			"name", action.Name)
	}

	return &ProposalResult{
		Action:   action,
		Proposed: true,
	}, nil
}

// calculateResourceChange determines what resource change to apply based on signals.
func (p *Proposer) calculateResourceChange(
	diag diagnosis.Diagnosis,
	persona *dorguv1.ApplicationPersona,
) (patchMap, prePatchMap map[string]interface{}, explanation string, err error) {
	primarySignal := primarySignalType(diag)

	switch primarySignal {
	case detection.SignalOOMKilled, detection.SignalMemorySaturationCrit, detection.SignalMemorySaturationHigh:
		return p.calculateMemoryIncrease(diag, persona)

	case detection.SignalCPUSaturationHigh, detection.SignalCPUSaturationCritical:
		return p.calculateCPUIncrease(diag, persona)

	case detection.SignalCrashLoopBackOff:
		// CrashLoop with OOM correlation → memory increase.
		if hasOOMCorrelation(diag) {
			return p.calculateMemoryIncrease(diag, persona)
		}
		// CrashLoop without OOM → skip.
		return nil, nil, "", nil

	default:
		return nil, nil, "", nil
	}
}

// calculateMemoryIncrease computes a memory limit increase.
func (p *Proposer) calculateMemoryIncrease(
	diag diagnosis.Diagnosis,
	persona *dorguv1.ApplicationPersona,
) (map[string]interface{}, map[string]interface{}, string, error) {
	currentMemory := persona.Spec.Resources.Limits.Memory
	if currentMemory == "" {
		return nil, nil, "", nil
	}

	qty, err := resource.ParseQuantity(currentMemory)
	if err != nil {
		return nil, nil, "", fmt.Errorf("parsing memory quantity %q: %w", currentMemory, err)
	}

	multiplier := memoryIncreaseWarning
	if diag.Severity == detection.SeverityCritical {
		multiplier = memoryIncreaseCritical
	}

	// Cap at maxResourceMultiplier.
	if multiplier > maxResourceMultiplier {
		multiplier = maxResourceMultiplier
	}

	newBytes := int64(float64(qty.Value()) * multiplier)
	newQty := resource.NewQuantity(newBytes, resource.BinarySI)

	explanation := fmt.Sprintf("Increase memory limit from %s to %s (%.0f%% increase) due to %s signal with %s severity",
		currentMemory, newQty.String(), (multiplier-1)*100, primarySignalType(diag), diag.Severity)

	patchMap := buildNestedMap("spec", "resources", "limits", "memory", newQty.String())
	prePatchMap := buildNestedMap("spec", "resources", "limits", "memory", currentMemory)

	return patchMap, prePatchMap, explanation, nil
}

// calculateCPUIncrease computes a CPU limit increase.
func (p *Proposer) calculateCPUIncrease(
	diag diagnosis.Diagnosis,
	persona *dorguv1.ApplicationPersona,
) (map[string]interface{}, map[string]interface{}, string, error) {
	currentCPU := persona.Spec.Resources.Limits.CPU
	if currentCPU == "" {
		return nil, nil, "", nil
	}

	qty, err := resource.ParseQuantity(currentCPU)
	if err != nil {
		return nil, nil, "", fmt.Errorf("parsing CPU quantity %q: %w", currentCPU, err)
	}

	multiplier := cpuIncreaseWarning
	if diag.Severity == detection.SeverityCritical {
		multiplier = cpuIncreaseCritical
	}

	if multiplier > maxResourceMultiplier {
		multiplier = maxResourceMultiplier
	}

	newMillis := int64(float64(qty.MilliValue()) * multiplier)
	newQty := resource.NewMilliQuantity(newMillis, resource.DecimalSI)

	explanation := fmt.Sprintf("Increase CPU limit from %s to %s (%.0f%% increase) due to %s signal with %s severity",
		currentCPU, newQty.String(), (multiplier-1)*100, primarySignalType(diag), diag.Severity)

	patchMap := buildNestedMap("spec", "resources", "limits", "cpu", newQty.String())
	prePatchMap := buildNestedMap("spec", "resources", "limits", "cpu", currentCPU)

	return patchMap, prePatchMap, explanation, nil
}

// getApplicationPersona fetches the ApplicationPersona by reference.
func (p *Proposer) getApplicationPersona(ctx context.Context, ref *dorguv1.PersonaReference) (*dorguv1.ApplicationPersona, error) {
	if ref.Kind != "ApplicationPersona" {
		return nil, fmt.Errorf("unsupported persona kind for resource adjustment: %s", ref.Kind)
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = "default"
	}

	var persona dorguv1.ApplicationPersona
	key := client.ObjectKey{Name: ref.Name, Namespace: namespace}
	if err := p.client.Get(ctx, key, &persona); err != nil {
		return nil, fmt.Errorf("getting ApplicationPersona %s/%s: %w", namespace, ref.Name, err)
	}

	return &persona, nil
}

// generateActionName creates a deterministic name for a RemediationAction.
// Format: ra-{persona}-{action}-{timestamp-hash} (max 253 chars).
func generateActionName(incident *dorguv1.IncidentMemory, actionType string) string {
	hashInput := fmt.Sprintf("%s/%s/%s/%d", incident.Namespace, incident.Name, actionType, time.Now().UnixNano())
	hash := sha256.Sum256([]byte(hashInput))
	hashStr := fmt.Sprintf("%x", hash[:6])

	persona := sanitizeActionName(incident.Spec.PersonaRef.Name)
	action := sanitizeActionName(actionType)

	maxSegmentLen := (maxActionNameLength - actionNameOverhead) / 3
	persona = truncateStr(persona, maxSegmentLen)
	action = truncateStr(action, maxSegmentLen)

	return fmt.Sprintf("ra-%s-%s-%s", persona, action, hashStr)
}

// primarySignalType returns the primary signal type from a diagnosis.
func primarySignalType(diag diagnosis.Diagnosis) detection.SignalType {
	if len(diag.Contributing) > 0 {
		return diag.Contributing[0].Signal.Type
	}
	return detection.SignalType("Unknown")
}

// hasOOMCorrelation checks if a CrashLoop diagnosis has OOM-related contributing signals.
func hasOOMCorrelation(diag diagnosis.Diagnosis) bool {
	for _, cs := range diag.Contributing {
		if cs.Signal.Type == detection.SignalOOMKilled ||
			cs.Signal.Type == detection.SignalMemorySaturationCrit ||
			cs.Signal.Type == detection.SignalMemorySaturationHigh {
			return true
		}
	}
	return false
}

// buildNestedMap creates a nested map structure from a list of keys ending with a value.
// e.g., buildNestedMap("spec", "resources", "limits", "memory", "512Mi") →
// {"spec": {"resources": {"limits": {"memory": "512Mi"}}}}
func buildNestedMap(keys ...string) map[string]interface{} {
	if len(keys) < 2 {
		return nil
	}

	value := keys[len(keys)-1]
	pathKeys := keys[:len(keys)-1]

	result := map[string]interface{}{pathKeys[len(pathKeys)-1]: value}
	for i := len(pathKeys) - 2; i >= 0; i-- {
		result = map[string]interface{}{pathKeys[i]: result}
	}
	return result
}

// sanitizeActionName converts a string to a valid K8s name component.
func sanitizeActionName(s string) string {
	result := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32)
		} else {
			result = append(result, '-')
		}
	}
	return string(result)
}

// truncateStr returns s truncated to maxLen characters.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
