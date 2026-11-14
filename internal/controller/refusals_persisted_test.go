package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics/metricstest"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// B6 1.10 and 1.12 at the controller: every refusal is a condition, a Kubernetes
// Event and a count, with a reason enum; tickets, heal credit and verification
// are counted.

func metricValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	return metricstest.Counter(t, name, labels)
}

// events drains what a FakeRecorder was handed.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func withRecorder(rc *RemediationController) *record.FakeRecorder {
	rec := record.NewFakeRecorder(32)
	rc.Recorder = rec
	return rec
}

func hasEvent(evs []string, eventType, reason string) bool {
	for _, e := range evs {
		if strings.HasPrefix(e, eventType+" "+reason+" ") {
			return true
		}
	}
	return false
}

// --- refused at approval ----------------------------------------------------

func TestB6_ARefusalAtApprovalIsAnEventAndCountedAndNamesItsReason(t *testing.T) {
	ra := lcRA("Approved")
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), lcDeployment("64Mi"), ra.DeepCopy()})
	rec := withRecorder(rc)
	refused := map[string]string{"result": "refuse", "phase": "ApprovalRecheck", "stage": "Guardrails", "reason": gate.GuardrailNothingLeft}
	decisions0 := metricValue(t, "dorgu_gate_decisions_total", refused)
	tickets0 := metricValue(t, "dorgu_tickets_total", map[string]string{"result": metrics.TicketRefusedAtApproval})

	_, got := lcReconcile(t, rc, ra)

	require.Equal(t, RemediationPhaseFailed, got.Status.Phase)
	evs := drainEvents(rec)
	assert.True(t, hasEvent(evs, "Warning", "DorguGuardrailsRefused"), "events: %v", evs)
	assert.Equal(t, decisions0+1, metricValue(t, "dorgu_gate_decisions_total", refused))
	assert.Equal(t, tickets0+1, metricValue(t, "dorgu_tickets_total", map[string]string{"result": metrics.TicketRefusedAtApproval}))
	cond := lcCondition(got, dorguv1.ConditionGateRefused)
	require.NotNil(t, cond)
	assert.Equal(t, gate.ReasonEnum(gate.GuardrailNothingLeft), cond.Reason)
}

func TestB6_AnObserveModeRefusalAtApprovalIsAStageNamedEvent(t *testing.T) {
	ra := lcRA("Approved")
	observe := lcClusterPersona(dorguv1.SelfHealingModeObserve)
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), observe, ra.DeepCopy()})
	rec := withRecorder(rc)

	_, got := lcReconcile(t, rc, ra)

	require.Equal(t, RemediationPhaseFailed, got.Status.Phase)
	assert.True(t, hasEvent(drainEvents(rec), "Warning", "DorguModePolicyRefused"))
}

// --- ticket issuance --------------------------------------------------------

func TestB6_IssuingATicketIsAnEventAndACountAndAStageDuration(t *testing.T) {
	ra := lcRA("Approved", func(ra *dorguv1.RemediationAction) {
		ra.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
	})
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rec := withRecorder(rc)
	issued := map[string]string{"result": metrics.TicketIssued}
	tickets0 := metricValue(t, "dorgu_tickets_total", issued)
	decide0 := metricstest.HistogramCount(t, "dorgu_stage_duration_seconds", map[string]string{"stage": metrics.StageProposalToDecision})
	apply0 := metricstest.HistogramCount(t, "dorgu_stage_duration_seconds", map[string]string{"stage": metrics.StageDecisionToApplied})

	_, got := lcReconcile(t, rc, ra)

	require.Equal(t, RemediationPhaseApplying, got.Status.Phase)
	assert.True(t, hasEvent(drainEvents(rec), "Normal", "DorguTicketIssued"))
	assert.Equal(t, tickets0+1, metricValue(t, "dorgu_tickets_total", issued))
	assert.Equal(t, decide0+1, metricstest.HistogramCount(t, "dorgu_stage_duration_seconds", map[string]string{"stage": metrics.StageProposalToDecision}))
	assert.Equal(t, apply0+1, metricstest.HistogramCount(t, "dorgu_stage_duration_seconds", map[string]string{"stage": metrics.StageDecisionToApplied}))
}

