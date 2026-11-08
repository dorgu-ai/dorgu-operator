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

// Package gate is the one place the operator decides whether a remediation may
// proceed: the model proposes, the harness decides.
//
// Anything that wants a change made (the rule engine, the AI planner, and later
// an approval, an external agent) builds a Candidate and asks Gate.Run. The
// answer is a Decision: Pass, Clamp (the plan was narrowed and proceeds) or
// Refuse, with every verdict recorded as data. Nothing in the operator may
// enforce a safety property anywhere else; a reviewer should be able to grep
// for one and find it here.
//
// The pipeline is fixed:
//
//	Validate -> Ground -> Ownership -> Guardrails -> ModePolicy -> Dedup/Cooldown
//
// Run executes it in one of two phases. Proposal runs when a plan is about to
// become a RemediationAction. ApprovalRecheck runs when a human has approved one
// and re-measures the same plan against the cluster as it is now, because a
// proposal can be hours old.
package gate

import (
	"context"
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/workload"
)

// Phase is when the gate is being run.
type Phase string

const (
	// PhaseProposal gates a plan before it is written as a RemediationAction.
	PhaseProposal Phase = "Proposal"

	// PhaseApprovalRecheck re-gates a persisted plan on live state at approval.
	// It is always Final: nothing shapes a plan after approval, so every
	// invariant is enforced whatever the caller asked for.
	PhaseApprovalRecheck Phase = "ApprovalRecheck"
)

// valid reports whether the gate knows the phase. An unknown phase is refused
// rather than run as a permissive one.
func (p Phase) valid() bool { return p == PhaseProposal || p == PhaseApprovalRecheck }

// Result is the overall outcome of a gate run.
type Result string

const (
	// ResultPass means nothing was changed or refused.
	ResultPass Result = "Pass"

	// ResultClamp means the plan was narrowed (a field removed, a bound applied)
	// and the narrowed plan may proceed.
	ResultClamp Result = "Clamp"

	// ResultRefuse means nothing may proceed.
	ResultRefuse Result = "Refuse"
)

// Stage names, as recorded on verdicts.
const (
	StageValidate      = "Validate"
	StageGround        = "Ground"
	StageOwnership     = "Ownership"
	StageGuardrails    = "Guardrails"
	StageModePolicy    = "ModePolicy"
	StageDedupCooldown = "DedupCooldown"
)

// Guardrail names, as recorded on verdicts.
const (
	GuardrailTarget          = "target"
	GuardrailStepType        = "step-type"
	GuardrailAutoExecutable  = "auto-executable"
	GuardrailFieldAllowlist  = "field-allowlist"
	GuardrailVerdictsReset   = "verdicts-recomputed"
	GuardrailVerifyBounds    = "verify-bounds"
	GuardrailLiveWorkload    = "live-workload"
	GuardrailPersona         = "persona"
	GuardrailOwnership       = "ownership"
	GuardrailOwnerCommands   = "owner-commands"
	GuardrailDenyList        = RuleDenyList
	GuardrailRateLimit       = RuleRateLimit
	GuardrailConcurrent      = RuleConcurrent
	GuardrailFailedCooldown  = RuleFailedCooldown
	GuardrailBlastRadius     = RuleBlastRadius
	GuardrailAbsentField     = "absent-field"
	GuardrailNothingLeft     = "nothing-left"
	GuardrailNothingApplies  = "nothing-applies"
	GuardrailSelfHealingMode = "self-healing-mode"
	GuardrailActiveAction    = "active-remediation"
	GuardrailRejection       = "rejection-cooldown"
	GuardrailSealed          = "sealed"

	// Reasons added by the #54 review (R1 to R12). Each is the machine-readable
	// reason a run was refused; callers and tests branch on these, never on the
	// message text.
	GuardrailWorkloadGone      = "workload-gone"
	GuardrailWorkloadAmbiguous = "workload-ambiguous"
	GuardrailNoBaseline        = "no-baseline"
	GuardrailPatchShape        = "patch-shape"
	GuardrailStepCommand       = "step-command"
	GuardrailOperatorNamespace = "operator-namespace"

	// Reasons added with the approval re-check (B5). Same rule: callers and tests
	// branch on these, never on message text.
	//
	// GuardrailNoProposalRecord: at approval there is no operator-written record
	// of the proposal (status.gate from a Proposal run that cleared, with a
	// workload UID in it), so there is nothing to compare the live cluster to.
	// A hand-made RemediationAction lands here.
	GuardrailNoProposalRecord = "no-proposal-record"

	// GuardrailWorkloadUnbound: the plan was proposed against no live workload, so
	// no UID exists for an approval to bind to.
	GuardrailWorkloadUnbound = "workload-unbound"

	// GuardrailSpecChanged: the spec was edited after the operator proposed it, so
	// what a human approved is not what the proposal cleared.
	GuardrailSpecChanged = "spec-changed"

	// GuardrailNoPlan: the approval carries no persona-update to clear.
	GuardrailNoPlan = "no-plan"

	// GuardrailUnknownPhase: the caller named a phase the gate does not run.
	GuardrailUnknownPhase = "unknown-phase"

	// GuardrailTicket is the apply-time check of a ticket (see CheckTicket).
	GuardrailTicket = "apply-ticket"
)

