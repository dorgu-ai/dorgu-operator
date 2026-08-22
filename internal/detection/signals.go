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
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Signal represents a detected health signal from the cluster.
// Signals are the raw observations — diagnosis turns them into root causes.
type Signal struct {
	// Type identifies the signal (e.g., "OOMKilled", "NodeNotReady", "HighMemoryUsage").
	Type SignalType

	// Severity of the signal.
	Severity Severity

	// Category classifies the signal.
	Category Category

	// Source identifies the detector that produced this signal.
	Source string

	// Message is a human-readable description.
	Message string

	// Resource is the affected K8s resource.
	Resource dorguv1.ResourceReference

	// PersonaRef links to the affected Persona (if known).
	PersonaRef *dorguv1.PersonaReference

	// Value is an optional numeric value (e.g., CPU usage percentage).
	Value *float64

	// Threshold is the threshold that was exceeded (if applicable).
	Threshold *float64

	// DetectedAt is when the signal was first observed.
	DetectedAt time.Time

	// Metadata contains additional signal-specific data.
	Metadata map[string]string
}

// SignalType identifies the kind of health signal detected.
type SignalType string

// Node signals.
const (
	SignalNodeNotReady       SignalType = "NodeNotReady"
	SignalNodeMemoryPressure SignalType = "NodeMemoryPressure"
	SignalNodeDiskPressure   SignalType = "NodeDiskPressure"
	SignalNodePIDPressure    SignalType = "NodePIDPressure"
	SignalNodeNetworkDown    SignalType = "NodeNetworkUnavailable"
)

// Pod signals.
const (
	SignalOOMKilled        SignalType = "OOMKilled"
	SignalCrashLoopBackOff SignalType = "CrashLoopBackOff"
	SignalImagePullBackOff SignalType = "ImagePullBackOff"
	SignalPodEvicted       SignalType = "PodEvicted"
	SignalPodPendingLong   SignalType = "PodPendingLong"
	SignalProbeFailure     SignalType = "ProbeFailure"
	SignalContainerRestart SignalType = "ContainerHighRestarts"
)

// Resource signals.
const (
	SignalCPUSaturationHigh     SignalType = "CPUSaturationHigh"
	SignalMemorySaturationHigh  SignalType = "MemorySaturationHigh"
	SignalCPUSaturationCritical SignalType = "CPUSaturationCritical"
	SignalMemorySaturationCrit  SignalType = "MemorySaturationCritical"
	SignalCPUUsageHigh          SignalType = "CPUUsageHigh"
	SignalMemoryUsageHigh       SignalType = "MemoryUsageHigh"
)

// Control plane signals.
const (
	SignalAPIServerUnhealthy    SignalType = "APIServerUnhealthy"
	SignalETCDUnhealthy         SignalType = "ETCDUnhealthy"
	SignalSchedulerUnhealthy    SignalType = "SchedulerUnhealthy"
	SignalControllerMgrUnhealth SignalType = "ControllerManagerUnhealthy"
	SignalComponentUnhealthy    SignalType = "ComponentUnhealthy"
)

// Severity indicates the impact level of a signal.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Category classifies signals by domain.
type Category string

const (
	CategoryResource     Category = "resource"
	CategoryHealth       Category = "health"
	CategoryNode         Category = "node"
	CategoryControlPlane Category = "controlplane"
	CategoryDeployment   Category = "deployment"
	CategoryDependency   Category = "dependency"
	CategoryScaling      Category = "scaling"
	CategorySecurity     Category = "security"
)

// SeverityRank returns a numeric rank for ordering severities (higher = more
// severe). An unrecognised severity ranks below info so it never reads as an
// escalation.
func SeverityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 2
	case SeverityWarning:
		return 1
	case SeverityInfo:
		return 0
	default:
		return -1
	}
}