// --- refused at apply -------------------------------------------------------

func TestB6_AnExpiredTicketIsCountedAsExpiredAndRefusedByName(t *testing.T) {
	ra := lcRA("Ticketed", func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.ExpiresAt = metav1.NewTime(time.Now().Add(-time.Second))
	})
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rec := withRecorder(rc)
	expired := map[string]string{"result": metrics.TicketExpired}
	expired0 := metricValue(t, "dorgu_tickets_total", expired)
	refused0 := metricValue(t, "dorgu_tickets_total", map[string]string{"result": metrics.TicketRefusedAtApply})

	_, got := lcReconcile(t, rc, ra)

	require.Equal(t, RemediationPhaseExpired, got.Status.Phase, "an expired ticket ends Expired, not Failed")
	cond := lcCondition(got, dorguv1.ConditionGateRefused)
	require.NotNil(t, cond)
	assert.Equal(t, "TicketExpired", cond.Reason)
	expiredCond := lcCondition(got, ConditionExpired)
	require.NotNil(t, expiredCond)
	assert.Equal(t, dorguv1.ReasonTicketExpired, expiredCond.Reason)
	assert.True(t, hasEvent(drainEvents(rec), "Warning", "DorguTicketRefused"))
	assert.Equal(t, expired0+1, metricValue(t, "dorgu_tickets_total", expired))
	assert.Equal(t, refused0, metricValue(t, "dorgu_tickets_total", map[string]string{"result": metrics.TicketRefusedAtApply}),
		"an expired ticket is counted as expired, not also as refused")
}

func TestB6_AnotherApplyRefusalIsCountedAsRefusedAtApply(t *testing.T) {
	ra := lcRA("Ticketed", func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.WorkloadUID = "uid-someone-else" })
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rec := withRecorder(rc)
	labels := map[string]string{"result": metrics.TicketRefusedAtApply}
	before := metricValue(t, "dorgu_tickets_total", labels)

	_, got := lcReconcile(t, rc, ra)

	require.Equal(t, RemediationPhaseFailed, got.Status.Phase)
	assert.True(t, hasEvent(drainEvents(rec), "Warning", "DorguTicketRefused"))
	assert.Equal(t, before+1, metricValue(t, "dorgu_tickets_total", labels))
}

func TestB6_EveryApplyRefusalCarriesAConditionReason(t *testing.T) {
	// A ticket check that errors (the cluster could not be read) used to leave no
	// GateRefused condition at all, because its PreconditionError had no guardrail.
	ra := lcRA("Approved") // wrong phase for the executor
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	err := rc.Executor.Apply(context.Background(), ra)

	var pre *remediation.PreconditionError
	require.ErrorAs(t, err, &pre)
	assert.Equal(t, gate.GuardrailNotTicketed, pre.Guardrail)
}

// --- verification, heal credit, incident outcomes --------------------------

func TestB6_ACreditedHealIsCountedWithItsReason(t *testing.T) {
	ra := lcRA("Verifying", lcPatchedByTheCLI)
	rc, _ := healthyController(t, ra, lcDeploymentAtGeneration(2))
	credit := map[string]string{"credited": "true", "reason": metrics.HealReasonWorkloadPatched}
	healthy := map[string]string{"result": metrics.VerificationHealthy}
	resolved := map[string]string{"outcome": IncidentOutcomeResolved}
	c0, h0, r0 := metricValue(t, "dorgu_heal_credit_total", credit), metricValue(t, "dorgu_verifications_total", healthy),
		metricValue(t, "dorgu_incidents_resolved_total", resolved)
	verified0 := metricstest.HistogramCount(t, "dorgu_stage_duration_seconds", map[string]string{"stage": metrics.StageAppliedToVerified})

	lcReconcile(t, rc, ra)

	assert.Equal(t, c0+1, metricValue(t, "dorgu_heal_credit_total", credit))
	assert.Equal(t, h0+1, metricValue(t, "dorgu_verifications_total", healthy))
	assert.Equal(t, r0+1, metricValue(t, "dorgu_incidents_resolved_total", resolved))
	assert.Equal(t, verified0+1, metricstest.HistogramCount(t, "dorgu_stage_duration_seconds", map[string]string{"stage": metrics.StageAppliedToVerified}))
}

