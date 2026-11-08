package gate

import (
	"slices"
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// Observability of the gate: what each run decided, as counters and timings.
//
// None of this decides anything. The gate's answer is the Decision; this file
// only reports it, so the numbers in /metrics are the numbers the gate acted on.
// Every label value is an enum from the catalog below, never message text, an
// object name or anything a model wrote (KPI I5).

// Labels for dorgu_gate_decisions_total beyond the Stage* and Guardrail* enums.
const (
	// MetricPhaseApply labels the apply-time check of a ticket (CheckTicket). It is
	// a metric phase only: the gate runs two phases, and apply re-runs one of them.
	MetricPhaseApply = "Apply"

	// StageComplete is the stage label of a run that no stage refused.
	StageComplete = "complete"

	// StageTicket is the stage label of an apply-time refusal that is about the
	// ticket itself (missing, expired, forged) rather than one gate stage.
	StageTicket = "Ticket"

	// ReasonNone is the reason of a pass.
	ReasonNone = "none"

	// ReasonGateError is the reason of a run that could not decide (a cluster read
	// failed). The caller treats it as "do not proceed".
	ReasonGateError = "gate-error"

	// resultRefuse, resultDeferred, resultPass, resultClamp and resultError are the
	// values of the result label of dorgu_gate_decisions_total.
	resultRefuse   = "refuse"
	resultDeferred = "deferred"
	resultPass     = "pass"
	resultClamp    = "clamp"
	resultError    = "error"

	// ReasonOther bounds a stage or reason the catalog does not list, so a new
	// refusal that forgot the catalog cannot mint an unbounded label.
	ReasonOther = "other"
)

// refusalSite is one place the gate refuses, as a stage and a reason enum.
type refusalSite struct {
	Stage     string
	Guardrail string

	// approvalOnly and proposalOnly narrow the phases the refusal can occur in. A
	// site with neither can occur in both.
	approvalOnly bool
	proposalOnly bool

	// deferred marks a refusal that is not a refusal in the sense of the KPIs: the
	// plan is not wrong, something Dorgu already did stands in for it. It is counted
	// as result="deferred", never "refuse", and is not persisted as a refusal.
	deferred bool
}

func (s refusalSite) phases() []Phase {
	switch {
	case s.approvalOnly:
		return []Phase{PhaseApprovalRecheck}
	case s.proposalOnly:
		return []Phase{PhaseProposal}
	default:
		return []Phase{PhaseProposal, PhaseApprovalRecheck}
	}
}

// refusalCatalog lists every (stage, reason) the gate refuses with. It is the
// bound on dorgu_gate_decisions_total's labels, the list the series are
// pre-registered from, and what TestObserve_EveryRefusalSiteIsInTheCatalog checks
// the source against. Add a refusal to the gate and the test fails until it is
// listed here.
var refusalCatalog = []refusalSite{
	{Stage: StageValidate, Guardrail: GuardrailTarget},
	{Stage: StageValidate, Guardrail: GuardrailStepType},
	{Stage: StageValidate, Guardrail: GuardrailActionClass},
	{Stage: StageValidate, Guardrail: GuardrailFieldAllowlist},
	{Stage: StageValidate, Guardrail: GuardrailStepCommand},
	{Stage: StageValidate, Guardrail: GuardrailPatchShape},

	{Stage: StageGround, Guardrail: GuardrailWorkloadAmbiguous},
	{Stage: StageGround, Guardrail: GuardrailPersona},
	{Stage: StageGround, Guardrail: GuardrailNoProposalRecord, approvalOnly: true},
	{Stage: StageGround, Guardrail: GuardrailSpecChanged, approvalOnly: true},
	{Stage: StageGround, Guardrail: GuardrailWorkloadUnbound, approvalOnly: true},
	{Stage: StageGround, Guardrail: GuardrailWorkloadGone, approvalOnly: true},
	{Stage: StageGround, Guardrail: GuardrailLiveWorkload, approvalOnly: true},

	{Stage: StageOwnership, Guardrail: GuardrailOwnerCommands},

	{Stage: StageGuardrails, Guardrail: GuardrailNothingLeft, approvalOnly: true},
	{Stage: StageGuardrails, Guardrail: GuardrailNoPlan, approvalOnly: true},
	{Stage: StageGuardrails, Guardrail: GuardrailNothingApplies},
	{Stage: StageGuardrails, Guardrail: GuardrailDenyList},
	{Stage: StageGuardrails, Guardrail: GuardrailOperatorNamespace},
	{Stage: StageGuardrails, Guardrail: GuardrailRateLimit, proposalOnly: true},
	{Stage: StageGuardrails, Guardrail: GuardrailConcurrent},
	{Stage: StageGuardrails, Guardrail: GuardrailFailedCooldown},
	{Stage: StageGuardrails, Guardrail: GuardrailAbsentField},
	{Stage: StageGuardrails, Guardrail: GuardrailNoBaseline},
	{Stage: StageGuardrails, Guardrail: GuardrailBlastRadius},

	{Stage: StageModePolicy, Guardrail: GuardrailSelfHealingMode},
	{Stage: StageModePolicy, Guardrail: GuardrailPolicyUnreadable},
	{Stage: StageModePolicy, Guardrail: GuardrailUnknownMode},

	{Stage: StageDedupCooldown, Guardrail: GuardrailActiveAction, deferred: true},
	{Stage: StageDedupCooldown, Guardrail: GuardrailRejection},
}

// clampReasons are the guardrails that narrow a plan instead of refusing it.
var clampReasons = []string{
	GuardrailAbsentField, GuardrailBlastRadius, GuardrailNoBaseline,
	GuardrailAutoExecutable, GuardrailFieldAllowlist, GuardrailVerifyBounds,
}

// ticketOnlyReasons are the apply-time refusals that exist only because a ticket
// is not a gate decision: it can be missing, forged, expired or not what the gate
// would now derive. They are the guardrails of the TicketProblems CheckTicket and
// the rollback planner build themselves.
var ticketOnlyReasons = []string{
	GuardrailTicket, GuardrailNotApproved, GuardrailPatchShape, GuardrailTicketExpired,
	GuardrailNoProposalRecord, GuardrailWorkloadGone, GuardrailWorkloadAmbiguous, GuardrailLiveWorkload,
	GuardrailTicketMismatch, GuardrailNoPlan, GuardrailNoBaseline, GuardrailPersona,
	GuardrailNotTicketed, GuardrailClusterUnreadable,
}

// applyReasons are every reason an apply-time check can refuse with: the ticket's
// own, plus any refusal the re-run of the approval re-check can return.
func applyReasons() []string {
	reasons := slices.Clone(ticketOnlyReasons)
	for _, s := range refusalCatalog {
		if !slices.Contains(reasons, s.Guardrail) {
			reasons = append(reasons, s.Guardrail)
		}
	}
	return reasons
}

// stageNames are the pipeline's stages in order.
var stageNames = []string{
	StageValidate, StageGround, StageOwnership, StageGuardrails, StageModePolicy, StageDedupCooldown,
}

// normalizeRefusal returns the (stage, reason) to label a refusal with: the pair
// itself when the catalog lists it, the stage with ReasonOther when only the
// reason is unknown, and ReasonOther for both when the stage is unknown too.
func normalizeRefusal(stage, reason string) (string, string) {
	if !slices.Contains(stageNames, stage) {
		return ReasonOther, ReasonOther
	}
	for _, s := range refusalCatalog {
		if s.Stage == stage && s.Guardrail == reason {
			return stage, reason
		}
	}
	return stage, ReasonOther
}

// knownGuardrail bounds the guardrail label of dorgu_guardrail_verdicts_total.
func knownGuardrail(guardrail string) string {
	if slices.Contains(allGuardrails(), guardrail) {
		return guardrail
	}
	return ReasonOther
}

// allGuardrails is every guardrail name a verdict can carry.
func allGuardrails() []string {
	all := []string{
		GuardrailTarget, GuardrailStepType, GuardrailAutoExecutable, GuardrailFieldAllowlist, GuardrailVerdictsReset,
		GuardrailVerifyBounds, GuardrailLiveWorkload, GuardrailPersona, GuardrailOwnership, GuardrailOwnerCommands,
		GuardrailDenyList, GuardrailRateLimit, GuardrailConcurrent, GuardrailFailedCooldown, GuardrailBlastRadius,
		GuardrailAbsentField, GuardrailNothingLeft, GuardrailNothingApplies, GuardrailSelfHealingMode,
		GuardrailActiveAction, GuardrailRejection, GuardrailWorkloadGone, GuardrailWorkloadAmbiguous,
		GuardrailNoBaseline, GuardrailPatchShape, GuardrailStepCommand, GuardrailOperatorNamespace,
		GuardrailNoProposalRecord, GuardrailWorkloadUnbound, GuardrailSpecChanged, GuardrailNoPlan,
		GuardrailActionClass, GuardrailPolicyUnreadable, GuardrailUnknownMode, GuardrailNotTicketed,
		GuardrailClusterUnreadable, GuardrailTicket, GuardrailNotApproved, GuardrailTicketMismatch, GuardrailTicketExpired,
	}
	return all
}

// Deferred reports whether the plan was held back only because a proposal for the
// same incident or fix is already outstanding (DedupCooldown/active-remediation).
// The decision is still not Allowed, so nothing proceeds, but it is not a
// refusal: the first proposal is waiting for a human, and the same answer comes
// back on every health-check cycle until it is decided. It is counted as
// result="deferred", is not persisted as ProposalRefused, and stays out of the
// refusal mix. A rejection cooldown, by contrast, is a refusal.
func (d *Decision) Deferred() bool {
	stage, guardrail := d.RefusedBy()
	return d.Result == ResultRefuse && stage == StageDedupCooldown && guardrail == GuardrailActiveAction
}

// recordDecision counts one finished run under a metric phase.
func recordDecision(phase string, d *Decision) {
	switch d.Result {
	case ResultRefuse:
		stage, guardrail := d.RefusedBy()
		stage, reason := normalizeRefusal(stage, guardrail)
		result := resultRefuse
		if d.Deferred() {
			result = resultDeferred
		}
		metrics.RecordGateDecision(phase, stage, result, reason)
	case ResultClamp:
		metrics.RecordGateDecision(phase, StageComplete, resultClamp, clampReason(d))
	default:
		metrics.RecordGateDecision(phase, StageComplete, resultPass, ReasonNone)
	}
	for _, v := range d.Verdicts {
		metrics.RecordGuardrailVerdict(knownGuardrail(v.Guardrail), v.Verdict)
	}
}

// clampReason is the first guardrail that narrowed the plan.
func clampReason(d *Decision) string {
	for _, v := range d.Verdicts {
		if v.Verdict == dorguv1.GateVerdictClamp && slices.Contains(clampReasons, v.Guardrail) {
			return v.Guardrail
		}
	}
	return ReasonOther
}

// noPlanReason is the sentence on the verdict of a cleared approval that has
// nothing left to ticket.
const noPlanReason = "the gate cleared the plan but nothing is left to apply after it was clamped"

// RefuseNoPlan records the no-plan refusal on a decision that was allowed but
// carries nothing to issue a ticket for, so status.gate shows who refused and why
// like any other refusal. It adds the verdict only and does not count: Gate.Run
// already turns this into a refusal, and counts it once, for a candidate that had
// a change; this is for the caller that reaches an allowed decision with no ticket
// for any other reason (the controller's own advisory check).
func (d *Decision) RefuseNoPlan() {
	d.refuse(StageGuardrails, GuardrailNoPlan, noPlanReason, nil)
}

// hasAutoApplicableChange reports whether the candidate asks for a change that
// would write the persona: an auto-executable persona-update step with a patch, or
// a legacy persona-update action with one.
func hasAutoApplicableChange(cand *Candidate) bool {
	for i := range cand.Steps {
		step := &cand.Steps[i]
		if step.AutoExecutable && step.Type == dorguv1.StepTypePersonaUpdate && step.Patch != nil && len(step.Patch.Raw) > 0 {
			return true
		}
	}
	return len(cand.Steps) == 0 && cand.Action.Type == dorguv1.ActionTypePersonaUpdate &&
		cand.Action.Patch != nil && len(cand.Action.Patch.Raw) > 0
}

// sealApproval makes an approval that was cleared but cannot be ticketed a
// refusal before it is counted: a plan that asked for a change, and whose clamps
// left no persona patch to issue, was not cleared for anything. An advisory plan
// (it asked for no change) stays allowed and carries no ticket.
func sealApproval(cand *Candidate, d *Decision) {
	if !d.Allowed() || !hasAutoApplicableChange(cand) {
		return
	}
	if _, err := d.Ticket(); err != nil {
		d.RefuseNoPlan()
	}
}

// recordGateError counts a run that could not decide, at the stage that failed.
func recordGateError(phase, stage string) {
	if !slices.Contains(stageNames, stage) {
		stage = ReasonOther
	}
	metrics.RecordGateDecision(phase, stage, resultError, ReasonGateError)
}

// recordApply counts one apply-time ticket check.
func recordApply(problem *TicketProblem, err error) {
	switch {
	case err != nil:
		metrics.RecordGateDecision(MetricPhaseApply, StageTicket, resultError, ReasonGateError)
	case problem != nil:
		reason := problem.Guardrail
		if !slices.Contains(applyReasons(), reason) {
			reason = ReasonOther
		}
		metrics.RecordGateDecision(MetricPhaseApply, StageTicket, resultRefuse, reason)
	default:
		metrics.RecordGateDecision(MetricPhaseApply, StageComplete, resultPass, ReasonNone)
	}
}

// observeStage records how long one stage took.
func observeStage(phase, stage string, started time.Time) {
	metrics.RecordGateStageDuration(phase, stage, time.Since(started))
}

// PreRegisteredDecisionSeries is every dorgu_gate_decisions_total series created
// at zero at start-up. Exposed so a test in its own binary can check the registry
// has them before anything has run.
func PreRegisteredDecisionSeries() []metrics.GateSeries {
	series := make([]metrics.GateSeries, 0, 256)
	phases := []string{string(PhaseProposal), string(PhaseApprovalRecheck), MetricPhaseApply}

	for _, phase := range phases {
		series = append(series,
			metrics.GateSeries{Phase: phase, Stage: StageComplete, Result: resultPass, Reason: ReasonNone},
			metrics.GateSeries{Phase: phase, Stage: StageTicket, Result: resultError, Reason: ReasonGateError})
	}
	for _, phase := range []string{string(PhaseProposal), string(PhaseApprovalRecheck)} {
		for _, reason := range append(slices.Clone(clampReasons), ReasonOther) {
			series = append(series, metrics.GateSeries{Phase: phase, Stage: StageComplete, Result: resultClamp, Reason: reason})
		}
		for _, stage := range stageNames {
			series = append(series, metrics.GateSeries{Phase: phase, Stage: stage, Result: resultError, Reason: ReasonGateError})
		}
	}
	for _, site := range refusalCatalog {
		result := resultRefuse
		if site.deferred {
			result = resultDeferred
		}
		for _, phase := range site.phases() {
			series = append(series, metrics.GateSeries{Phase: string(phase), Stage: site.Stage, Result: result, Reason: site.Guardrail})
		}
	}
	for _, reason := range applyReasons() {
		series = append(series, metrics.GateSeries{Phase: MetricPhaseApply, Stage: StageTicket, Result: resultRefuse, Reason: reason})
	}
	return series
}

func init() {
	stagePairs := make([]metrics.GateStagePair, 0, 3*(len(stageNames)+1))
	for _, phase := range []string{string(PhaseProposal), string(PhaseApprovalRecheck), MetricPhaseApply} {
		for _, stage := range append(slices.Clone(stageNames), ReasonOther) {
			stagePairs = append(stagePairs, metrics.GateStagePair{Phase: phase, Stage: stage})
		}
	}
	metrics.PreRegisterGate(PreRegisteredDecisionSeries(), stagePairs, append(allGuardrails(), ReasonOther))
}

// IsStage reports whether name is a stage of the pipeline. A caller that gets a
// refusal as (stage, reason) uses it to tell a gate refusal from a skip that is
// the proposer's own, or a deferral.
func IsStage(name string) bool { return slices.Contains(stageNames, name) }