// Safety rule names. They are the Rule of a Violation and the Guardrail of the
// verdict recorded for it.
const (
	// RuleDenyList is the rule for a protected namespace.
	RuleDenyList = "deny-list"

	// RuleRateLimit is the per-persona hourly limit.
	RuleRateLimit = "rate-limit"

	// RuleConcurrent is another remediation already in flight for the persona.
	RuleConcurrent = "concurrent"

	// RuleFailedCooldown is the quiet period after a remediation that went wrong.
	RuleFailedCooldown = "failed-cooldown"

	// RuleOperatorNamespace is the refusal when the operator's own namespace is
	// unknown, so the deny-list cannot protect it.
	RuleOperatorNamespace = "operator-namespace"

	// RuleBlastRadius is the cap on how far one change may move a value.
	RuleBlastRadius = "blast-radius"
)

// Candidate is what someone wants done. It is input only: Run never modifies
// it, and returns the plan the gate would allow in the Decision.
type Candidate struct {
	// Target is the persona the change lands on.
	Target dorguv1.PersonaReference

	// Namespace and Name identify the RemediationAction this candidate is, or will
	// be. They are how the guardrails exclude the candidate itself from the
	// siblings it is measured against. Name is empty before a proposal is named.
	Namespace string
	Name      string

	// Source is the plan source, rule-based or ai-anthropic.
	Source string

	// IncidentRef is the incident the plan answers.
	IncidentRef dorguv1.IncidentReference

	// Steps is the ordered plan. The gate is the only place its steps are
	// validated: types, fields, caps, verdicts.
	Steps []dorguv1.RemediationStep

	// Action is the legacy single action, used when Steps is empty (the shape the
	// rule engine writes). It is deprecated: Steps is the plan.
	Action dorguv1.RemediationActionDetail

	// RequestedVerify is the verification and rollback the author asked for. It
	// is recorded and then ignored: those bounds are operator-owned.
	RequestedVerify *dorguv1.RemediationRollbackSpec

	// ApprovedAt is when approval was recorded (ApprovalRecheck only). It orders
	// two simultaneously approved plans for one persona.
	ApprovedAt *metav1.Time

	// Final marks a candidate no further shaping will follow. The gate then also
	// enforces the invariants earlier shaping was expected to meet, such as no
	// workload-writing command on an owned workload. A Final candidate that is
	// clamped or refused was changed after the gate ruled on it.
	Final bool

	// Facts are the inputs the dedup and cooldown stage needs.
	Facts Facts

	// prior is what the operator recorded when it proposed the plan, and
	// priorProblem says why it could not be relied on. Both are set only by
	// ApprovalCandidate, from operator-written status: a caller cannot hand the
	// gate a prior observation of its own choosing (M2).
	prior        *dorguv1.GateObserved
	priorProblem string
	priorGuard   string
}

// Facts are diagnosis- and incident-derived inputs. Nothing here is model text.
type Facts struct {
	// Incident is the incident the plan answers, when there is one.
	Incident *dorguv1.IncidentMemory

	// Severity is the live diagnosis severity.
	Severity string

	// PrimarySignal is the diagnosis's primary signal type.
	PrimarySignal string

	// TargetPath is the persona-spec path the diagnosis calls for changing, the
	// incident-independent identity of the fix. Empty when none is derivable.
	TargetPath string

	// AdvisoryAllowed lifts the completeness check: the rule engine itself declined
	// to size a change (the live container sets no such limit), so an advisory-only
	// plan is the honest outcome rather than a plan that failed to produce a fix.
	// Proposal only: Run ignores it at approval.
	AdvisoryAllowed bool
}

// Decision is the gate's answer.
type Decision struct {
	// Phase is the phase the gate ran in.
	Phase Phase

	// Result is Pass, Clamp or Refuse.
	Result Result

	// Verdicts is every guardrail evaluated, in order.
	Verdicts []dorguv1.GateVerdict

	// Steps is the plan the gate allows: the candidate's steps with refused
	// fields removed, verdicts recomputed, and invariants enforced. Use this, not
	// Candidate.Steps.
	Steps []dorguv1.RemediationStep

	// Action is the legacy single action after the same treatment.
	Action dorguv1.RemediationActionDetail

	// Verify is the verification and rollback the operator will use. It is
	// operator-owned: the author's request is not read.
	Verify dorguv1.RemediationRollbackSpec

	// Observed, Ownership and PrePatchState are what the gate read and derived
	// from the live cluster.
	Observed      *dorguv1.GateObserved
	Ownership     *dorguv1.GateOwnership
	PrePatchState *apiextensionsv1.JSON

	// WorkloadRef is the spec.workloadRef the proposer records (back-compat).
	WorkloadRef *dorguv1.WorkloadRef

	// Observation is the live Deployment read, for callers that give a planner
	// ground truth. Nil when the workload could not be resolved.
	Observation *workload.Observation

	// Persona is the ApplicationPersona read, nil when it could not be.
	Persona *dorguv1.ApplicationPersona

	// EvaluatedAt is when the gate ran.
	EvaluatedAt metav1.Time

	// refusing is the first refusing verdict.
	refusing *dorguv1.GateVerdict
}

