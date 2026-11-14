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
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// kindApplicationPersona is the only persona kind the remediation path writes to.
const kindApplicationPersona = "ApplicationPersona"

// RemediationAction phases the proposer and executor refer to.
const (
	phasePending  = "Pending"
	phaseApproved = "Approved"
	phaseTicketed = "Ticketed"
)

// defaultNamespace is the namespace a remediation lands in when the diagnosis
// carries no namespace of its own.
const defaultNamespace = "default"

const (
	// Resource increase multipliers by severity.
	memoryIncreaseWarning  = 1.5
	memoryIncreaseCritical = 2.0
	cpuIncreaseWarning     = 1.25
	cpuIncreaseCritical    = 1.5

	// maxActionNameLength is the maximum length for RemediationAction names.
	maxActionNameLength = 253

	// actionNameOverhead: "ra-" (3) + 3 separators + hash (12) = 18.
	actionNameOverhead = 18
)

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
		return gate.ResourcePathMemory
	case detection.SignalCPUSaturationHigh, detection.SignalCPUSaturationCritical, detection.SignalCPUThrottling:
		return gate.ResourcePathCPU
	case detection.SignalCrashLoopBackOff:
		if hasOOMCorrelation(diag) {
			return gate.ResourcePathMemory
		}
	}
	return ""
}

// Proposer generates RemediationAction CRDs from diagnoses. It decides nothing
// about safety itself: it builds a candidate (from the rule engine or the AI
// planner), asks the gate, and writes what the gate allows.
type Proposer struct {
	client  client.Client
	gate    *gate.Gate
	logger  logr.Logger
	planner planner.Planner

	// inFlight serialises proposals per persona. The gate's rate limit, concurrency
	// and dedup decisions read what already exists; two proposals for one persona
	// racing between that read and their Create would each see nothing and both be
	// written. Holding a persona's lock from the first gate run to the Create makes
	// the second one's gate see the first's RemediationAction (R11). The operator
	// runs a single leader, so an in-process lock is the whole of the exclusion
	// that is needed; the gate's uncached reader makes what it then reads current.
	inFlight sync.Map // persona key -> *sync.Mutex
}

