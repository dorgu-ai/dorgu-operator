package gate

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics/metricstest"
)

// unrecognisedMode is a self-healing mode the CRD does not enumerate.
const unrecognisedMode = "yolo"

// B6 1.10 / 1.12: every gate run is counted, by phase, stage, result and reason
// enum; every refusal site is in the catalog the metric is bounded by.

func decisions(t *testing.T, phase, stage, result, reason string) float64 {
	t.Helper()
	return metricstest.Counter(t, "dorgu_gate_decisions_total",
		map[string]string{"phase": phase, "stage": stage, "result": result, "reason": reason})
}

func observeModeGate(t *testing.T) *Gate {
	t.Helper()
	return newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"),
		clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModeObserve })))
}

func TestObserve_ARefusalIsCountedByStageAndReason(t *testing.T) {
	before := decisions(t, "Proposal", StageModePolicy, "refuse", GuardrailSelfHealingMode)

	d := run(t, observeModeGate(t), planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	require.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, before+1, decisions(t, "Proposal", StageModePolicy, "refuse", GuardrailSelfHealingMode))
}

func TestObserve_APassIsCountedAsComplete(t *testing.T) {
	before := decisions(t, "Proposal", StageComplete, "pass", ReasonNone)

	d := run(t, standardGate(t), planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	require.True(t, d.Allowed(), "%v", d.SkipReason())
	assert.Equal(t, before+1, decisions(t, "Proposal", StageComplete, "pass", ReasonNone))
}

func TestObserve_AGateThatCannotDecideIsCountedAsAnError(t *testing.T) {
	c := interceptor.NewClient(fakeClient(t, persona("256Mi"), deployment("256Mi")), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*dorguv1.RemediationActionList); ok {
				return errors.NewInternalError(assert.AnError)
			}
			return c.List(ctx, list, opts...)
		},
	})
	before := decisions(t, "Proposal", StageGuardrails, "error", ReasonGateError)

	_, err := newTestGate(t, c).Run(context.Background(), planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	require.Error(t, err)
	assert.Equal(t, before+1, decisions(t, "Proposal", StageGuardrails, "error", ReasonGateError))
}

func TestObserve_EveryRunObservesItsStageDurations(t *testing.T) {
	run(t, standardGate(t), planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	families, err := ctrlmetrics.Registry.Gather()
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "dorgu_gate_stage_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.GetHistogram().GetSampleCount() == 0 {
				continue
			}
			for _, l := range m.GetLabel() {
				if l.GetName() == "stage" {
					seen[l.GetValue()] = true
				}
			}
		}
	}
	for _, stage := range []string{StageValidate, StageGround, StageOwnership, StageGuardrails, StageModePolicy, StageDedupCooldown} {
		assert.True(t, seen[stage], "no duration observed for stage %s", stage)
	}
}

func TestObserve_GuardrailVerdictsAreCounted(t *testing.T) {
	before := metricstest.Counter(t, "dorgu_guardrail_verdicts_total",
		map[string]string{"guardrail": GuardrailDenyList, "verdict": "pass"})

	run(t, standardGate(t), planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	after := metricstest.Counter(t, "dorgu_guardrail_verdicts_total",
		map[string]string{"guardrail": GuardrailDenyList, "verdict": "pass"})
	assert.Equal(t, before+1, after)
}

// --- apply-time refusals are counted under their own phase ------------------

func TestObserve_AnExpiredTicketHasItsOwnReason(t *testing.T) {
	expired := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.ExpiresAt.Time = fixedNow.Add(-time.Minute)
		ra.Status.ApplyTicket.IssuedAt.Time = fixedNow.Add(-time.Minute - TicketTTL)
	})
	before := decisions(t, MetricPhaseApply, StageTicket, "refuse", GuardrailTicketExpired)

	problem := checkTicket(t, standardGate(t), expired)

	require.NotNil(t, problem)
	assert.Equal(t, GuardrailTicketExpired, problem.Guardrail, "an expired ticket must not share a reason with a missing one")
	assert.Equal(t, before+1, decisions(t, MetricPhaseApply, StageTicket, "refuse", GuardrailTicketExpired))
}