func TestB6_ARecoveryNobodyPatchedIsCountedAsNotCredited(t *testing.T) {
	ra := lcRA("Verifying")
	rc, _ := healthyController(t, ra)
	credit := map[string]string{"credited": "false", "reason": metrics.HealReasonNothingPatched}
	resolved := map[string]string{"outcome": IncidentOutcomeSelfResolved}
	c0, r0 := metricValue(t, "dorgu_heal_credit_total", credit), metricValue(t, "dorgu_incidents_resolved_total", resolved)

	lcReconcile(t, rc, ra)

	assert.Equal(t, c0+1, metricValue(t, "dorgu_heal_credit_total", credit))
	assert.Equal(t, r0+1, metricValue(t, "dorgu_incidents_resolved_total", resolved))
}

func TestB6_ADivergedWorkloadIsCountedAsPartial(t *testing.T) {
	ra := lcRA("Verifying", lcPatchedByTheCLI)
	replaced := lcDeploymentAtGeneration(2)
	replaced.UID = "uid-replaced"
	rc, _ := healthyController(t, ra, replaced)
	credit := map[string]string{"credited": "false", "reason": metrics.HealReasonWorkloadDiverged}
	partial := map[string]string{"outcome": IncidentOutcomePartial}
	c0, p0 := metricValue(t, "dorgu_heal_credit_total", credit), metricValue(t, "dorgu_incidents_resolved_total", partial)

	lcReconcile(t, rc, ra)

	assert.Equal(t, c0+1, metricValue(t, "dorgu_heal_credit_total", credit))
	assert.Equal(t, p0+1, metricValue(t, "dorgu_incidents_resolved_total", partial))
}

func TestB6_TheHealReasonsMatchTheConditionReasons(t *testing.T) {
	assert.Equal(t, ReasonWorkloadPatchedByTicket, metrics.HealReasonWorkloadPatched)
	assert.Equal(t, ReasonNothingPatched, metrics.HealReasonNothingPatched)
	assert.Equal(t, ReasonResourcesNotTicket, metrics.HealReasonResourcesNotTicket)
	assert.Equal(t, dorguv1.ReasonWorkloadDivergedFromTicket, metrics.HealReasonWorkloadDiverged)
	assert.Equal(t, remediation.ReasonWorkloadUnreadable, metrics.HealReasonWorkloadUnreadable)
}

func TestB6_ADegradedVerificationAndItsRollbackAreCounted(t *testing.T) {
	ra := lcRA("Verifying")
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), ra.DeepCopy()}, lcOOMSignal())
	degraded := map[string]string{"result": metrics.VerificationDegraded}
	rollbacks := map[string]string{"workload_diverged": "false"}
	d0, r0 := metricValue(t, "dorgu_verifications_total", degraded), metricValue(t, "dorgu_rollbacks_total", rollbacks)
	rolled := map[string]string{"outcome": IncidentOutcomeRollback}
	o0 := metricValue(t, "dorgu_incidents_resolved_total", rolled)

	_, got := lcReconcile(t, rc, ra)

	require.Equal(t, RemediationPhaseRolledBack, got.Status.Phase)
	assert.Equal(t, d0+1, metricValue(t, "dorgu_verifications_total", degraded))
	assert.Equal(t, r0+1, metricValue(t, "dorgu_rollbacks_total", rollbacks))
	assert.Equal(t, o0+1, metricValue(t, "dorgu_incidents_resolved_total", rolled))
}

// --- proposal-time refusals create no RemediationAction ---------------------

func handoffWithIncident(t *testing.T, proposer remediation.RemediationProposer) (*HealthCheckReconciler, *record.FakeRecorder, *dorguv1.IncidentMemory) {
	t.Helper()
	_, incident := handoffFixtures()
	r := newHandoffReconciler(t, proposer)
	r.Client = fake.NewClientBuilder().WithScheme(r.Client.Scheme()).
		WithObjects(incident.DeepCopy()).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).Build()
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	return r, rec, incident
}

func incidentCondition(t *testing.T, r *HealthCheckReconciler, incident *dorguv1.IncidentMemory) *metav1.Condition {
	t.Helper()
	var got dorguv1.IncidentMemory
	require.NoError(t, r.Client.Get(context.Background(), client.ObjectKeyFromObject(incident), &got))
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == dorguv1.ConditionProposalRefused {
			return &got.Status.Conditions[i]
		}
	}
	return nil
}

