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
	"slices"
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

// kindApplicationPersona is the only persona kind the remediation path writes to.
const kindApplicationPersona = "ApplicationPersona"

// defaultNamespace is the namespace a remediation lands in when the diagnosis
// carries no namespace of its own.
const defaultNamespace = "default"

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
	return slices.Contains(patchLeafPaths(raw), target)
}

// patchLeafPaths walks a JSON merge patch and returns the dot-joined path of
// every leaf (non-object) value it sets. Returns nil for empty or invalid JSON.
func patchLeafPaths(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	var paths []string
	var walk func(prefix string, node map[string]any)
	walk = func(prefix string, node map[string]any) {
		for key, val := range node {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if child, ok := val.(map[string]any); ok {
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

	// Honor spec.policies.selfHealing.mode before doing any work: observe means
	// the incident is recorded (the caller already did that) and nothing is
	// proposed. This runs ahead of both the AI planner and the rule-based path so
	// observe never spends an API call.
	if reason := p.proposalSuppressedByMode(ctx, diag.PersonaRef.Name, incidentName(incident)); reason != "" {
		return &ProposalResult{SkipReason: reason}, nil
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

	// Read the live workload once, before either planning path. The persona is a
	// point-in-time import that drifts, so every stated fact and every cap below
	// is grounded in this observation rather than in the persona. When it cannot
	// be read, ref records ManagedBy=unknown, which is treated as owned.
	obs, workloadRef := p.observeWorkload(ctx, diag.PersonaRef)

	// AI path: when a planner is configured, try it first across all signal
	// types. Any failure degrades gracefully to the deterministic rules below
	// (mirroring diagnosis/ai.go's degrade-to-rules behavior).
	if p.planner != nil {
		result, err := p.proposeWithPlanner(ctx, diag, incident, obs, workloadRef)
		if err != nil {
			p.logger.V(0).Info("AI remediation planning failed, falling back to rules",
				"error", err, "category", diag.Category)
		} else if result != nil {
			return result, nil
		}
	}

	switch diag.SuggestedAction {
	case "resource-adjustment":
		return p.proposeResourceAdjustment(ctx, diag, incident, workloadRef)
	case "restart":
		return &ProposalResult{SkipReason: "restart remediation not automated in Phase 2b"}, nil
	default:
		return &ProposalResult{SkipReason: fmt.Sprintf("unsupported action type: %s", diag.SuggestedAction)}, nil
	}
}

// proposeResourceAdjustment creates a RemediationAction for resource limit changes.
//
// workloadRef is the live workload read in Propose. Every number this path
// states, and the cap it applies, come from it whenever it resolved.
func (p *Proposer) proposeResourceAdjustment(
	ctx context.Context,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
	workloadRef *dorguv1.WorkloadRef,
) (*ProposalResult, error) {
	// Read the current ApplicationPersona: it is the object being patched, and
	// its prior values are the rollback target. It is NOT the source of the
	// numbers below.
	persona, err := p.getApplicationPersona(ctx, diag.PersonaRef)
	if err != nil {
		return nil, fmt.Errorf("getting application persona: %w", err)
	}

	if personaLimits(persona) == nil && !hasObservedLimits(workloadRef) {
		return &ProposalResult{SkipReason: "persona has no resource limits configured"}, nil
	}

	// Determine which resource to adjust and by how much, sized against the
	// live workload.
	change, err := p.calculateResourceChange(diag, persona, workloadRef)
	if err != nil {
		return nil, fmt.Errorf("calculating resource change: %w", err)
	}
	if change.skipReason != "" {
		return &ProposalResult{SkipReason: change.skipReason}, nil
	}
	if change.patch == nil {
		return &ProposalResult{SkipReason: "no applicable resource adjustment"}, nil
	}

	patchJSON, err := json.Marshal(change.patch)
	if err != nil {
		return nil, fmt.Errorf("marshalling patch: %w", err)
	}

	prePatchJSON, err := json.Marshal(change.prePatch)
	if err != nil {
		return nil, fmt.Errorf("marshalling pre-patch state: %w", err)
	}

	namespace := diag.PersonaRef.Namespace
	if namespace == "" {
		namespace = defaultNamespace
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
			Explanation: change.explanation,
			Confidence:  fmt.Sprintf("%.2f", diag.Confidence),
			WorkloadRef: workloadRef,
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

	// Say so when the guardrail, not the diagnosis, picked the number. Measured
	// against the live workload, so the disclosure describes the change the
	// cluster will actually see.
	if discloseGroundedBlastRadiusClamp(action, workloadRef) {
		p.logger.Info("proposed change sits at the blast-radius cap; disclosing the clamp in the plan",
			"action", action.Name, "confidence", action.Spec.Confidence)
	}

	// Run safety checks against a probe whose pre-patch state is the live
	// workload's, so the 2x cap bounds the real change rather than a change
	// relative to a stale persona.
	safetyResult, err := p.safety.Check(ctx, groundedSafetyProbe(action, workloadRef))
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

// resourceAdjustment is the outcome of sizing a resource change: the persona
// patch, the persona values it replaces (the rollback target), the prose shown
// to the user, or a reason nothing should be proposed.
type resourceAdjustment struct {
	patch       map[string]any
	prePatch    map[string]any
	explanation string
	skipReason  string
}

// calculateResourceChange determines what resource change to apply based on signals.
func (p *Proposer) calculateResourceChange(
	diag diagnosis.Diagnosis,
	persona *dorguv1.ApplicationPersona,
	ref *dorguv1.WorkloadRef,
) (resourceAdjustment, error) {
	switch primarySignalType(diag) {
	case detection.SignalOOMKilled, detection.SignalMemorySaturationCrit, detection.SignalMemorySaturationHigh:
		return p.calculateLimitIncrease(diag, persona, ref, resourceKeyMemory)

	case detection.SignalCPUSaturationHigh, detection.SignalCPUSaturationCritical:
		return p.calculateLimitIncrease(diag, persona, ref, resourceKeyCPU)

	case detection.SignalCrashLoopBackOff:
		// CrashLoop with OOM correlation → memory increase.
		if hasOOMCorrelation(diag) {
			return p.calculateLimitIncrease(diag, persona, ref, resourceKeyMemory)
		}
		// CrashLoop without OOM → skip.
		return resourceAdjustment{}, nil

	default:
		return resourceAdjustment{}, nil
	}
}

// calculateLimitIncrease sizes a limit increase for one resource dimension.
//
// The baseline is the LIVE container's limit whenever the workload resolved.
// Sizing off the persona is what turned a 2x cap into a 4.5x jump on the
// cluster (32Mi live, 96Mi in a months-old persona, 144Mi applied), and what
// made the plan narrate a 96Mi limit for a pod that had 32Mi.
//
// Two refusals fall out of that grounding:
//   - the live container does not set this limit at all: raising one would
//     introduce a field the workload has never had, and a CPU limit in
//     particular starts throttling a service that was not throttled before.
//   - nothing is known: no live value and no persona value, so there is nothing
//     honest to compute from.
func (p *Proposer) calculateLimitIncrease(
	diag diagnosis.Diagnosis,
	persona *dorguv1.ApplicationPersona,
	ref *dorguv1.WorkloadRef,
	key string,
) (resourceAdjustment, error) {
	personaValue := personaLimit(persona, key)
	path := pathPrefixLimits + key

	baseline, groundedInLive := observedValue(ref, path)
	switch {
	case groundedInLive:
		// Sized against what is running.
	case resolved(ref):
		return resourceAdjustment{skipReason: fmt.Sprintf(
			"container %q on Deployment %s does not set a %s limit, so raising one would introduce a field the workload has never had",
			ref.Container, ref.Name, key)}, nil
	case personaValue != "":
		baseline = personaValue
	default:
		return resourceAdjustment{}, nil
	}

	qty, err := resource.ParseQuantity(baseline)
	if err != nil {
		return resourceAdjustment{}, fmt.Errorf("parsing %s quantity %q: %w", key, baseline, err)
	}

	multiplier := increaseMultiplier(key, diag.Severity)
	newValue := scaleQuantity(qty, multiplier, key)

	adjustment := resourceAdjustment{
		patch:       buildNestedMap("spec", "resources", "limits", key, newValue),
		explanation: limitIncreaseExplanation(diag, ref, key, baseline, newValue, multiplier, groundedInLive),
	}
	// The pre-patch state is the persona's prior value, because it is the
	// persona this patch rewrites and the persona a rollback restores. It is
	// deliberately not the cap baseline: the cap is measured against the live
	// workload (see groundedSafetyProbe), the rollback against the persona.
	//
	// When the persona records nothing at this path, the patch introduces the
	// key, and there is no prior persona value to restore. The live value is
	// used instead, so a rollback returns the persona to what the workload
	// actually has rather than to an empty snapshot the executor would reject.
	rollbackValue := personaValue
	if rollbackValue == "" {
		rollbackValue = baseline
	}
	adjustment.prePatch = buildNestedMap("spec", "resources", "limits", key, rollbackValue)
	return adjustment, nil
}

// increaseMultiplier picks the increase for a dimension and severity, never
// exceeding the blast-radius cap.
func increaseMultiplier(key string, severity detection.Severity) float64 {
	warning, critical := memoryIncreaseWarning, memoryIncreaseCritical
	if key == resourceKeyCPU {
		warning, critical = cpuIncreaseWarning, cpuIncreaseCritical
	}

	multiplier := warning
	if severity == detection.SeverityCritical {
		multiplier = critical
	}
	return min(multiplier, maxResourceMultiplier)
}

// scaleQuantity multiplies a quantity, rendering it in the format each
// dimension is normally written in.
func scaleQuantity(qty resource.Quantity, multiplier float64, key string) string {
	if key == resourceKeyCPU {
		return resource.NewMilliQuantity(int64(float64(qty.MilliValue())*multiplier), resource.DecimalSI).String()
	}
	return resource.NewQuantity(int64(float64(qty.Value())*multiplier), resource.BinarySI).String()
}

// limitIncreaseExplanation states where every number came from.
//
// When the workload resolved, the sentence quotes only live values and names
// the Deployment and container they were read from. When it did not, it says
// plainly that the figure is the persona's record and may have drifted, rather
// than presenting an import from weeks ago as the current limit.
func limitIncreaseExplanation(
	diag diagnosis.Diagnosis,
	ref *dorguv1.WorkloadRef,
	key, baseline, newValue string,
	multiplier float64,
	groundedInLive bool,
) string {
	percent := (multiplier - 1) * 100

	if !groundedInLive {
		return fmt.Sprintf(
			"Increase the %s limit from %s to %s (a %.0f%% increase) after a %s signal at %s severity. "+
				"%s is the limit recorded in the persona: the live Deployment could not be read, so the running value may differ.",
			key, baseline, newValue, percent, primarySignalType(diag), diag.Severity, baseline)
	}

	return fmt.Sprintf(
		"Increase the %s limit from %s to %s (a %.0f%% increase) after a %s signal at %s severity. "+
			"%s is the live limit read from container %q on Deployment %s/%s.",
		key, baseline, newValue, percent, primarySignalType(diag), diag.Severity,
		baseline, ref.Container, ref.Namespace, ref.Name)
}

// personaLimits returns the persona's recorded limits, or nil.
func personaLimits(persona *dorguv1.ApplicationPersona) *dorguv1.ResourceValues {
	if persona == nil || persona.Spec.Resources == nil {
		return nil
	}
	return persona.Spec.Resources.Limits
}

// personaLimit returns the persona's recorded limit for one dimension. It is
// the patch target and the rollback value, never a statement of current state.
func personaLimit(persona *dorguv1.ApplicationPersona, key string) string {
	limits := personaLimits(persona)
	if limits == nil {
		return ""
	}
	if key == resourceKeyCPU {
		return limits.CPU
	}
	return limits.Memory
}

// hasObservedLimits reports whether the live container sets any limit, which
// makes a proposal possible even when the persona records none.
func hasObservedLimits(ref *dorguv1.WorkloadRef) bool {
	return resolved(ref) && ref.ObservedResources != nil && ref.ObservedResources.Limits != nil
}

// getApplicationPersona fetches the ApplicationPersona by reference.
func (p *Proposer) getApplicationPersona(ctx context.Context, ref *dorguv1.PersonaReference) (*dorguv1.ApplicationPersona, error) {
	if ref.Kind != kindApplicationPersona {
		return nil, fmt.Errorf("unsupported persona kind for resource adjustment: %s", ref.Kind)
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = defaultNamespace
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
func buildNestedMap(keys ...string) map[string]any {
	if len(keys) < 2 {
		return nil
	}

	value := keys[len(keys)-1]
	pathKeys := keys[:len(keys)-1]

	result := map[string]any{pathKeys[len(pathKeys)-1]: value}
	for i := len(pathKeys) - 2; i >= 0; i-- {
		result = map[string]any{pathKeys[i]: result}
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
