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

package planner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics/metricstest"
)

type countingInner struct {
	calls int
	plan  *RemediationPlan
	err   error
}

func (c *countingInner) PlanRemediation(context.Context, RemediationContext) (*RemediationPlan, error) {
	c.calls++
	return c.plan, c.err
}

func cachedTestPlan() *RemediationPlan {
	return &RemediationPlan{
		RootCause:  "limit too low",
		Confidence: 0.8,
		Steps: []PlannedStep{{
			Order: 1, Type: "persona-update", Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`),
		}},
	}
}

func baseContext() RemediationContext {
	return RemediationContext{
		Diagnosis: oomDiagnosis("my-app"),
		Workload: &WorkloadContext{Ref: &dorguv1.WorkloadRef{
			Kind: "Deployment", Name: "my-app", Namespace: "default", Container: "app",
			ObservedResources: &dorguv1.ObservedResources{Limits: &dorguv1.ResourceValues{Memory: "256Mi"}},
		}},
	}
}

func planCache(t *testing.T, result string) float64 {
	t.Helper()
	return metricstest.Counter(t, "dorgu_llm_cache_total", map[string]string{"purpose": llm.PurposePlanning, "result": result})
}

func TestCachedPlanner_UnchangedSituationAsksOnce(t *testing.T) {
	inner := &countingInner{plan: cachedTestPlan()}
	p := NewCachedPlanner(inner, time.Hour, logr.Discard())
	hitsBefore, missesBefore := planCache(t, "hit"), planCache(t, "miss")

	for range 10 {
		plan, err := p.PlanRemediation(context.Background(), baseContext())
		require.NoError(t, err)
		assert.Equal(t, "limit too low", plan.RootCause)
	}

	assert.Equal(t, 1, inner.calls, "ten cycles over an unchanged situation are one model call")
	assert.Equal(t, 9.0, planCache(t, "hit")-hitsBefore)
	assert.Equal(t, 1.0, planCache(t, "miss")-missesBefore)
}

// Review V1: the gate stamps WorkloadRef.ObservedAt with its clock on every
// proposal, so a fingerprint that includes the stamp never matches twice.
func TestPlanFingerprint_IgnoresWhenTheWorkloadWasObserved(t *testing.T) {
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	first, later := baseContext(), baseContext()
	first.Workload.Ref.ObservedAt = &metav1.Time{Time: at}
	later.Workload.Ref.ObservedAt = &metav1.Time{Time: at.Add(time.Minute)}

	assert.Equal(t, PlanFingerprint(first), PlanFingerprint(later),
		"ObservedAt moves every cycle and says nothing about the situation")
}

func TestCachedPlanner_AMovingObservationClockStillHitsTheCache(t *testing.T) {
	inner := &countingInner{plan: cachedTestPlan()}
	p := NewCachedPlanner(inner, time.Hour, logr.Discard())
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

	for i := range 10 {
		rc := baseContext()
		rc.Workload.Ref.ObservedAt = &metav1.Time{Time: at.Add(time.Duration(i) * time.Minute)}
		_, err := p.PlanRemediation(context.Background(), rc)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, inner.calls)
}

// The stable fields that DO describe the situation must still change the key.
func TestPlanFingerprint_StableWorkloadFieldsStillMatter(t *testing.T) {
	base := PlanFingerprint(baseContext())
	tests := map[string]func(*dorguv1.WorkloadRef){
		"kind":      func(r *dorguv1.WorkloadRef) { r.Kind = "StatefulSet" },
		"name":      func(r *dorguv1.WorkloadRef) { r.Name = "other" },
		"namespace": func(r *dorguv1.WorkloadRef) { r.Namespace = "prod" },
		"container": func(r *dorguv1.WorkloadRef) { r.Container = "sidecar" },
		"managedBy": func(r *dorguv1.WorkloadRef) { r.ManagedBy = "helm" },
		"image":     func(r *dorguv1.WorkloadRef) { r.ObservedImage = "app:2" },
		"requests": func(r *dorguv1.WorkloadRef) {
			r.ObservedResources.Requests = &dorguv1.ResourceValues{CPU: "100m"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			rc := baseContext()
			mutate(rc.Workload.Ref)
			assert.NotEqual(t, base, PlanFingerprint(rc))
		})
	}
}

func TestCachedPlanner_ChangedSituationAsksAgain(t *testing.T) {
	tests := map[string]func(*RemediationContext){
		"the live limit changed": func(rc *RemediationContext) {
			rc.Workload.Ref.ObservedResources.Limits.Memory = "512Mi"
		},
		"the diagnosis category changed": func(rc *RemediationContext) { rc.Diagnosis.Category = "deployment" },
		"the severity escalated":         func(rc *RemediationContext) { rc.Diagnosis.Severity = "warning" },
		"the persona's resources changed": func(rc *RemediationContext) {
			rc.AppPersona = &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
				Spec: dorguv1.ApplicationPersonaSpec{
					Resources: &dorguv1.ResourceConstraints{Limits: &dorguv1.ResourceValues{Memory: "384Mi"}},
				},
			}
		},
		"an earlier proposal was rejected": func(rc *RemediationContext) {
			ra := dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "ra-1"}}
			ra.Status.Phase = "Rejected"
			rc.PastRemediations = []dorguv1.RemediationAction{ra}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			inner := &countingInner{plan: cachedTestPlan()}
			p := NewCachedPlanner(inner, time.Hour, logr.Discard())

			_, err := p.PlanRemediation(context.Background(), baseContext())
			require.NoError(t, err)

			changed := baseContext()
			mutate(&changed)
			_, err = p.PlanRemediation(context.Background(), changed)
			require.NoError(t, err)

			assert.Equal(t, 2, inner.calls, "a changed situation is a new question")
		})
	}
}

func TestPlanFingerprint_IgnoresWhatMovesWhileTheSituationDoesNot(t *testing.T) {
	a := baseContext()
	b := baseContext()
	b.Signals = nil
	b.Workload.Replicas, b.Workload.ReadyReplicas = 3, 1
	for i := range b.Diagnosis.Contributing {
		b.Diagnosis.Contributing[i].Signal.Message = "restart count 99"
		b.Diagnosis.Contributing[i].Signal.DetectedAt = time.Now().Add(time.Hour)
	}
	b.Diagnosis.Summary = "a differently worded summary"
	b.Diagnosis.Confidence = 0.1

	assert.Equal(t, PlanFingerprint(a), PlanFingerprint(b))
}

func TestPlanFingerprint_PastRemediationOrderDoesNotMatter(t *testing.T) {
	mk := func(names ...string) RemediationContext {
		rc := baseContext()
		for _, n := range names {
			rc.PastRemediations = append(rc.PastRemediations,
				dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: n}})
		}
		return rc
	}
	assert.Equal(t, PlanFingerprint(mk("a", "b")), PlanFingerprint(mk("b", "a")))
	assert.NotEqual(t, PlanFingerprint(mk("a")), PlanFingerprint(mk("a", "b")))
}

func TestCachedPlanner_FailureIsRememberedSoAnOutageIsNotRetriedEveryCycle(t *testing.T) {
	boom := errors.New("model unavailable")
	inner := &countingInner{err: boom}
	p := NewCachedPlanner(inner, time.Hour, logr.Discard())

	for range 10 {
		plan, err := p.PlanRemediation(context.Background(), baseContext())
		assert.Nil(t, plan)
		assert.ErrorIs(t, err, boom)
	}
	assert.Equal(t, 1, inner.calls)
}

func TestCachedPlanner_DisabledAsksEveryTime(t *testing.T) {
	inner := &countingInner{plan: cachedTestPlan()}
	p := NewCachedPlanner(inner, 0, logr.Discard())

	for range 3 {
		_, err := p.PlanRemediation(context.Background(), baseContext())
		require.NoError(t, err)
	}
	assert.Equal(t, 3, inner.calls)
}

func TestCachedPlanner_CallersCannotEditTheCachedPlan(t *testing.T) {
	inner := &countingInner{plan: cachedTestPlan()}
	p := NewCachedPlanner(inner, time.Hour, logr.Discard())

	first, err := p.PlanRemediation(context.Background(), baseContext())
	require.NoError(t, err)
	first.Steps[0].Type = "tampered"
	inner.plan.Steps[0].Patch[0] = 'X'

	second, err := p.PlanRemediation(context.Background(), baseContext())
	require.NoError(t, err)
	assert.Equal(t, "persona-update", second.Steps[0].Type)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`, string(second.Steps[0].Patch))

	second.Steps[0].Patch[0] = 'Y'
	third, err := p.PlanRemediation(context.Background(), baseContext())
	require.NoError(t, err)
	assert.JSONEq(t, `{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`, string(third.Steps[0].Patch))
}

func TestClonePlan_NilAndEmptyPatch(t *testing.T) {
	assert.Nil(t, clonePlan(nil))
	out := clonePlan(&RemediationPlan{Steps: []PlannedStep{{Order: 1}}})
	require.Len(t, out.Steps, 1)
	assert.Empty(t, out.Steps[0].Patch)
}