func refusedProposal(stage, guardrail string) *stubProposer {
	return &stubProposer{result: &remediation.ProposalResult{
		SkipReason: "selfHealing.mode=observe: a sentence", Stage: stage, Reason: guardrail,
	}}
}

func TestB6_AProposalTimeRefusalIsAConditionAndAnEventOnTheIncident(t *testing.T) {
	r, rec, incident := handoffWithIncident(t, refusedProposal(gate.StageModePolicy, gate.GuardrailSelfHealingMode))
	diag, _ := handoffFixtures()

	r.proposeRemediation(context.Background(), diag, incident)

	cond := incidentCondition(t, r, incident)
	require.NotNil(t, cond, "a refusal that creates no RemediationAction must still be on the record")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "ModePolicy_SelfHealingMode", cond.Reason, "stage and reason enum, never message text")
	evs := drainEvents(rec)
	require.True(t, hasEvent(evs, "Warning", "DorguModePolicyRefused"), "events: %v", evs)
}

func TestB6_ARepeatedRefusalIsNotAnotherEventOrWrite(t *testing.T) {
	r, rec, incident := handoffWithIncident(t, refusedProposal(gate.StageModePolicy, gate.GuardrailSelfHealingMode))
	diag, _ := handoffFixtures()

	r.proposeRemediation(context.Background(), diag, incident)
	first := incidentCondition(t, r, incident)
	drainEvents(rec)
	r.proposeRemediation(context.Background(), diag, incident)

	assert.Empty(t, drainEvents(rec), "the same refusal every 60s cycle must not spam events")
	second := incidentCondition(t, r, incident)
	assert.Equal(t, first.LastTransitionTime, second.LastTransitionTime)
}

func TestB6_AChangedRefusalIsRecordedAgain(t *testing.T) {
	proposer := refusedProposal(gate.StageModePolicy, gate.GuardrailSelfHealingMode)
	r, rec, incident := handoffWithIncident(t, proposer)
	diag, _ := handoffFixtures()
	r.proposeRemediation(context.Background(), diag, incident)
	drainEvents(rec)

	proposer.result = &remediation.ProposalResult{Stage: gate.StageGuardrails, Reason: gate.GuardrailRateLimit, SkipReason: "limit"}
	r.proposeRemediation(context.Background(), diag, incident)

	assert.Equal(t, "Guardrails_RateLimit", incidentCondition(t, r, incident).Reason)
	assert.True(t, hasEvent(drainEvents(rec), "Warning", "DorguGuardrailsRefused"))
}

func TestB6_AProposerSkipThatIsNotTheGatesIsNotARefusal(t *testing.T) {
	r, rec, incident := handoffWithIncident(t, refusedProposal(remediation.StagePropose, remediation.SkipRestartNotAutomated))
	diag, _ := handoffFixtures()

	r.proposeRemediation(context.Background(), diag, incident)

	assert.Nil(t, incidentCondition(t, r, incident), "a capability skip is not a gate refusal")
	assert.Empty(t, drainEvents(rec))
}

func TestB6_AProposalClearsAStandingRefusal(t *testing.T) {
	proposer := refusedProposal(gate.StageModePolicy, gate.GuardrailSelfHealingMode)
	r, rec, incident := handoffWithIncident(t, proposer)
	diag, _ := handoffFixtures()
	r.proposeRemediation(context.Background(), diag, incident)
	drainEvents(rec)

	action := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "ra-1", Namespace: "shop"}}
	proposer.result = &remediation.ProposalResult{Proposed: true, Action: action}
	r.proposeRemediation(context.Background(), diag, incident)

	cond := incidentCondition(t, r, incident)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "a standing refusal must not outlive the proposal that answered it")
	assert.Equal(t, dorguv1.ReasonProposalMade, cond.Reason, "ProposalMade is for a real proposal")
}