func TestObserve_AHonouredTicketIsCountedAsAPassAtApply(t *testing.T) {
	before := decisions(t, MetricPhaseApply, StageComplete, "pass", ReasonNone)

	require.Nil(t, checkTicket(t, standardGate(t), ticketedAction()))

	assert.Equal(t, before+1, decisions(t, MetricPhaseApply, StageComplete, "pass", ReasonNone))
}

func TestObserve_ApplyChecksDoNotAlsoCountAsApprovalRechecks(t *testing.T) {
	g := standardGate(t)
	before := decisions(t, "ApprovalRecheck", StageComplete, "pass", ReasonNone)

	require.Nil(t, checkTicket(t, g, ticketedAction()))

	assert.Equal(t, before, decisions(t, "ApprovalRecheck", StageComplete, "pass", ReasonNone),
		"CheckTicket re-derives the ticket with a gate run; that is one Apply decision, not an extra approval")
}

// --- the policy reasons that used to share one enum ------------------------

func TestObserve_AnUnreadablePolicyIsNotTheObserveReason(t *testing.T) {
	// The mode stage on its own: the deny-list reads ClusterPersonas first on a full
	// run and would refuse before ModePolicy is reached.
	g := newTestGate(t, failingListClient(t, isKind[*dorguv1.ClusterPersonaList]))
	cand := planCandidate(personaUpdateStep(memPatch("384Mi")))
	d := newDecision(&cand, PhaseProposal, metav1Now())

	require.NoError(t, modePolicyStage{g}.Run(context.Background(), &cand, d))

	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailPolicyUnreadable, guardrail)
}

func TestObserve_AnUnrecognisedModeHasItsOwnReason(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"),
		clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = unrecognisedMode })))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)

	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailUnknownMode, guardrail)
}

// --- the catalog is the bound on the metric's labels ------------------------

func TestObserve_ReasonsOutsideTheCatalogAreBoundedToOther(t *testing.T) {
	stage, reason := normalizeRefusal(StageGround, "a-reason-nobody-registered")
	assert.Equal(t, StageGround, stage)
	assert.Equal(t, ReasonOther, reason)

	stage, reason = normalizeRefusal("Mystery", GuardrailBlastRadius)
	assert.Equal(t, ReasonOther, stage)
	assert.Equal(t, ReasonOther, reason)
}

// F-11: the series list the gate pre-registers is checked as data, not by reading
// a registry earlier tests have already populated. The registry itself is checked
// from a clean binary in internal/metrics/startup.
func TestObserve_ThePreRegisteredSeriesCoverEveryCatalogedRefusal(t *testing.T) {
	have := map[string]bool{}
	for _, s := range PreRegisteredDecisionSeries() {
		have[s.Phase+"/"+s.Stage+"/"+s.Result+"/"+s.Reason] = true
	}
	for _, site := range refusalCatalog {
		result := resultRefuse
		if site.deferred {
			result = "deferred"
		}
		for _, phase := range site.phases() {
			key := string(phase) + "/" + site.Stage + "/" + result + "/" + site.Guardrail
			assert.True(t, have[key], "series %s is not pre-registered: a dashboard would show no data until it first fires", key)
		}
	}
	for _, reason := range applyReasons() {
		assert.True(t, have["Apply/"+StageTicket+"/refuse/"+reason], "apply reason %s is not pre-registered", reason)
	}
	assert.True(t, have["Proposal/"+StageDedupCooldown+"/deferred/"+GuardrailActiveAction], "the deferral series")
	assert.False(t, have["Proposal/"+StageDedupCooldown+"/refuse/"+GuardrailActiveAction],
		"an outstanding proposal must not be pre-registered as a refusal")
}

// dynamicRefusalSites are the refusals whose guardrail is a variable, with every
// value the variable can take. The scan fails when the count of such sites in a
// file changes, so a new variable site cannot slip in, and requires each value to
// be in the catalog at that stage.
var dynamicRefusalSites = map[string]struct {
	stage  string
	values []string
	count  int
}{
	// guardrails.go: deny-list, or operator-namespace when the namespace is unknown.
	"guardrails.go": {StageGuardrails, []string{GuardrailDenyList, GuardrailOperatorNamespace}, 1},
	// stage_ground.go: the prior record's guardrail (operatorRecord).
	"stage_ground.go": {StageGround, []string{GuardrailNoProposalRecord, GuardrailSpecChanged}, 1},
}

