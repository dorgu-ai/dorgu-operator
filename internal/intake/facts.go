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

package intake

import (
	v1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// factsFromIncident derives, from the incident alone, the gate inputs a diagnosis
// gives the planner in process: the signal, the severity, the class of change the
// rules would recommend, and the persona path that change would touch.
//
// It reads recorded detection data, never a proposal's text. The class mapping
// mirrors the rule engine's: a resource-saturation signal, an OOM kill, or a crash
// loop correlated with an OOM, is a resource adjustment; everything else is not,
// so a persona-update proposed for it is refused at Validate/action-class.
func factsFromIncident(incident *v1.IncidentMemory) gate.Facts {
	if incident == nil {
		return gate.Facts{}
	}
	signal := incident.Spec.Detection.Signal
	facts := gate.Facts{
		Incident:      incident,
		Severity:      incident.Spec.Severity,
		PrimarySignal: signal,
	}

	switch detection.SignalType(signal) {
	case detection.SignalOOMKilled, detection.SignalMemorySaturationCrit, detection.SignalMemorySaturationHigh:
		facts.SuggestedAction = gate.ClassResourceAdjustment
		facts.TargetPath = gate.ResourcePathMemory
	case detection.SignalCPUSaturationHigh, detection.SignalCPUSaturationCritical:
		facts.SuggestedAction = gate.ClassResourceAdjustment
		facts.TargetPath = gate.ResourcePathCPU
	case detection.SignalCrashLoopBackOff:
		if hasOOMContribution(incident) {
			facts.SuggestedAction = gate.ClassResourceAdjustment
			facts.TargetPath = gate.ResourcePathMemory
		}
	}
	return facts
}

// hasOOMContribution reports whether the incident's diagnosis was correlated with
// an out-of-memory signal.
func hasOOMContribution(incident *v1.IncidentMemory) bool {
	if incident.Spec.RootCause == nil {
		return false
	}
	for _, c := range incident.Spec.RootCause.Contributing {
		switch detection.SignalType(c.Signal) {
		case detection.SignalOOMKilled, detection.SignalMemorySaturationCrit, detection.SignalMemorySaturationHigh:
			return true
		}
	}
	return false
}