func TestB6_AProposalIsTimedFromDetection(t *testing.T) {
	action := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "ra-1", Namespace: "shop"}}
	r, _, incident := handoffWithIncident(t, &stubProposer{result: &remediation.ProposalResult{Proposed: true, Action: action}})
	incident.Spec.Detection.FirstSeen = metav1.NewTime(time.Now().Add(-45 * time.Second))
	diag, _ := handoffFixtures()
	labels := map[string]string{"stage": metrics.StageDetectToProposal}
	before := metricstest.HistogramCount(t, "dorgu_stage_duration_seconds", labels)

	r.proposeRemediation(context.Background(), diag, incident)

	assert.Equal(t, before+1, metricstest.HistogramCount(t, "dorgu_stage_duration_seconds", labels))
}

// --- incidents: time to detect, and auto-clearing ---------------------------

func TestB6_ANewIncidentRecordsItsTimeToDetect(t *testing.T) {
	r, _ := resolverFor(t)
	diag := aiDiagnosis()
	diag.Contributing[0].Signal.DetectedAt = time.Now().Add(-20 * time.Second)
	before := metricstest.HistogramCount(t, "dorgu_incident_time_to_detect_seconds", nil)

	require.NoError(t, r.createIncident(context.Background(), personaSubject(*diag.PersonaRef), diag, metav1.Now()))

	assert.Equal(t, before+1, metricstest.HistogramCount(t, "dorgu_incident_time_to_detect_seconds", nil))
}

func TestB6_AnIncidentWithNoSignalTimeIsNotGivenAMadeUpTimeToDetect(t *testing.T) {
	r, _ := resolverFor(t)
	diag := aiDiagnosis()
	diag.Contributing = []diagnosis.ContributingSignal{}
	before := metricstest.HistogramCount(t, "dorgu_incident_time_to_detect_seconds", nil)

	require.NoError(t, r.createIncident(context.Background(), personaSubject(*diag.PersonaRef), diag, metav1.Now()))

	assert.Equal(t, before, metricstest.HistogramCount(t, "dorgu_incident_time_to_detect_seconds", nil),
		"no signal time means no measurement")
}

func TestB6_AnIncidentThatClearsOnItsOwnIsCountedAsCleared(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, _ := resolverFor(t, im, deploymentFor("platform", "checkout", 0))
	labels := map[string]string{"outcome": metrics.OutcomeCleared}
	before := metricValue(t, "dorgu_incidents_resolved_total", labels)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, before+1, metricValue(t, "dorgu_incidents_resolved_total", labels),
		"no Dorgu action was involved, so it is not a heal")
}

func TestB6_ACycleIsTimed(t *testing.T) {
	// The cycle needs a Detection engine; a reconciler with none cannot run one, so
	// the timing wrapper is exercised through its own entry point.
	before := metricstest.HistogramCount(t, "dorgu_healthcheck_cycle_duration_seconds", nil)
	timeCycle(func() {})
	assert.Equal(t, before+1, metricstest.HistogramCount(t, "dorgu_healthcheck_cycle_duration_seconds", nil))
}

// lcClusterPersona is a ClusterPersona with the given self-healing mode.
func lcClusterPersona(mode string) *dorguv1.ClusterPersona {
	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: dorguv1.ClusterPersonaSpec{
			Name:     "cluster",
			Policies: &dorguv1.ClusterPolicies{SelfHealing: &dorguv1.SelfHealingPolicy{Mode: mode}},
		},
	}
}

// The retry counter is an enum list indexed by count; it must cover every retry
// the controller allows.
func TestB6_TheRetryReasonsCoverEveryAllowedRetry(t *testing.T) {
	assert.Len(t, unknownRetryReasons, maxUnknownRetries+1)
	for n := 1; n <= maxUnknownRetries; n++ {
		assert.NotEmpty(t, unknownRetryReason(n))
	}
}

