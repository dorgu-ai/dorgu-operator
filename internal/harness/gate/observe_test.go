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

func TestObserve_TheMetricIsPreRegisteredForEveryCatalogedRefusal(t *testing.T) {
	families, err := ctrlmetrics.Registry.Gather()
	require.NoError(t, err)
	have := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "dorgu_gate_decisions_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			parts := map[string]string{}
			for _, l := range m.GetLabel() {
				parts[l.GetName()] = l.GetValue()
			}
			have[parts["phase"]+"/"+parts["stage"]+"/"+parts["result"]+"/"+parts["reason"]] = true
		}
	}
	for _, site := range refusalCatalog {
		for _, phase := range site.phases() {
			key := string(phase) + "/" + site.Stage + "/refuse/" + site.Guardrail
			assert.True(t, have[key], "series %s is not pre-registered: a dashboard would show no data until it first fires", key)
		}
	}
	assert.True(t, have["Apply/"+StageTicket+"/refuse/"+GuardrailTicketExpired])
}

// TestObserve_EveryRefusalSiteIsInTheCatalog is the audit, as a test. It reads
// this package's source and requires that every d.refuse(Stage, Guardrail, ...)
// names a stage and guardrail pair the catalog lists, and that every
// TicketProblem built at apply names a guardrail the Apply catalog lists. A new
// refusal that forgets the catalog fails here, not in a dashboard.
func TestObserve_EveryRefusalSiteIsInTheCatalog(t *testing.T) {
	consts := packageStringConsts(t)
	files := sourceFiles(t)

	// Sites whose guardrail is a variable, covered by their own behavioural tests:
	// guardrails.go (deny-list or operator-namespace) and stage_ground.go
	// (the prior record's guardrail).
	dynamic := map[string]bool{"guardrails.go": true, "stage_ground.go": true}

	listed := map[string]bool{}
	for _, s := range refusalCatalog {
		listed[s.Stage+"/"+s.Guardrail] = true
	}
	ticket := map[string]bool{}
	for _, g := range applyReasons() {
		ticket[g] = true
	}

	sites := 0
	{
		for base, file := range files {
			ast.Inspect(file, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "refuse" || len(x.Args) < 2 || base == "types.go" {
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
						assert.True(t, dynamic[base], "%s: refuse() with a variable guardrail must be listed as dynamic", base)
						return true
					}
					assert.True(t, listed[stage+"/"+guardrail], "%s: refuse(%s, %s) is not in the refusal catalog", base, stage, guardrail)
				case *ast.CompositeLit:
					id, ok := x.Type.(*ast.Ident)
					if !ok || id.Name != "TicketProblem" || len(x.Elts) == 0 {
						return true
					}
					sites++
					if _, keyed := x.Elts[0].(*ast.KeyValueExpr); keyed {
						return true
					}
					if g, ok := constValue(consts, x.Elts[0]); ok {
						assert.True(t, ticket[g], "%s: TicketProblem{%s} is not in the Apply catalog", base, g)
					}
				}
				return true
			})
		}
	}
	assert.Greater(t, sites, 40, "the scan found suspiciously few refusal sites")
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