// Allowed reports whether the plan may proceed, possibly narrowed. Only an
// explicit Pass or Clamp allows: the zero Decision, and any result the gate does
// not know, do not (L1).
func (d *Decision) Allowed() bool { return d.Result == ResultPass || d.Result == ResultClamp }

// Refusal returns the verdict that refused the plan, or nil.
func (d *Decision) Refusal() *dorguv1.GateVerdict { return d.refusing }

// RefusedBy returns the stage and guardrail that refused the plan, or empty
// strings when nothing was refused.
func (d *Decision) RefusedBy() (stage, guardrail string) {
	if d.refusing == nil {
		return "", ""
	}
	return d.refusing.Stage, d.refusing.Guardrail
}

// SkipReason is the sentence a refusal is reported as. Guardrail refusals keep
// the "safety check failed: [[rule] message]" form callers have always seen.
// Empty when the plan was not refused.
func (d *Decision) SkipReason() string {
	if d.refusing == nil {
		return ""
	}
	if d.refusing.Stage != StageGuardrails {
		return d.refusing.Reason
	}

	reasons := make([]string, 0, 2)
	for _, v := range d.Verdicts {
		if v.Stage == StageGuardrails && v.Verdict == dorguv1.GateVerdictRefuse {
			reasons = append(reasons, fmt.Sprintf("[%s] %s", v.Guardrail, v.Reason))
		}
	}
	return fmt.Sprintf("safety check failed: %v", reasons)
}

// Status renders the decision as the record stored on the RemediationAction.
func (d *Decision) Status() *dorguv1.GateStatus {
	return &dorguv1.GateStatus{
		Phase:         string(d.Phase),
		Result:        string(d.Result),
		EvaluatedAt:   d.EvaluatedAt,
		Observed:      d.Observed.DeepCopy(),
		Ownership:     d.Ownership.DeepCopy(),
		Verdicts:      append([]dorguv1.GateVerdict(nil), d.Verdicts...),
		PrePatchState: d.PrePatchState.DeepCopy(),
	}
}

// add records a verdict and folds it into the overall result.
func (d *Decision) add(stage, guardrail, verdict, reason string, numbers map[string]string) {
	v := dorguv1.GateVerdict{
		Stage:     stage,
		Guardrail: guardrail,
		Verdict:   verdict,
		Reason:    truncate(reason, maxReasonLength),
		Numbers:   numbers,
	}
	d.Verdicts = append(d.Verdicts, v)

	switch verdict {
	case dorguv1.GateVerdictRefuse:
		d.Result = ResultRefuse
		if d.refusing == nil {
			refusal := v
			d.refusing = &refusal
		}
	case dorguv1.GateVerdictClamp:
		if d.Result != ResultRefuse {
			d.Result = ResultClamp
		}
	}
}

func (d *Decision) pass(stage, guardrail, reason string, numbers map[string]string) {
	d.add(stage, guardrail, dorguv1.GateVerdictPass, reason, numbers)
}

func (d *Decision) clamp(stage, guardrail, reason string, numbers map[string]string) {
	d.add(stage, guardrail, dorguv1.GateVerdictClamp, reason, numbers)
}

func (d *Decision) refuse(stage, guardrail, reason string, numbers map[string]string) {
	d.add(stage, guardrail, dorguv1.GateVerdictRefuse, reason, numbers)
}

// maxReasonLength matches the CRD's bound on a verdict reason.
const maxReasonLength = 1024

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n-1]) + "…"
}

// Stage is one step of the pipeline. A stage records verdicts on the Decision
// and may narrow its Steps; it returns an error only when it could not decide
// (a cluster read failed), never to refuse.
type Stage interface {
	Name() string
	Run(ctx context.Context, cand *Candidate, d *Decision) error
}

// Violation is one guardrail violation, as the legacy safety checker reported
// it. The gate records each as a verdict; the type remains because the step
// narration quotes the numbers.
type Violation struct {
	// Rule identifies the guardrail (RuleDenyList, RuleBlastRadius, ...).
	Rule string

	// Message is a human-readable explanation.
	Message string

	// BlastRadius carries a blast-radius violation as numbers, set only for
	// RuleBlastRadius.
	BlastRadius *BlastRadiusViolation
}

// BlastRadiusViolation is a blast-radius refusal expressed as its inputs.
type BlastRadiusViolation struct {
	// Field is the persona-spec path, e.g. "spec.resources.limits.memory".
	Field string

	// Baseline is the value the change was measured against.
	Baseline string

	// Requested is the value the plan asked for.
	Requested string

	// Ratio is Requested over Baseline.
	Ratio float64

	// MaxRatio is the largest increase the guardrail allows.
	MaxRatio float64
}