// The gate allowed the plan but its clamps left nothing to issue a ticket for: the
// refusal is named, recorded and counted like every other.
func TestB6_AClearedPlanWithNothingLeftIsRefusedByName(t *testing.T) {
	ra := lcRA("Approved")
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()})
	rec := withRecorder(rc)

	var fresh dorguv1.RemediationAction
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(ra), &fresh))
	allowed := &gate.Decision{Result: gate.ResultPass, Phase: gate.PhaseApprovalRecheck}
	require.NoError(t, rc.refuseAtApproval(context.Background(), rc.Logger, &fresh, allowed))

	// The count is the gate's, once, in Gate.Run (TestObserve_ANoPlanApprovalIsCountedOnlyAsARefusal);
	// this is the controller's fallback for an allowed decision with no ticket.
	cond := lcCondition(&fresh, dorguv1.ConditionGateRefused)
	require.NotNil(t, cond)
	assert.Equal(t, gate.ReasonEnum(gate.GuardrailNoPlan), cond.Reason)
	assert.True(t, hasEvent(drainEvents(rec), "Warning", "DorguGuardrailsRefused"))
	require.NotNil(t, fresh.Status.Gate)
	stage, guardrail := "", ""
	for _, v := range fresh.Status.Gate.Verdicts {
		if v.Verdict == dorguv1.GateVerdictRefuse {
			stage, guardrail = v.Stage, v.Guardrail
		}
	}
	assert.Equal(t, gate.StageGuardrails, stage, "status.gate must name the refusing stage, which is how a client reads it")
	assert.Equal(t, gate.GuardrailNoPlan, guardrail)
}

// ---------------------------------------------------------------------------
// B6 fix round (review of #59).

// F-1: an outstanding proposal is not a refusal. The gate holds the second
// proposal back, but nothing is wrong, so nothing is recorded against the
// incident, no Warning Event goes out, and it is counted as deferred.
func TestB6Fix_AnOutstandingProposalIsNotPersistedAsARefusal(t *testing.T) {
	r, rec, incident := handoffWithIncident(t, &stubProposer{result: &remediation.ProposalResult{
		SkipReason: "active remediation already exists for incident im-1",
		Stage:      remediation.StageDeferred, Reason: gate.GuardrailActiveAction,
	}})
	diag, _ := handoffFixtures()
	deferred := map[string]string{"source": metrics.SourceDorgu, "action_type": diag.SuggestedAction, "result": metrics.ProposalDeferred}
	before := metricValue(t, "dorgu_proposals_total", deferred)

	r.proposeRemediation(context.Background(), diag, incident)

	assert.Nil(t, incidentCondition(t, r, incident), "waiting for a human is not a refusal")
	assert.Empty(t, drainEvents(rec), "no Warning Event for a proposal that is simply outstanding")
	assert.Equal(t, before+1, metricValue(t, "dorgu_proposals_total", deferred))
}