// TestObserve_EveryRefusalSiteIsInTheCatalog is the audit, as a test. It reads
// this package's source and requires that every d.refuse(Stage, Guardrail, ...)
// names a stage and guardrail pair the catalog lists, that variable-guardrail
// sites are exactly the declared ones with values in the catalog, that every
// TicketProblem built at apply (positional or keyed) names a guardrail the Apply
// catalog lists, and that no refusal verdict is written around refuse() with a raw
// d.add(..., GateVerdictRefuse, ...). A new refusal that forgets the catalog fails
// here, not in a dashboard.
func TestObserve_EveryRefusalSiteIsInTheCatalog(t *testing.T) {
	consts := packageStringConsts(t)
	files := sourceFiles(t)

	listed := map[string]bool{}
	for _, s := range refusalCatalog {
		listed[s.Stage+"/"+s.Guardrail] = true
	}
	ticket := map[string]bool{}
	for _, g := range applyReasons() {
		ticket[g] = true
	}

	sites, ticketSites := 0, 0
	variableSites := map[string]int{}
	variableTickets := map[string]int{}
	for base, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "add" && base != "types.go" && len(x.Args) >= 4 {
					// Decision.add takes (stage, guardrail, verdict, reason, numbers). The verdict
					// may be a constant or a variable; either way it bypasses pass/clamp/refuse(),
					// so a refusal written this way would not be in the catalog.
					t.Errorf("%s: a verdict is written with add(); use pass(), clamp() or refuse() so a refusal is in the catalog", base)
				}
				if sel.Sel.Name != "refuse" || len(x.Args) < 2 || base == "types.go" {
					return true
				}
				sites++
				stage, sok := constValue(consts, x.Args[0])
				guardrail, gok := constValue(consts, x.Args[1])
				if !sok {
					t.Errorf("%s: refuse() stage is not a constant", base)
					return true
				}
				if !gok {
					variableSites[base]++
					return true
				}
				assert.True(t, listed[stage+"/"+guardrail], "%s: refuse(%s, %s) is not in the refusal catalog", base, stage, guardrail)
			case *ast.CompositeLit:
				id, ok := x.Type.(*ast.Ident)
				if !ok || id.Name != "TicketProblem" || len(x.Elts) == 0 {
					return true
				}
				ticketSites++
				guardrailExpr := x.Elts[0]
				if kv, keyed := x.Elts[0].(*ast.KeyValueExpr); keyed {
					guardrailExpr = nil
					for _, elt := range x.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Guardrail" {
								guardrailExpr = kv.Value
							}
						}
					}
					_ = kv
				}
				if guardrailExpr == nil {
					t.Errorf("%s: a TicketProblem with no Guardrail", base)
					return true
				}
				if g, ok := constValue(consts, guardrailExpr); ok {
					assert.True(t, ticket[g], "%s: TicketProblem{%s} is not in the Apply catalog", base, g)
				} else {
					variableTickets[base]++
				}
			}
			return true
		})
	}

	for base, got := range variableSites {
		want, declared := dynamicRefusalSites[base]
		if !assert.True(t, declared, "%s has a refuse() with a variable guardrail that is not declared in dynamicRefusalSites", base) {
			continue
		}
		assert.Equal(t, want.count, got, "%s: the number of variable-guardrail refuse() sites changed; declare the new one", base)
		for _, v := range want.values {
			assert.True(t, listed[want.stage+"/"+v], "%s: the variable guardrail can be %s, which is not in the catalog at %s", base, v, want.stage)
		}
	}
	for base := range dynamicRefusalSites {
		assert.Equal(t, dynamicRefusalSites[base].count, variableSites[base], "%s: a declared variable site disappeared", base)
	}
	// The re-run of the approval re-check at apply returns whichever guardrail
	// refused (approval.go); every one of them is in applyReasons by construction.
	assert.Equal(t, map[string]int{"approval.go": 1}, variableTickets,
		"a TicketProblem with a variable guardrail must be the declared re-derivation and nothing else")
	assert.Greater(t, sites, 30, "the scan found suspiciously few refusal sites")
	assert.Greater(t, ticketSites, 20, "the scan found suspiciously few ticket problem sites")
}

