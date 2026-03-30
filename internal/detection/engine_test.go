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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// stubCollector is a test double for SignalCollector.
type stubCollector struct {
	name    string
	signals []Signal
	err     error
}

func (s *stubCollector) Name() string                                { return s.name }
func (s *stubCollector) Collect(_ context.Context) ([]Signal, error) { return s.signals, s.err }

func TestEngine_CollectAll_Empty(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	engine := NewEngine(logger)

	signals, err := engine.CollectAll(context.Background())
	require.NoError(t, err)
	assert.Empty(t, signals)
}

func TestEngine_CollectAll_SingleCollector(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	now := time.Now()

	collector := &stubCollector{
		name: "test",
		signals: []Signal{
			{Type: SignalNodeNotReady, Severity: SeverityCritical, DetectedAt: now},
		},
	}

	engine := NewEngine(logger, collector)
	signals, err := engine.CollectAll(context.Background())

	require.NoError(t, err)
	assert.Len(t, signals, 1)
	assert.Equal(t, SignalNodeNotReady, signals[0].Type)
}

func TestEngine_CollectAll_MultipleCollectors(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	now := time.Now()

	c1 := &stubCollector{
		name: "nodes",
		signals: []Signal{
			{Type: SignalNodeNotReady, Severity: SeverityCritical, DetectedAt: now},
		},
	}
	c2 := &stubCollector{
		name: "pods",
		signals: []Signal{
			{Type: SignalOOMKilled, Severity: SeverityCritical, DetectedAt: now.Add(-time.Minute)},
			{Type: SignalImagePullBackOff, Severity: SeverityWarning, DetectedAt: now},
		},
	}

	engine := NewEngine(logger, c1, c2)
	signals, err := engine.CollectAll(context.Background())

	require.NoError(t, err)
	assert.Len(t, signals, 3)
}

func TestEngine_CollectAll_PartialFailure(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	now := time.Now()

	good := &stubCollector{
		name: "good",
		signals: []Signal{
			{Type: SignalNodeNotReady, Severity: SeverityCritical, DetectedAt: now},
		},
	}
	bad := &stubCollector{
		name: "bad",
		err:  errors.New("connection refused"),
	}

	engine := NewEngine(logger, bad, good)
	signals, err := engine.CollectAll(context.Background())

	require.NoError(t, err)
	assert.Len(t, signals, 1, "should still return signals from working collectors")
}

func TestEngine_CollectAll_SortsBySeverityThenTime(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	now := time.Now()

	collector := &stubCollector{
		name: "mixed",
		signals: []Signal{
			{Type: SignalImagePullBackOff, Severity: SeverityWarning, DetectedAt: now.Add(-5 * time.Minute)},
			{Type: SignalNodeNotReady, Severity: SeverityCritical, DetectedAt: now.Add(-10 * time.Minute)},
			{Type: SignalOOMKilled, Severity: SeverityCritical, DetectedAt: now},
			{Type: SignalNodeDiskPressure, Severity: SeverityWarning, DetectedAt: now},
		},
	}

	engine := NewEngine(logger, collector)
	signals, err := engine.CollectAll(context.Background())

	require.NoError(t, err)
	require.Len(t, signals, 4)

	// Critical first, sorted by time (newest first)
	assert.Equal(t, SeverityCritical, signals[0].Severity)
	assert.Equal(t, SignalOOMKilled, signals[0].Type)
	assert.Equal(t, SeverityCritical, signals[1].Severity)
	assert.Equal(t, SignalNodeNotReady, signals[1].Type)

	// Then warnings, sorted by time (newest first)
	assert.Equal(t, SeverityWarning, signals[2].Severity)
	assert.Equal(t, SignalNodeDiskPressure, signals[2].Type)
	assert.Equal(t, SeverityWarning, signals[3].Severity)
	assert.Equal(t, SignalImagePullBackOff, signals[3].Type)
}

func TestEngine_CollectAll_AllCollectorsFail(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))

	bad1 := &stubCollector{name: "bad1", err: errors.New("fail1")}
	bad2 := &stubCollector{name: "bad2", err: errors.New("fail2")}

	engine := NewEngine(logger, bad1, bad2)
	signals, err := engine.CollectAll(context.Background())

	require.NoError(t, err)
	assert.Empty(t, signals)
}
