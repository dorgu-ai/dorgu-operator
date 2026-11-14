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
	"testing"

	"github.com/stretchr/testify/assert"
)

// A fault is an application, a CONTAINER and a family. CPUThrottling is a family of
// its own with one rung, so it takes the same identity, recurrence and cooldown paths
// as the progression of restarts and kills, and has nothing to escalate to or from.
func TestFamilyOf(t *testing.T) {
	tests := []struct {
		signal SignalType
		family Family
		ok     bool
	}{
		{SignalReadinessNotReady, FamilyFault, true},
		{SignalRestartLoop, FamilyFault, true},
		{SignalLivenessRestarts, FamilyFault, true},
		{SignalCrashLoopBackOff, FamilyFault, true},
		{SignalOOMKilled, FamilyFault, true},
		{SignalCPUThrottling, FamilyCPUThrottling, true},
		{SignalImagePullBackOff, "", false},
		{SignalNodeNotReady, "", false},
		{SignalCPUUsageHigh, "", false},
		{SignalType("Unknown"), "", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.signal), func(t *testing.T) {
			family, ok := FamilyOf(tt.signal)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.family, family)
		})
	}
}

func TestCPUThrottlingIsNotOnTheFaultLadder(t *testing.T) {
	_, ok := FaultRank(SignalCPUThrottling)
	assert.False(t, ok, "throttling is a different fault from a container that is dying: it never merges into, "+
		"escalates or de-escalates through the restart/OOM ladder")
}
