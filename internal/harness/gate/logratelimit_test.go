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

package gate

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// errorLog collects the ERROR lines a logger receives.
type errorLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *errorLog) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{})
}

func (l *errorLog) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func (l *errorLog) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) == 0 {
		return ""
	}
	return l.lines[len(l.lines)-1]
}

func TestLogLimiter(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	l := newLogLimiter(5 * time.Minute)

	ok, held := l.allow("site", now)
	assert.True(t, ok, "the first occurrence always logs")
	assert.Zero(t, held)

	for i := 1; i <= 3; i++ {
		ok, _ = l.allow("site", now.Add(time.Duration(i)*time.Minute))
		assert.False(t, ok, "repeats inside the interval are held back")
	}

	ok, _ = l.allow("other", now.Add(time.Minute))
	assert.True(t, ok, "each site has its own budget")

	ok, held = l.allow("site", now.Add(5*time.Minute))
	assert.True(t, ok, "the interval is over")
	assert.Equal(t, 3, held, "what was held back is reported, not forgotten")

	ok, held = l.allow("site", now.Add(6*time.Minute))
	assert.False(t, ok)
	assert.Zero(t, held)
}

// The #51 follow-up: while the ClusterPersona cannot be read, every proposal
// attempt used to log the same three ERROR lines (deny-list, rate limit, mode)
// once per incident per cycle. The refusal is not throttled; only the log is.
func TestUnreadableClusterPersona_LogIsRateLimitedButEveryProposalIsStillRefused(t *testing.T) {
	log := &errorLog{}
	clock := fixedNow
	c := failingListClient(t, isKind[*dorguv1.ClusterPersonaList], persona("256Mi"), deployment("256Mi"))
	g := New(c, log.logger(), WithClock(func() time.Time { return clock }))

	for i := range 20 {
		d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)
		require.Equal(t, ResultRefuse, d.Result, "attempt %d must still be refused", i)
	}

	assert.Equal(t, 1, log.count("cannot read ClusterPersona excludeNamespaces"),
		"twenty refused proposals are one deny-list log line")

	clock = clock.Add(unreadableLogInterval)
	run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

	assert.Equal(t, 2, log.count("cannot read ClusterPersona excludeNamespaces"),
		"it logs again once the interval has passed")
	assert.Contains(t, log.last(), "suppressedSinceLastLog",
		"and says how many it held back")
}

func TestUnreadableClusterPersona_EachSiteLogsOnce(t *testing.T) {
	log := &errorLog{}
	g := New(failingListClient(t, isKind[*dorguv1.ClusterPersonaList]), log.logger(),
		WithClock(func() time.Time { return fixedNow }))

	for range 5 {
		cand := planCandidate(personaUpdateStep(memPatch("384Mi")))
		d := newDecision(&cand, PhaseProposal, metav1Now())
		require.NoError(t, modePolicyStage{g}.Run(t.Context(), &cand, d))

		g.logUnreadable("rateLimit", errors.New("x"), "cannot read the ClusterPersona rate limit; refusing until it is readable")
	}

	assert.Equal(t, 1, log.count("selfHealing mode"))
	assert.Equal(t, 1, log.count("rate limit"))
}
