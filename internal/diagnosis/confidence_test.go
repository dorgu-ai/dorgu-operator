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

package diagnosis

import (
	"math"
	"testing"
	"time"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

func TestCalculateConfidence(t *testing.T) {
	tests := []struct {
		name    string
		factors ConfidenceFactors
		wantMin float64
		wantMax float64
	}{
		{
			name: "perfect score: high base, clear signal, tight window",
			factors: ConfidenceFactors{
				BaseConfidence:    0.95,
				SignalCount:       3,
				SignalClarity:     1.0,
				TimeWindowSeconds: 30,
			},
			wantMin: 0.95, // 0.95 * 1.1 * 1.0 * 1.0 = 1.045, clamped to 1.0
			wantMax: 1.0,
		},
		{
			name: "single signal, default boosts",
			factors: ConfidenceFactors{
				BaseConfidence:    0.70,
				SignalCount:       1,
				SignalClarity:     1.0,
				TimeWindowSeconds: 0,
			},
			wantMin: 0.70,
			wantMax: 0.70,
		},
		{
			name: "two signals boost",
			factors: ConfidenceFactors{
				BaseConfidence:    0.80,
				SignalCount:       2,
				SignalClarity:     0.9,
				TimeWindowSeconds: 30,
			},
			wantMin: 0.75, // 0.80 * 1.05 * 0.9 * 1.0 = 0.756
			wantMax: 0.76,
		},
		{
			name: "wide time window reduces confidence",
			factors: ConfidenceFactors{
				BaseConfidence:    0.80,
				SignalCount:       1,
				SignalClarity:     1.0,
				TimeWindowSeconds: 600,
			},
			wantMin: 0.68, // 0.80 * 1.0 * 1.0 * 0.85 = 0.68
			wantMax: 0.68,
		},
		{
			name: "medium time window",
			factors: ConfidenceFactors{
				BaseConfidence:    0.80,
				SignalCount:       1,
				SignalClarity:     1.0,
				TimeWindowSeconds: 120,
			},
			wantMin: 0.76, // 0.80 * 1.0 * 1.0 * 0.95 = 0.76
			wantMax: 0.76,
		},
		{
			name: "zero base confidence",
			factors: ConfidenceFactors{
				BaseConfidence:    0.0,
				SignalCount:       5,
				SignalClarity:     1.0,
				TimeWindowSeconds: 0,
			},
			wantMin: 0.0,
			wantMax: 0.0,
		},
		{
			name: "low clarity reduces confidence",
			factors: ConfidenceFactors{
				BaseConfidence:    0.90,
				SignalCount:       1,
				SignalClarity:     0.5,
				TimeWindowSeconds: 0,
			},
			wantMin: 0.45, // 0.90 * 1.0 * 0.5 * 1.0 = 0.45
			wantMax: 0.45,
		},
		{
			name: "clamped to 1.0 maximum",
			factors: ConfidenceFactors{
				BaseConfidence:    1.0,
				SignalCount:       5,
				SignalClarity:     1.0,
				TimeWindowSeconds: 0,
			},
			wantMin: 1.0,
			wantMax: 1.0,
		},
		{
			name: "clamped to 0.0 minimum with negative base",
			factors: ConfidenceFactors{
				BaseConfidence:    -0.5,
				SignalCount:       1,
				SignalClarity:     1.0,
				TimeWindowSeconds: 0,
			},
			wantMin: 0.0,
			wantMax: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateConfidence(tt.factors)
			if got < tt.wantMin-0.001 || got > tt.wantMax+0.001 {
				t.Errorf("CalculateConfidence() = %v, want [%v, %v]", got, tt.wantMin, tt.wantMax)
			}
			if got < 0 || got > 1 {
				t.Errorf("CalculateConfidence() = %v, not clamped to [0, 1]", got)
			}
		})
	}
}

func TestSignalClarity(t *testing.T) {
	tests := []struct {
		signalType detection.SignalType
		want       float64
	}{
		{detection.SignalOOMKilled, 1.0},
		{detection.SignalCrashLoopBackOff, 0.9},
		{detection.SignalNodeNotReady, 0.95},
		{detection.SignalPodPendingLong, 0.6},
		{detection.SignalProbeFailure, 0.7},
		{detection.SignalContainerRestart, 0.6},
		{"UnknownSignalType", 0.5}, // default
	}

	for _, tt := range tests {
		t.Run(string(tt.signalType), func(t *testing.T) {
			got := SignalClarity(tt.signalType)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("SignalClarity(%s) = %v, want %v", tt.signalType, got, tt.want)
			}
		})
	}
}

func TestAverageClarity(t *testing.T) {
	tests := []struct {
		name    string
		signals []detection.Signal
		want    float64
	}{
		{
			name:    "empty signals",
			signals: nil,
			want:    0.5,
		},
		{
			name: "single OOM signal",
			signals: []detection.Signal{
				{Type: detection.SignalOOMKilled},
			},
			want: 1.0,
		},
		{
			name: "mixed clarity signals",
			signals: []detection.Signal{
				{Type: detection.SignalOOMKilled},      // 1.0
				{Type: detection.SignalPodPendingLong}, // 0.6
			},
			want: 0.8, // (1.0 + 0.6) / 2
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AverageClarity(tt.signals)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("AverageClarity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTimeWindowSeconds(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		signals []detection.Signal
		want    float64
	}{
		{
			name:    "empty signals",
			signals: nil,
			want:    0,
		},
		{
			name: "single signal",
			signals: []detection.Signal{
				{DetectedAt: now},
			},
			want: 0,
		},
		{
			name: "two signals 60 seconds apart",
			signals: []detection.Signal{
				{DetectedAt: now},
				{DetectedAt: now.Add(60 * time.Second)},
			},
			want: 60,
		},
		{
			name: "three signals spanning 5 minutes",
			signals: []detection.Signal{
				{DetectedAt: now},
				{DetectedAt: now.Add(2 * time.Minute)},
				{DetectedAt: now.Add(5 * time.Minute)},
			},
			want: 300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TimeWindowSeconds(tt.signals)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("TimeWindowSeconds() = %v, want %v", got, tt.want)
			}
		})
	}
}
