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

package detection

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type alwaysFails struct{ name string }

func (a alwaysFails) Name() string                            { return a.name }
func (a alwaysFails) Collect(context.Context) ([]Signal, error) { return nil, errors.New("503") }

// A collector that stays broken is reported to every caller of CollectAll, every
// time, as a *CollectionError: that is what keeps a blind verifier from reading
// Healthy. Only the ERROR log line is rate-limited, so a permanently missing
// metrics source is not 1,440 log lines a day.
func TestEngine_FailingCollectorLogIsRateLimitedButTheErrorIsNot(t *testing.T) {
	var mu sync.Mutex
	lines := 0
	logger := funcr.New(func(_, _ string) {
		mu.Lock()
		defer mu.Unlock()
		lines++
	}, funcr.Options{})

	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	e := NewEngine(logger, alwaysFails{name: "flaky"})
	e.now = func() time.Time { return now }

	for range 30 {
		_, err := e.CollectAll(context.Background())
		require.Error(t, err)
		assert.True(t, IsPartial(err), "every cycle still says the look was incomplete")
		now = now.Add(time.Minute)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, lines, 1, "the first failure is loud")
	assert.LessOrEqual(t, lines, 8, "30 failing minutes is not 30 lines")
}

func TestEngine_DifferentCollectorsLogIndependently(t *testing.T) {
	var mu sync.Mutex
	lines := 0
	logger := funcr.New(func(_, _ string) {
		mu.Lock()
		defer mu.Unlock()
		lines++
	}, funcr.Options{})
	e := NewEngine(logger, alwaysFails{name: "a"}, alwaysFails{name: "b"})
	_, err := e.CollectAll(context.Background())
	require.Error(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, lines, "one collector's noise must not hide another's first failure")
}