// --- helpers for the audit ---------------------------------------------------

// sourceFiles parses this package's non-test source files, keyed by file name.
func sourceFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)
		out[name] = file
	}
	return out
}

// packageStringConsts maps every package-level string constant (following
// aliases such as GuardrailDenyList = RuleDenyList) to its value.
func packageStringConsts(t *testing.T) map[string]string {
	t.Helper()
	raw := map[string]ast.Expr{}
	for _, file := range sourceFiles(t) {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						raw[name.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	out := map[string]string{}
	var resolve func(string, int) (string, bool)
	resolve = func(name string, depth int) (string, bool) {
		expr, ok := raw[name]
		if !ok || depth > 5 {
			return "", false
		}
		switch e := expr.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				v, err := strconv.Unquote(e.Value)
				return v, err == nil
			}
		case *ast.Ident:
			return resolve(e.Name, depth+1)
		}
		return "", false
	}
	for name := range raw {
		if v, ok := resolve(name, 0); ok {
			out[name] = v
		}
	}
	return out
}

func constValue(consts map[string]string, expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		v, ok := consts[e.Name]
		return v, ok
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			v, err := strconv.Unquote(e.Value)
			return v, err == nil
		}
	}
	return "", false
}

// --- B6 fix round (review of #59) -------------------------------------------

// F-1: an outstanding proposal is deferred, not refused: still not allowed, but
// counted as result="deferred" and answerable by Decision.Deferred, so nothing
// persists it as a refusal.
func TestObserve_AnOutstandingProposalIsDeferredNotRefused(t *testing.T) {
	dup := sibling("dup", "Pending", func(ra *dorguv1.RemediationAction) { ra.Spec.IncidentRef.Name = incidentOne })
	deferred := func() float64 {
		return decisions(t, "Proposal", StageDedupCooldown, "deferred", GuardrailActiveAction)
	}
	refused := func() float64 {
		return decisions(t, "Proposal", StageDedupCooldown, "refuse", GuardrailActiveAction)
	}
	deferred0, refused0 := deferred(), refused()

	d := run(t, dedupGate(t, dup), dedupCandidate(), PhaseProposal)

	require.False(t, d.Allowed(), "a deferral still stops the proposal")
	assert.True(t, d.Deferred())
	assert.Equal(t, deferred0+1, deferred())
	assert.Equal(t, refused0, refused(), "an outstanding proposal is not counted as a refusal")
}

func TestObserve_ARejectionCooldownAndEveryOtherRefusalAreNotDeferred(t *testing.T) {
	d := run(t, observeModeGate(t), planCandidate(personaUpdateStep(memPatch("384Mi"))), PhaseProposal)
	require.Equal(t, ResultRefuse, d.Result)
	assert.False(t, d.Deferred())

	rejection := &Decision{Result: ResultRefuse, refusing: &dorguv1.GateVerdict{Stage: StageDedupCooldown, Guardrail: GuardrailRejection}}
	assert.False(t, rejection.Deferred(), "a rejection cooldown is a refusal")
}

// F-6: a plan the clamps left nothing to ticket is counted once, as a refusal, and
// not first as a pass or clamp.
func TestObserve_ANoPlanApprovalIsCountedOnlyAsARefusal(t *testing.T) {
	cand := legacyCandidate(memPatch("384Mi")) // asks for a change
	cleared := &Decision{Phase: PhaseApprovalRecheck, Result: ResultPass}
	require.Empty(t, cleared.Steps, "the decision carries no persona patch to ticket")
	counts := func() (float64, float64, float64) {
		return decisions(t, "ApprovalRecheck", StageComplete, "pass", ReasonNone),
			decisions(t, "ApprovalRecheck", StageComplete, "clamp", ReasonOther),
			decisions(t, "ApprovalRecheck", StageGuardrails, "refuse", GuardrailNoPlan)
	}
	pass0, clamp0, refuse0 := counts()

	sealApproval(&cand, cleared)
	recordDecision("ApprovalRecheck", cleared)

	pass1, clamp1, refuse1 := counts()
	assert.Equal(t, ResultRefuse, cleared.Result)
	assert.Equal(t, refuse0+1, refuse1)
	assert.Equal(t, pass0, pass1, "the refusal must not also be counted as a pass")
	assert.Equal(t, clamp0, clamp1)
	stage, guardrail := cleared.RefusedBy()
	assert.Equal(t, StageGuardrails, stage)
	assert.Equal(t, GuardrailNoPlan, guardrail)
}