// F-3: detect-to-proposal is the machine's latency to its FIRST proposal for an
// incident. A later RemediationAction for the same incident (after a failure or a
// rejection) is not a second sample.
func TestB6Fix_DetectToProposalIsMeasuredOncePerIncident(t *testing.T) {
	labels := map[string]string{"stage": metrics.StageDetectToProposal}
	proposed := func(name string) *remediation.ProposalResult {
		return &remediation.ProposalResult{Proposed: true, Action: &dorguv1.RemediationAction{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"}}}
	}
	ra := func(name string) *dorguv1.RemediationAction {
		return &dorguv1.RemediationAction{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
			Spec:       dorguv1.RemediationActionSpec{IncidentRef: dorguv1.IncidentReference{Name: "im-1", Namespace: "shop"}},
		}
	}
	proposer := &stubProposer{result: proposed("ra-first")}
	r, _, incident := handoffWithIncident(t, proposer)
	incident.Spec.Detection.FirstSeen = metav1.NewTime(time.Now().Add(-45 * time.Second))
	diag, _ := handoffFixtures()
	require.NoError(t, r.Client.Create(context.Background(), ra("ra-first")))
	before := metricValue2(t, labels)

	r.proposeRemediation(context.Background(), diag, incident)
	assert.Equal(t, before+1, metricValue2(t, labels), "the first proposal for the incident is measured")

	// The first one failed; Dorgu proposes again hours later.
	require.NoError(t, r.Client.Create(context.Background(), ra("ra-retry")))
	proposer.result = proposed("ra-retry")
	r.proposeRemediation(context.Background(), diag, incident)
	assert.Equal(t, before+1, metricValue2(t, labels), "a retry must not add the hours since first detection to a machine latency")
}

func metricValue2(t *testing.T, labels map[string]string) uint64 {
	t.Helper()
	return metricstest.HistogramCount(t, "dorgu_stage_duration_seconds", labels)
}

// F-15: a long Event message is cut on a rune boundary.
func TestB6Fix_AnEventMessageIsNeverCutMidRune(t *testing.T) {
	rec := record.NewFakeRecorder(1)
	emit(rec, &dorguv1.IncidentMemory{}, "Warning", "DorguGroundRefused", strings.Repeat("é", maxEventMessage))

	got := <-rec.Events
	assert.True(t, utf8.ValidString(got), "the Event message must stay valid UTF-8")
	assert.LessOrEqual(t, len(strings.TrimPrefix(got, "Warning DorguGroundRefused ")), maxEventMessage)
}

// F-16: a refusal is still recorded when the incident is not in the cache yet.
func TestB6Fix_AProposalRefusalSurvivesACacheThatLagsTheIncident(t *testing.T) {
	r, rec, incident := handoffWithIncident(t, refusedProposal(gate.StageModePolicy, gate.GuardrailSelfHealingMode))
	base := r.Client
	misses := 1
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*dorguv1.IncidentMemory); ok && misses > 0 {
				misses--
				return apierrors.NewNotFound(schema.GroupResource{Group: "dorgu.io", Resource: "incidentmemories"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	diag, _ := handoffFixtures()

	r.proposeRemediation(context.Background(), diag, incident)

	assert.NotNil(t, incidentCondition(t, r, incident), "one cache miss must cost a retry, not the record")
	assert.True(t, hasEvent(drainEvents(rec), "Warning", "DorguModePolicyRefused"))
}

// F-9: a verification is counted only once its status write succeeded; a conflict
// that requeues the reconcile must not count it twice.
func TestB6Fix_AConflictedStatusWriteDoesNotCountTheVerificationTwice(t *testing.T) {
	ra := lcRA("Verifying", lcPatchedByTheCLI)
	conflicts := 1
	rc := lcControllerIntercepted(t, []client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), ra.DeepCopy(), lcDeploymentAtGeneration(2)},
		interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*dorguv1.RemediationAction); ok && conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(schema.GroupResource{Group: "dorgu.io", Resource: "remediationactions"}, obj.GetName(), errors.New("raced"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		}})
	healthy := map[string]string{"result": metrics.VerificationHealthy}
	credit := map[string]string{"credited": "true", "reason": metrics.HealReasonWorkloadPatched}
	v0, c0 := metricValue(t, "dorgu_verifications_total", healthy), metricValue(t, "dorgu_heal_credit_total", credit)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)}

	_, err := rc.Reconcile(context.Background(), req)
	require.NoError(t, err, "a conflict is requeued quietly")
	assert.Equal(t, v0, metricValue(t, "dorgu_verifications_total", healthy), "the write conflicted, so nothing was verified yet")
	assert.Equal(t, c0, metricValue(t, "dorgu_heal_credit_total", credit))

	_, err = rc.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, v0+1, metricValue(t, "dorgu_verifications_total", healthy), "counted once, after the write that succeeded")
	assert.Equal(t, c0+1, metricValue(t, "dorgu_heal_credit_total", credit))
}

// lcControllerIntercepted is lcController over a client with interceptors.
func lcControllerIntercepted(t *testing.T, objs []client.Object, funcs interceptor.Funcs, signals ...detection.Signal) *RemediationController {
	t.Helper()
	base := fake.NewClientBuilder().WithScheme(lcScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&dorguv1.RemediationAction{}, &dorguv1.IncidentMemory{}, &dorguv1.ApplicationPersona{}).Build()
	c := interceptor.NewClient(base, funcs)
	log := logr.Discard()
	engine := detection.NewEngine(log, &stubCollector{name: "lc", signals: signals})
	g := gate.New(c, log)
	return &RemediationController{
		Client: c, Gate: g, Executor: remediation.NewExecutor(c, g, log),
		Verifier: remediation.NewVerifier(engine, c, log), Rollback: remediation.NewRollback(c, log), Logger: log,
	}
}

// ---------------------------------------------------------------------------
// B6 fix round 2 (re-review of #59).

