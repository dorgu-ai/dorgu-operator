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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// CachedPlanner asks the wrapped Planner at most once per planning situation.
//
// The health cycle proposes again every minute for every open incident. Most of
// the time the answer is "not now" and the planner is never reached, but when the
// gate refuses a finished plan, or the model is down, the next cycle would plan
// again, and again. A plan is a function of the diagnosis, the live workload and
// what happened to earlier proposals; while none of those change, neither does
// the question, so the earlier answer is reused until the TTL runs out. A failed
// request is remembered too, for llm.FailureTTL, so an outage is retried calmly.
type CachedPlanner struct {
	inner  Planner
	cache  *llm.Cache[cachedPlan]
	logger logr.Logger

	// empties counts, per situation, the plans in a row that came back empty. After
	// emptyPlanLimit of them the "unusable" answer is remembered for the full TTL.
	mu      sync.Mutex
	empties map[string]int
}

// emptyPlanLimit is how many consecutive empty plans for one situation make the model's
// answer for it stick for the normal cache TTL, not the short failure one. An empty
// plan costs two calls (it is asked once more), so without this a model that always
// answers nothing is asked twice every FailureTTL for as long as the incident is open.
const emptyPlanLimit = 2

// maxTrackedEmpties bounds the empties map; it is cleared when it grows past this.
const maxTrackedEmpties = 1024

type cachedPlan struct {
	plan *RemediationPlan
	err  error
}

// NewCachedPlanner wraps inner with a fingerprint cache. A ttl of zero or less
// disables caching and every call reaches inner.
func NewCachedPlanner(inner Planner, ttl time.Duration, logger logr.Logger) *CachedPlanner {
	return &CachedPlanner{
		inner:   inner,
		cache:   llm.NewCache[cachedPlan](ttl, 0),
		logger:  logger.WithName("planner-cache"),
		empties: map[string]int{},
	}
}

// PlanRemediation returns the cached plan for this situation, or asks the
// wrapped planner and remembers the answer.
func (c *CachedPlanner) PlanRemediation(ctx context.Context, rc RemediationContext) (*RemediationPlan, error) {
	key := PlanFingerprint(rc)

	if entry, hit := c.cache.Get(key); hit {
		metrics.RecordLLMCache(llm.PurposePlanning, metrics.LLMCacheHit)
		c.logger.V(1).Info("reusing the plan for an unchanged situation", "category", rc.Diagnosis.Category)
		if entry.err != nil {
			return nil, entry.err
		}
		return clonePlan(entry.plan), nil
	}
	metrics.RecordLLMCache(llm.PurposePlanning, metrics.LLMCacheMiss)

	plan, err := c.inner.PlanRemediation(ctx, rc)
	if err != nil {
		if errors.Is(err, ErrEmptyPlan) && c.countEmpty(key) >= emptyPlanLimit {
			c.logger.Info("the planner answered with an empty plan again for an unchanged situation; remembering that for the full cache TTL")
			metrics.RecordLLMCache(llm.PurposePlanning, metrics.LLMCacheUnusable)
			c.cache.Set(key, cachedPlan{err: err})
			return nil, err
		}
		c.cache.SetFor(key, cachedPlan{err: err}, llm.FailureTTL)
		return nil, err
	}
	c.forgetEmpties(key)
	c.cache.Set(key, cachedPlan{plan: clonePlan(plan)})
	return plan, nil
}

// countEmpty records one more empty plan for the situation and returns the run so far.
func (c *CachedPlanner) countEmpty(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.empties) >= maxTrackedEmpties {
		c.empties = map[string]int{}
	}
	c.empties[key]++
	return c.empties[key]
}

// forgetEmpties ends a run of empty plans: the model gave a usable one.
func (c *CachedPlanner) forgetEmpties(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.empties, key)
}

// PlanFingerprint identifies a planning situation: the diagnosis fingerprint,
// the live workload the plan is grounded in, the persona's current resources, and
// the fate of earlier proposals (a rejected fix changes what the model is told to
// avoid). It contains no model text.
func PlanFingerprint(rc RemediationContext) string {
	h := sha256.New()
	write := func(format string, args ...any) { _, _ = fmt.Fprintf(h, format, args...) }
	write("diagnosis=%s\n", diagnosis.Fingerprint(rc.Diagnosis))

	if rc.Workload != nil && rc.Workload.Ref != nil {
		writeWorkload(write, rc.Workload.Ref)
	}
	if rc.AppPersona != nil {
		if raw, err := json.Marshal(rc.AppPersona.Spec.Resources); err == nil {
			write("persona=%s/%s resources=%s\n", rc.AppPersona.Namespace, rc.AppPersona.Name, raw)
		}
	}

	past := make([]string, 0, len(rc.PastRemediations))
	for i := range rc.PastRemediations {
		ra := &rc.PastRemediations[i]
		past = append(past, fmt.Sprintf("%s:%s:%s", ra.Name, ra.Status.Phase, ra.Status.VerificationResult))
	}
	sort.Strings(past)
	for _, p := range past {
		write("past=%s\n", p)
	}

	return hex.EncodeToString(h.Sum(nil))
}

// writeWorkload adds the stable identity of the live workload to the key: what
// it is, who manages it, and what was observed on it. It lists fields one by one
// on purpose. WorkloadRef also carries ObservedAt, which the gate stamps with its
// clock on every proposal; hashing the whole struct made every cycle a new
// situation and the cache never hit (review V1). ManagedByDetail is prose that
// restates ManagedBy, so it is left out too.
func writeWorkload(write func(string, ...any), ref *dorguv1.WorkloadRef) {
	write("workload=%s/%s/%s container=%s managedBy=%s image=%s\n",
		ref.Kind, ref.Namespace, ref.Name, ref.Container, ref.ManagedBy, ref.ObservedImage)
	if ref.ObservedResources != nil {
		if raw, err := json.Marshal(ref.ObservedResources); err == nil {
			write("observed=%s\n", raw)
		}
	}
}

// clonePlan copies a plan so a caller that edits its steps cannot change what the
// cache hands to the next one.
func clonePlan(p *RemediationPlan) *RemediationPlan {
	if p == nil {
		return nil
	}
	out := *p
	out.Steps = make([]PlannedStep, len(p.Steps))
	for i, s := range p.Steps {
		s.Patch = append(json.RawMessage(nil), s.Patch...)
		out.Steps[i] = s
	}
	return &out
}