func TestObserve_AnAdvisoryApprovalIsStillAllowedAndCarriesNoTicket(t *testing.T) {
	advisory := Candidate{Target: target(), Namespace: testNS, Name: "ra-a"}
	d := &Decision{Phase: PhaseApprovalRecheck, Result: ResultPass}

	sealApproval(&advisory, d)

	assert.True(t, d.Allowed(), "a plan that asked for no change has nothing to ticket and nothing wrong")
}

// N-4: Gate.Run, not just sealApproval, seals an approval. A candidate that asks
// for a change through a legacy action (no steps) has no persona patch to ticket;
// the run itself must come back refused, with no-plan, and be counted once as such.
// Removing the sealApproval call from Run fails this.
func TestObserve_RunRefusesAnApprovalThatCannotBeTicketed(t *testing.T) {
	refused := func() float64 { return decisions(t, "ApprovalRecheck", StageGuardrails, "refuse", GuardrailNoPlan) }
	passed := func() float64 { return decisions(t, "ApprovalRecheck", StageComplete, "pass", ReasonNone) }
	before, passedBefore := refused(), passed()

	d := run(t, standardGate(t), legacyCandidate(memPatch("384Mi")), PhaseApprovalRecheck)

	assert.Equal(t, ResultRefuse, d.Result, "an approval that cannot be ticketed is not cleared")
	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailNoPlan, guardrail)
	assert.Equal(t, before+1, refused())
	assert.Equal(t, passedBefore, passed(), "and it is not also counted as a pass")
}

// N-1: at proposal, a concurrent in-flight sibling that answers THIS incident is a
// deferral; one that belongs to another incident and changes another field stays
// a refusal.
func TestObserve_AnInFlightSiblingOfTheSameIncidentIsDeferredNotConcurrent(t *testing.T) {
	for _, phase := range []string{"Approved", "Ticketed", "Applying", "Verifying"} {
		t.Run(phase, func(t *testing.T) {
			own := sibling("own", phase, func(ra *dorguv1.RemediationAction) { ra.Spec.IncidentRef.Name = incidentOne })
			deferred0 := decisions(t, "Proposal", StageDedupCooldown, "deferred", GuardrailActiveAction)
			concurrent0 := decisions(t, "Proposal", StageGuardrails, "refuse", GuardrailConcurrent)

			d := run(t, dedupGate(t, own), dedupCandidate(), PhaseProposal)

			require.False(t, d.Allowed())
			assert.True(t, d.Deferred())
			assert.Equal(t, deferred0+1, decisions(t, "Proposal", StageDedupCooldown, "deferred", GuardrailActiveAction))
			assert.Equal(t, concurrent0, decisions(t, "Proposal", StageGuardrails, "refuse", GuardrailConcurrent))
		})
	}
}

func TestObserve_AnInFlightSiblingOfAnotherIncidentOnAnotherFieldStaysARefusal(t *testing.T) {
	other := sibling("other", "Ticketed", func(ra *dorguv1.RemediationAction) {
		ra.Spec.IncidentRef.Name = "another-incident"
		ra.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: jsonOf(`{"spec":{"resources":{"limits":{"cpu":"500m"}}}}`)}
	})
	d := run(t, dedupGate(t, other), dedupCandidate(), PhaseProposal)

	require.False(t, d.Allowed())
	assert.False(t, d.Deferred())
	stage, guardrail := d.RefusedBy()
	assert.Equal(t, StageGuardrails, stage)
	assert.Equal(t, GuardrailConcurrent, guardrail)
}
