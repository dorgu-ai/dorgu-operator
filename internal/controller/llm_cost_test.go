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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// KPI Q3: at most three model calls per incident. The health cycle runs every
// minute and an incident can stay open for hours, so the bound only holds if an
// unchanged incident stops costing calls. EKS QA 2026-11-08 (S10, H-3) measured
// about 2.3 diagnosis calls a minute for one incident group.
//
// These tests run the real reconciler, the real diagnosis engine with the AI
// provider, and the real proposer over counting doubles for the model, for N
// cycles over the same unchanged signals.

const (
	costCycles          = 10
	maxCallsPerIncident = 3
	costClockStep       = 20 * time.Second
)

// tickingClock returns a clock that advances by step on every read.
func tickingClock(step time.Duration) func() time.Time {
	var mu sync.Mutex
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(step)
		return now
	}
}

type callCounts struct {
	diagnosis int
	planning  int
}

func (c callCounts) total() int { return c.diagnosis + c.planning }

// countingPlanner is a planner.Planner double that counts requests.
type countingPlanner struct {
	calls int
	err   error
}

func (p *countingPlanner) PlanRemediation(_ context.Context, _ planner.RemediationContext) (*planner.RemediationPlan, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &planner.RemediationPlan{
		RootCause:  "memory limit too low",
		Confidence: 0.8,
		Steps: []planner.PlannedStep{{
			Order: 1, Type: "persona-update", Description: "raise the memory limit", Risk: "low",
			Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`),
		}},
	}, nil
}

type costScenario struct {
	name       string
	objects    []client.Object
	collector  func(c client.Client, log logr.Logger) detection.SignalCollector
	plannerErr error
	// incidents is how many IncidentMemory objects the scenario is expected to
	// produce; the bound is per incident.
	incidents int
}

func podCollector(c client.Client, log logr.Logger) detection.SignalCollector {
	return detection.NewPodCollector(c, log)
}

func nodePressureCollector(_ client.Client, _ logr.Logger) detection.SignalCollector {
	return &stubCollector{name: "nodes", signals: []detection.Signal{{
		Type:       detection.SignalNodeMemoryPressure,
		Severity:   detection.SeverityCritical,
		Category:   detection.CategoryResource,
		Source:     "node-collector",
		Message:    "Node worker-1 reports MemoryPressure",
		Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "worker-1"},
		DetectedAt: time.Now(),
	}}}
}

func costScenarios() []costScenario {
	oom := brokenApp{namespace: "apps", name: "report-worker", podSuffix: "788c95d9bc-p9pkz", oomKilled: true}
	mystery := []client.Object{
		deploymentFor("apps", "mystery", 1),
		crashLoopingPod("apps", "mystery-7c9d-abc12", "mystery"),
	}
	return []costScenario{
		{name: "attributed OOM, planner answers", objects: oom.objects(), collector: podCollector, incidents: 1},
		{name: "attributed OOM, planner is down", objects: oom.objects(), collector: podCollector,
			plannerErr: errors.New("model unavailable"), incidents: 1},
		{name: "unattributed crash loop", objects: mystery, collector: podCollector, incidents: 1},
		{name: "cluster-scoped node pressure", collector: nodePressureCollector, incidents: 0},
	}
}

// costFixture is the production wiring over doubles: real reconciler, real
// diagnosis engine with the AI provider, real proposer behind the plan cache.
type costFixture struct {
	r     *HealthCheckReconciler
	c     client.Client
	model *countingModel
	plan  *countingPlanner
}

func newCostFixture(t *testing.T, sc costScenario, aiOpts ...diagnosis.AIOption) *costFixture {
	t.Helper()

	c := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithObjects(sc.objects...).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.IncidentMemory{}, &dorguv1.ApplicationPersona{}, &dorguv1.RemediationAction{}).
		Build()

	logger, _ := newRecordingLogger()
	detectionEngine := detection.NewEngine(logger, sc.collector(c, logger))
	detectionEngine.SetPersonaCorrelator(detection.NewPersonaCorrelator(c, logger))

	// The gate stamps WorkloadRef.ObservedAt from its clock on every proposal.
	// A clock frozen inside one test run hid a fingerprint that included that
	// stamp (review V1), so the clock moves 20s per read, as wall time would
	// between health cycles.
	g := gate.New(c, logger, gate.WithProtectedNamespaces("dorgu-system"), gate.WithClock(tickingClock(costClockStep)))
	model := newCountingModel()
	pl := &countingPlanner{err: sc.plannerErr}

	return &costFixture{
		c:     c,
		model: model,
		plan:  pl,
		r: &HealthCheckReconciler{
			Client:    c,
			Detection: detectionEngine,
			Diagnosis: diagnosis.NewEngine(logger,
				diagnosis.NewRuleBasedProvider(logger),
				diagnosis.NewAIProvider(model, g, logger, aiOpts...),
			),
			EventStore:   &noopEventStore{},
			EventEmitter: &noopEmitter{},
			Proposer:     remediation.NewProposer(c, g, logger, remediation.WithPlanner(wrapPlanner(pl))),
			Logger:       logger,
		},
	}
}

func (f *costFixture) incidents(t *testing.T) []dorguv1.IncidentMemory {
	t.Helper()
	var list dorguv1.IncidentMemoryList
	require.NoError(t, f.c.List(context.Background(), &list))
	return list.Items
}

// runCostScenario runs costCycles reconcile cycles over unchanged signals and
// returns how many model calls they made.
func runCostScenario(t *testing.T, sc costScenario) (callCounts, int) {
	t.Helper()

	f := newCostFixture(t, sc)
	for range costCycles {
		f.r.reconcile(context.Background())
	}
	return callCounts{diagnosis: f.model.calls(), planning: f.plan.calls}, len(f.incidents(t))
}

func TestQ3_UnchangedIncidentStopsCostingModelCalls(t *testing.T) {
	for _, sc := range costScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			counts, incidents := runCostScenario(t, sc)
			t.Logf("Q3 %-34s cycles=%d incidents=%d diagnosisCalls=%d planningCalls=%d total=%d",
				sc.name, costCycles, incidents, counts.diagnosis, counts.planning, counts.total())

			assert.Equal(t, sc.incidents, incidents, "the scenario must produce the incidents it claims to")
			assert.LessOrEqual(t, counts.total(), maxCallsPerIncident*incidents,
				"model calls for %d unchanged cycles of %d incident(s): %+v", costCycles, incidents, counts)
		})
	}
}

// An unattributed incident is still reported, rule-based: only the model call is
// withheld.
func TestQ3_UnattributedIncidentIsRecordedRuleBasedWithoutTheModel(t *testing.T) {
	sc := costScenarios()[2]
	f := newCostFixture(t, sc)

	f.r.reconcile(context.Background())

	items := f.incidents(t)
	require.Len(t, items, 1)
	assert.Equal(t, AttributionUnattributed, items[0].Spec.Attribution)
	require.NotNil(t, items[0].Spec.RootCause)
	assert.NotEqual(t, "ai-enhanced", items[0].Spec.RootCause.Provider,
		"a group the model never saw cannot be recorded as AI-enhanced")
	assert.Zero(t, f.model.calls())
}

// A change in the signals is a new question, whatever else stays the same.
func TestQ3_ANewlyBrokenApplicationAsksAgainAndTheOldOneDoesNot(t *testing.T) {
	first := brokenApp{namespace: "apps", name: "report-worker", podSuffix: "788c95d9bc-p9pkz", oomKilled: true}
	second := brokenApp{namespace: "apps", name: "frontend", podSuffix: "b4867d9b4-cddt2", oomKilled: true}
	f := newCostFixture(t, costScenario{objects: first.objects(), collector: podCollector})

	for range 3 {
		f.r.reconcile(context.Background())
	}
	require.Equal(t, 1, f.model.calls(), "one broken application, three cycles")

	for _, obj := range second.objects() {
		require.NoError(t, f.c.Create(context.Background(), obj))
	}
	for range 3 {
		f.r.reconcile(context.Background())
	}

	assert.Equal(t, 2, f.model.calls(), "the second application is asked about once; the first stays cached")
	assert.Len(t, f.incidents(t), 2)
}

// "0 previous occurrences" fix: the prompt carries the incident's real history.
// The cache is off here, so every cycle asks and the count can be read off the
// request.
func TestPromptCarriesTheIncidentsRealOccurrenceCount(t *testing.T) {
	app := brokenApp{namespace: "apps", name: "report-worker", podSuffix: "788c95d9bc-p9pkz", oomKilled: true}
	f := newCostFixture(t, costScenario{objects: app.objects(), collector: podCollector}, diagnosis.WithCacheTTL(0))

	for range 3 {
		f.r.reconcile(context.Background())
	}

	reqs := f.model.Requests()
	require.Len(t, reqs, 3)
	assert.Contains(t, reqs[0].User, "first sighting", "a new incident has no history to claim")
	assert.NotContains(t, reqs[0].User, "0 previous")
	assert.Contains(t, reqs[1].User, "seen 1 time(s) before this cycle")
	assert.Contains(t, reqs[2].User, "seen 2 time(s) before this cycle")
}