// lockPersona serialises proposals for one persona and returns the unlock.
func (p *Proposer) lockPersona(ref *dorguv1.PersonaReference) func() {
	key := ref.Namespace + "/" + ref.Name
	mu, _ := p.inFlight.LoadOrStore(key, &sync.Mutex{})
	lock := mu.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// ProposerOption configures an optional Proposer dependency.
type ProposerOption func(*Proposer)

// WithPlanner injects an AI remediation planner. When set, Propose first tries
// the AI planning path and falls back to the deterministic rules on any failure.
func WithPlanner(p planner.Planner) ProposerOption {
	return func(pr *Proposer) { pr.planner = p }
}

// NewProposer creates a new Proposer. Pass WithPlanner to enable AI planning.
func NewProposer(c client.Client, g *gate.Gate, logger logr.Logger, opts ...ProposerOption) *Proposer {
	p := &Proposer{
		client: c,
		gate:   g,
		logger: logger.WithName("proposer"),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Gate returns the gate the proposer consults. It is exposed so wiring tests can
// assert the proposer and the diagnosis provider share one deny-list.
func (p *Proposer) Gate() *gate.Gate { return p.gate }

// Planner returns the AI planner, or nil when planning is disabled.
func (p *Proposer) Planner() planner.Planner { return p.planner }

// withholdFromModel reports whether diag touches any protected namespace.
func (p *Proposer) withholdFromModel(ctx context.Context, diag diagnosis.Diagnosis) bool {
	_, protected := diagnosis.ProtectedNamespace(ctx, p.gate, diag)
	return protected
}

// intent is the candidate for "do something about this diagnosis" before any
// plan exists. Running the gate on it is the preflight: it decides whether to
// plan at all (observe mode, a protected namespace, a duplicate, a cooldown)
// before an AI call is spent, and reads the live workload the planner is given.
func (p *Proposer) intent(diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) gate.Candidate {
	return gate.Candidate{
		Target:    *diag.PersonaRef,
		Namespace: diag.PersonaRef.Namespace,
		Source:    dorguv1.PlanSourceRuleBased,
		Facts:     factsFor(diag, incident),
	}
}

// factsFor extracts the inputs the gate's dedup and cooldown stage reads. None of
// it is model text: the target path and signal come from the rules.
func factsFor(diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) gate.Facts {
	return gate.Facts{
		Incident:        incident,
		SuggestedAction: diag.SuggestedAction,
		Severity:        string(diag.Severity),
		PrimarySignal:   string(primarySignalType(diag)),
		TargetPath:      diagnosisTargetPath(diag),
	}
}

// candidateFor turns a drafted RemediationAction into the candidate the gate
// rules on.
func candidateFor(action *dorguv1.RemediationAction, diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory, final bool) gate.Candidate {
	facts := factsFor(diag, incident)
	return gate.Candidate{
		Target:          action.Spec.PersonaRef,
		Namespace:       action.Namespace,
		Name:            action.Name,
		Source:          action.Spec.PlanSource,
		IncidentRef:     action.Spec.IncidentRef,
		Steps:           action.Spec.Steps,
		Action:          action.Spec.Action,
		RequestedVerify: action.Spec.Rollback,
		Final:           final,
		Facts:           facts,
	}
}

// proposerSkip is a skip that is the proposer's own, not a gate refusal.
func proposerSkip(sentence, reason string, pre *gate.Decision) *ProposalResult {
	return &ProposalResult{SkipReason: sentence, Stage: StagePropose, Reason: reason, Decision: pre}
}

// skipped converts a refusal into the result callers act on.
func skipped(dec *gate.Decision) *ProposalResult {
	stage, guardrail := dec.RefusedBy()
	if dec.Deferred() {
		stage = StageDeferred
	}
	return &ProposalResult{
		SkipReason: dec.SkipReason(),
		Stage:      stage,
		Reason:     guardrail,
		Suppressed: guardrail == gate.GuardrailRejection,
		Decision:   dec,
	}
}

// Propose generates a RemediationAction from a diagnosis. Every safety property
// is the gate's: Propose builds candidates and writes the gate's answer.
func (p *Proposer) Propose(ctx context.Context, diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) (*ProposalResult, error) {
	if diag.PersonaRef == nil {
		return proposerSkip("diagnosis has no persona reference", SkipNoPersonaRef, nil), nil
	}
	defer p.lockPersona(diag.PersonaRef)()

	// Preflight, before planning. Observe mode, a protected namespace, a rate or
	// concurrency limit, a duplicate and a recent rejection all end here, so none
	// of them can spend an AI call or send that namespace's context out. It also
	// reads the live workload once: the persona is a point-in-time import that
	// drifts, so every stated fact and every cap is grounded in this observation.
	pre, err := p.gate.Run(ctx, p.intent(diag, incident), gate.PhaseProposal)
	if err != nil {
		return nil, fmt.Errorf("gate: %w", err)
	}
	if !pre.Allowed() {
		return skipped(pre), nil
	}

	// AI path: when a planner is configured, try it first across all signal
	// types. Any failure degrades gracefully to the deterministic rules below
	// (mirroring diagnosis/ai.go's degrade-to-rules behavior).
	//
	// A diagnosis that touches a protected namespace anywhere is never given to
	// the planner (the prompt carries every contributing signal's message), and
	// falls through to the rule-based path below, which never calls a model: the
	// same enumeration the AI diagnosis provider uses, so the two cannot differ.
	if p.planner != nil && p.withholdFromModel(ctx, diag) {
		p.logger.V(1).Info("not asking the planner: the diagnosis touches a protected or unknown namespace",
			"category", diag.Category)
	} else if p.planner != nil {
		result, err := p.proposeWithPlanner(ctx, diag, incident, pre)
		if errors.Is(err, errPersistFailed) {
			return nil, err
		}
		if err != nil {
			p.logger.V(0).Info("AI remediation planning failed, falling back to rules",
				"error", err, "category", diag.Category)
		} else if result != nil {
			return result, nil
		}
	}

	switch diag.SuggestedAction {
	case "resource-adjustment":
		return p.proposeResourceAdjustment(ctx, diag, incident, pre)
	case "restart":
		return proposerSkip("restart remediation not automated in Phase 2b", SkipRestartNotAutomated, pre), nil
	default:
		return proposerSkip(fmt.Sprintf("unsupported action type: %s", diag.SuggestedAction), SkipUnsupportedAction, pre), nil
	}
}

// proposeResourceAdjustment creates a RemediationAction for resource limit changes.
//
// pre is the preflight decision: its WorkloadRef is the live workload, and every
// number this path states, and the cap it applies, come from it whenever it
// resolved.
func (p *Proposer) proposeResourceAdjustment(
	ctx context.Context,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
	pre *gate.Decision,
) (*ProposalResult, error) {
	workloadRef := pre.WorkloadRef

	// The ApplicationPersona is the object being patched, and its prior values are
	// the rollback target. It is NOT the source of the numbers below.
	persona, err := p.getApplicationPersona(ctx, diag.PersonaRef)
	if err != nil {
		return nil, fmt.Errorf("getting application persona: %w", err)
	}

	if personaLimits(persona) == nil && !hasObservedLimits(workloadRef) {
		return proposerSkip("persona has no resource limits configured", SkipNoResourceLimits, pre), nil
	}

	// Determine which resource to adjust and by how much, sized against the live
	// workload.
	change, err := p.calculateResourceChange(diag, persona, workloadRef)
	if err != nil {
		return nil, fmt.Errorf("calculating resource change: %w", err)
	}
	if change.skipReason != "" {
		return proposerSkip(change.skipReason, change.skipCode, pre), nil
	}
	if change.patch == nil {
		return proposerSkip("no applicable resource adjustment", SkipNoApplicableChange, pre), nil
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

	action := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateActionName(incident, diag.SuggestedAction),
			Namespace: namespace,
			Labels: map[string]string{
				"dorgu.io/persona-kind":      diag.PersonaRef.Kind,
				"dorgu.io/persona-name":      diag.PersonaRef.Name,
				"dorgu.io/persona-namespace": namespace,
			},
			Annotations: answersSignal(diag),
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{
				Name:      incident.Name,
				Namespace: incident.Namespace,
			},
			PersonaRef:  *diag.PersonaRef,
			TrustLevel:  2,
			Explanation: change.explanation,
			PlanSource:  dorguv1.PlanSourceRuleBased,
			Confidence:  fmt.Sprintf("%.2f", diag.Confidence),
			WorkloadRef: workloadRef,
			// steps[] is the only plan; the single-action form is derived from it
			// below, for one release of back-compat.
			Steps: []dorguv1.RemediationStep{{
				Order:          1,
				ID:             "step-1",
				Type:           dorguv1.StepTypePersonaUpdate,
				Description:    change.explanation,
				AutoExecutable: true,
				Patch:          &apiextensionsv1.JSON{Raw: patchJSON},
				PrePatchState:  &apiextensionsv1.JSON{Raw: prePatchJSON},
			}},
			Approval: &dorguv1.ApprovalSpec{
				Required: true,
			},
		},
	}
	action.Spec.Action = gate.DeriveAction(action.Spec.Steps)

	// Damp confidence when the change lands on the cap rather than where the
	// diagnosis wanted it. Measured against the live workload, so it describes
	// the change the cluster will actually see.
	if discloseGroundedBlastRadiusClamp(action, workloadRef) {
		p.logger.Info("proposed change sits at the blast-radius cap; damping confidence",
			"action", action.Name, "confidence", action.Spec.Confidence)
	}

	// The gate rules on the plan. This path never reshapes it afterwards, so the
	// candidate is final: a clamp is installed, a refusal ends the proposal.
	//
	// A workload a deployer owns gets advice only (B10 b). The persona step is measured
	// first, as a plan the gate has not seen finished, so the value the advice quotes is
	// one the guardrails cleared; it is then shaped as advice and ruled on again as final.
	owned := gate.DeployerOwned(workloadRef)
	dec, err := p.gate.Run(ctx, candidateFor(action, diag, incident, !owned), gate.PhaseProposal)
	if err != nil {
		return nil, fmt.Errorf("gate: %w", err)
	}
	if !dec.Allowed() {
		return skipped(dec), nil
	}
	installDecision(action, dec)
	if owned {
		var refusal *ProposalResult
		if dec, refusal = p.adviseAndSeal(ctx, action, diag, incident, workloadRef); refusal != nil {
			return refusal, nil
		}
	}

	// CR5-03, on this path too. A rule-based plan records no step verdicts, so
	// its at-the-cap disclosure claims a clamp that never happened just as the
	// AI path's did. The scrub is keyed on the recorded verdict rather than on
	// who authored the sentence, which is why it applies to both.
	if scrubbed := scrubFabricatedGuardrailClaims(action); scrubbed > 0 {
		metrics.RecordModelClaimsStripped(metrics.ClaimFabricatedClamp, scrubbed)
		p.logger.Info("removed guardrail claims no recorded verdict supports",
			"action", action.Name, "surfaces", scrubbed)
	}

	return p.persist(ctx, action, dec)
}

// installDecision applies what the gate allows to the action: the plan it let
// through, and the verification and rollback settings the operator owns. The
// plan's own settings were requests; these are the operator's.
func installDecision(action *dorguv1.RemediationAction, dec *gate.Decision) {
	action.Spec.Steps = dec.Steps
	action.Spec.Action = dec.Action
	if len(dec.Steps) > 0 {
		// steps[] is the only plan; the single-action form follows from it.
		action.Spec.Action = gate.DeriveAction(dec.Steps)
	}
	verify := dec.Verify
	action.Spec.Rollback = &verify
}

// errPersistFailed marks a RemediationAction that could not be recorded. It is not
// a planning failure, so Propose must not fall back to a second proposal for it.
var errPersistFailed = errors.New("recording the remediation failed")

// persist writes the RemediationAction and records the gate's decision on its
// status. The spec fields the gate also derives (workloadRef, step verdicts,
// prePatchState) stay populated for one release; status.gate is the record to
// trust.
//
// A RemediationAction whose status could not be written is not proposed (R11): it
// would sit with no phase, which no controller acts on and the dedup check cannot
// tell from a live one. The write is retried on a conflict; if it still fails the
// action is deleted and the error returned, so the caller reports a failure
// instead of a proposal that does not exist.
func (p *Proposer) persist(ctx context.Context, action *dorguv1.RemediationAction, dec *gate.Decision) (*ProposalResult, error) {
	// No step is written with the text "placeholder" (B10 d).
	action, repairErr := p.repairBeforePersist(action)
	if repairErr != nil {
		return nil, repairErr
	}
	// spec.action is a notification and nothing more (F7). It used to carry the
	// merged persona patch for one release of back-compat. Dropping it does not stop
	// a CLI from before tickets: it reads steps[] first and patches without asking
	// the operator, so the CLI has to be upgraded with the operator (CHANGELOG, B5).
	// steps[] is the only plan, and what is applied is the ticket the gate issues at
	// approval. The patch stays in the working copy above because the proposer
	// still shapes a plan through it; it is stripped here, on the way out.
	action.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}

	if err := p.client.Create(ctx, action); err != nil {
		return nil, fmt.Errorf("creating RemediationAction: %w", err)
	}
	// The generation the apiserver gave this object. The Proposed condition pins it,
	// and it must not be re-read: a spec edit that lands between Create and the
	// status write would otherwise be cleared by a gate that never saw it (F9).
	createdGeneration := action.GetGeneration()

	key := client.ObjectKeyFromObject(action)
	err := retry.OnError(retry.DefaultBackoff, retriableStatusWrite, func() error {
		var fresh dorguv1.RemediationAction
		if err := p.client.Get(ctx, key, &fresh); err != nil {
			return err
		}
		// The Proposed condition pins the spec generation the gate cleared. An
		// approval re-check refuses a spec edited after this (M2).
		fresh.Status = dorguv1.RemediationActionStatus{
			Phase: phasePending,
			Gate:  dec.Status(),
			Conditions: []metav1.Condition{{
				Type:               dorguv1.ConditionProposed,
				Status:             metav1.ConditionTrue,
				Reason:             dorguv1.ReasonGateCleared,
				Message:            "the operator's gate cleared this plan when it was proposed",
				ObservedGeneration: createdGeneration,
				LastTransitionTime: metav1.Now(),
			}},
		}
		if err := p.client.Status().Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.DeepCopyInto(action)
		return nil
	})
	if err != nil {
		p.logger.Error(err, "RemediationAction created but its status could not be written; removing it",
			"name", action.Name)
		if delErr := p.client.Delete(ctx, action); delErr != nil && !apierrors.IsNotFound(delErr) {
			p.logger.Error(delErr, "could not remove the RemediationAction that has no status; it needs deleting by hand",
				"name", action.Name, "namespace", action.Namespace)
		}
		return nil, fmt.Errorf("%w: status of %s/%s: %w", errPersistFailed, action.Namespace, action.Name, err)
	}

	return &ProposalResult{Action: action, Proposed: true, Decision: dec}, nil
}

// retriableStatusWrite reports whether a failed status write is worth another
// attempt: a conflict, or a read of an object the cache has not caught up with.
func retriableStatusWrite(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsNotFound(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err)
}

// resourceAdjustment is the outcome of sizing a resource change: the persona
// patch, the persona values it replaces (the rollback target), the prose shown
// to the user, or a reason nothing should be proposed.
type resourceAdjustment struct {
	patch       map[string]any
	prePatch    map[string]any
	explanation string
	skipReason  string
	// skipCode is skipReason as an enum (a Skip* constant).
	skipCode string
}

// calculateResourceChange determines what resource change to apply based on signals.
func (p *Proposer) calculateResourceChange(
	diag diagnosis.Diagnosis,
	persona *dorguv1.ApplicationPersona,
	ref *dorguv1.WorkloadRef,
) (resourceAdjustment, error) {
	switch primarySignalType(diag) {
	case detection.SignalOOMKilled, detection.SignalMemorySaturationCrit, detection.SignalMemorySaturationHigh:
		return p.calculateLimitIncrease(diag, persona, ref, gate.ResourceKeyMemory)

	case detection.SignalCPUSaturationHigh, detection.SignalCPUSaturationCritical, detection.SignalCPUThrottling:
		// A throttled container raises its own CPU limit, sized and bounded exactly as
		// a memory limit is for an OOM kill. The gate rules on the result.
		if skip, ok := throttledContainerMismatch(diag, ref); ok {
			return skip, nil
		}
		return p.calculateLimitIncrease(diag, persona, ref, gate.ResourceKeyCPU)

	case detection.SignalCrashLoopBackOff:
		// CrashLoop with OOM correlation → memory increase.
		if hasOOMCorrelation(diag) {
			return p.calculateLimitIncrease(diag, persona, ref, gate.ResourceKeyMemory)
		}
		// CrashLoop without OOM → skip.
		return resourceAdjustment{}, nil

	default:
		return resourceAdjustment{}, nil
	}
}

// throttledContainerMismatch refuses to size a CPU proposal from a container the signal
// is not about. The persona has one resources block and the proposer reads the live
// limit of workload.PickContainer's container (the one named like the persona, else the
// first), so with a throttled sidecar it would raise the main container's limit and
// spend a human's approval on the wrong container. The signal names the throttled
// container; when the live workload resolved and none of the throttled containers is the
// one this proposal is sized from, nothing is proposed and the advisory says why. A
// signal that names no container, or a workload that did not resolve, keeps the
// behaviour it had. This is a target-identity check, not a safety rule: the gate still
// rules on whatever is proposed.
func throttledContainerMismatch(diag diagnosis.Diagnosis, ref *dorguv1.WorkloadRef) (resourceAdjustment, bool) {
	if !gate.Resolved(ref) || ref.Container == "" {
		return resourceAdjustment{}, false
	}
	var throttled []string
	for _, cs := range diag.Contributing {
		if cs.Signal.Type != detection.SignalCPUThrottling {
			continue
		}
		name := cs.Signal.Metadata["container"]
		if name == "" || name == ref.Container {
			return resourceAdjustment{}, false
		}
		if !slices.Contains(throttled, name) {
			throttled = append(throttled, name)
		}
	}
	if len(throttled) == 0 {
		return resourceAdjustment{}, false
	}
	return resourceAdjustment{
		skipCode: SkipContainerMismatch,
		skipReason: fmt.Sprintf(
			"the throttled container %q is not %q, the container on Deployment %s that this persona's single resources block describes, "+
				"so raising that limit would not fix it; raise the CPU limit of %q by hand",
			throttled[0], ref.Container, ref.Name, throttled[0])}, true
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
	path := gate.PathPrefixLimits + key

	baseline, groundedInLive := gate.ObservedValue(ref, path)
	switch {
	case groundedInLive:
		// Sized against what is running.
	case gate.Resolved(ref):
		return resourceAdjustment{
			skipCode: SkipNoLiveLimit,
			skipReason: fmt.Sprintf(
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
	if key == gate.ResourceKeyCPU {
		warning, critical = cpuIncreaseWarning, cpuIncreaseCritical
	}

	multiplier := warning
	if severity == detection.SeverityCritical {
		multiplier = critical
	}
	return min(multiplier, gate.MaxBlastRadiusMultiplier)
}

// scaleQuantity multiplies a quantity, rendering it in the format each
// dimension is normally written in.
func scaleQuantity(qty resource.Quantity, multiplier float64, key string) string {
	if key == gate.ResourceKeyCPU {
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
	if key == gate.ResourceKeyCPU {
		return limits.CPU
	}
	return limits.Memory
}

// hasObservedLimits reports whether the live container sets any limit, which
// makes a proposal possible even when the persona records none.
func hasObservedLimits(ref *dorguv1.WorkloadRef) bool {
	return gate.Resolved(ref) && ref.ObservedResources != nil && ref.ObservedResources.Limits != nil
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

// answersSignal is the annotation that records which signal a proposal answers, so
// a later rejection of it can be read as a no to that shape of the fault only.
func answersSignal(diag diagnosis.Diagnosis) map[string]string {
	return map[string]string{gate.AnnotationAnswersSignal: string(primarySignalType(diag))}
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