// A refusal that stood while the incident had no proposal is answered once a
// proposal is outstanding: the incident must not keep saying it was refused.
func TestB6R2_ADeferralClearsAStandingRefusal(t *testing.T) {
	proposer := refusedProposal(gate.StageModePolicy, gate.GuardrailSelfHealingMode)
	r, _, incident := handoffWithIncident(t, proposer)
	diag, _ := handoffFixtures()
	r.proposeRemediation(context.Background(), diag, incident)
	require.Equal(t, metav1.ConditionTrue, incidentCondition(t, r, incident).Status)

	proposer.result = &remediation.ProposalResult{
		SkipReason: "active remediation already exists", Stage: remediation.StageDeferred, Reason: gate.GuardrailActiveAction,
	}
	r.proposeRemediation(context.Background(), diag, incident)

	cond := incidentCondition(t, r, incident)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "a proposal is outstanding, so the earlier refusal is stale")
	assert.Equal(t, dorguv1.ReasonProposalDeferred, cond.Reason, "nothing was proposed this cycle, so it must not read ProposalMade")
	assert.Contains(t, cond.Message, "outstanding or cooling down")
	assert.NotContains(t, cond.Message, "was proposed")
}

// A refusal that was true of an open incident does not outlive its healing.
func TestB6R2_ResolvingAnIncidentClearsAStandingRefusal(t *testing.T) {
	standing := func() *dorguv1.IncidentMemory {
		im := lcIncidentObj()
		im.Status.Conditions = []metav1.Condition{{
			Type: dorguv1.ConditionProposalRefused, Status: metav1.ConditionTrue,
			Reason: "ModePolicy_SelfHealingMode", LastTransitionTime: metav1.Now(),
		}}
		return im
	}
	reason := func(t *testing.T, c client.Client) (metav1.ConditionStatus, string) {
		var im dorguv1.IncidentMemory
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: lcIncident, Namespace: lcNamespace}, &im))
		for _, cond := range im.Status.Conditions {
			if cond.Type == dorguv1.ConditionProposalRefused {
				return cond.Status, cond.Reason
			}
		}
		t.Fatal("the condition vanished")
		return "", ""
	}

	t.Run("healed by a Dorgu fix", func(t *testing.T) {
		ra := lcRA("Verifying", lcPatchedByTheCLI)
		rc, c := lcController(t, []client.Object{lcPersonaObj(lcApplied), standing(), ra.DeepCopy(), lcDeploymentAtGeneration(2)})
		lcReconcile(t, rc, ra)
		status, why := reason(t, c)
		assert.Equal(t, metav1.ConditionFalse, status)
		assert.Equal(t, dorguv1.ReasonIncidentResolved, why)
	})

	t.Run("cleared on its own by the health cycle", func(t *testing.T) {
		im := openIncident("platform", "checkout", "im-platform-checkout-crash")
		im.Status.Conditions = standing().Status.Conditions
		r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 0))
		require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))
		var got dorguv1.IncidentMemory
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
		cond := findCondition(got.Status.Conditions, dorguv1.ConditionProposalRefused)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, dorguv1.ReasonIncidentResolved, cond.Reason)
	})
}

// N-2: the rollback is counted once its status write succeeded.
func TestB6R2_AConflictedRolledBackWriteDoesNotCountTheRollbackTwice(t *testing.T) {
	ra := lcRA("Verifying")
	conflicts := 1
	rc := lcControllerIntercepted(t, []client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), ra.DeepCopy(), lcDeployment(lcBaseline)},
		interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*dorguv1.RemediationAction); ok && conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(schema.GroupResource{Group: "dorgu.io", Resource: "remediationactions"}, obj.GetName(), errors.New("raced"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		}},
		lcOOMSignal())
	// The second pass finds the persona already restored by the first, so the
	// label depends on what it reads; both passes are summed across the labels.
	total := func() float64 {
		return metricValue(t, "dorgu_rollbacks_total", map[string]string{"workload_diverged": "false"}) +
			metricValue(t, "dorgu_rollbacks_total", map[string]string{"workload_diverged": "true"}) +
			metricValue(t, "dorgu_rollbacks_total", map[string]string{"workload_diverged": "unknown"})
	}
	before := total()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)}

	_, err := rc.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, before, total(), "the RolledBack write conflicted: not rolled back yet")

	_, err = rc.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, before+1, total(), "counted once, after the write that succeeded")
}
