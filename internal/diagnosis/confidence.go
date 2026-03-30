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

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// ConfidenceFactors affect the final confidence score.
type ConfidenceFactors struct {
	// BaseConfidence from the rule (0.0-1.0).
	BaseConfidence float64

	// SignalCount — more correlated signals increase confidence.
	SignalCount int

	// SignalClarity — are signals unambiguous? (e.g., OOMKilled is clearer than PodPending).
	SignalClarity float64

	// TimeWindowSeconds — time span of contributing signals (tighter = higher confidence).
	TimeWindowSeconds float64
}

// CalculateConfidence computes the final confidence score, clamped to [0.0, 1.0].
func CalculateConfidence(factors ConfidenceFactors) float64 {
	confidence := factors.BaseConfidence *
		signalCountBoost(factors.SignalCount) *
		factors.SignalClarity *
		timeProximityBoost(factors.TimeWindowSeconds)

	return math.Min(1.0, math.Max(0.0, confidence))
}

// signalCountBoost returns a multiplier based on the number of correlated signals.
// 1 signal: 1.0, 2 signals: 1.05, 3+ signals: 1.1
func signalCountBoost(count int) float64 {
	switch {
	case count >= 3:
		return 1.1
	case count == 2:
		return 1.05
	default:
		return 1.0
	}
}

// timeProximityBoost returns a multiplier based on how close signals are in time.
// <60s: 1.0, <300s: 0.95, >=300s: 0.85
func timeProximityBoost(windowSeconds float64) float64 {
	switch {
	case windowSeconds < 60:
		return 1.0
	case windowSeconds < 300:
		return 0.95
	default:
		return 0.85
	}
}

// signalClarity maps signal types to clarity values.
// Higher clarity = less ambiguous signal.
var signalClarity = map[detection.SignalType]float64{
	detection.SignalOOMKilled:           1.0,
	detection.SignalCrashLoopBackOff:    0.9,
	detection.SignalNodeNotReady:        0.95,
	detection.SignalImagePullBackOff:    0.9,
	detection.SignalPodEvicted:          0.95,
	detection.SignalCPUSaturationCritical: 0.85,
	detection.SignalMemorySaturationCrit: 0.85,
	detection.SignalPodPendingLong:      0.6,
	detection.SignalProbeFailure:        0.7,
	detection.SignalContainerRestart:    0.6,
	detection.SignalNodeMemoryPressure:  0.9,
	detection.SignalNodeDiskPressure:    0.9,
	detection.SignalNodePIDPressure:     0.9,
	detection.SignalNodeNetworkDown:     0.95,
	detection.SignalAPIServerUnhealthy:  0.95,
	detection.SignalETCDUnhealthy:       0.95,
	detection.SignalSchedulerUnhealthy:  0.95,
	detection.SignalControllerMgrUnhealth: 0.95,
	detection.SignalComponentUnhealthy:  0.9,
	detection.SignalCPUSaturationHigh:   0.75,
	detection.SignalMemorySaturationHigh: 0.75,
	detection.SignalCPUUsageHigh:        0.7,
	detection.SignalMemoryUsageHigh:     0.7,
}

// SignalClarity returns the clarity value for a signal type.
// Returns 0.5 as default for unknown signal types.
func SignalClarity(signalType detection.SignalType) float64 {
	if clarity, ok := signalClarity[signalType]; ok {
		return clarity
	}
	return 0.5
}

// AverageClarity computes the average clarity of a set of signals.
func AverageClarity(signals []detection.Signal) float64 {
	if len(signals) == 0 {
		return 0.5
	}
	var total float64
	for _, s := range signals {
		total += SignalClarity(s.Type)
	}
	return total / float64(len(signals))
}

// TimeWindowSeconds returns the time span in seconds between the earliest and latest signals.
func TimeWindowSeconds(signals []detection.Signal) float64 {
	if len(signals) <= 1 {
		return 0
	}
	earliest := signals[0].DetectedAt
	latest := signals[0].DetectedAt
	for _, s := range signals[1:] {
		if s.DetectedAt.Before(earliest) {
			earliest = s.DetectedAt
		}
		if s.DetectedAt.After(latest) {
			latest = s.DetectedAt
		}
	}
	return latest.Sub(earliest).Seconds()
}
