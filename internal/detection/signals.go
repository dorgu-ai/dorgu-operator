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

// Resource kinds the detectors report that have no namespace. They are the ONE
// list of cluster-scoped kinds: the detectors name their resources with these
// constants, and everything that must tell "cluster-scoped" from "namespace not
// determined" asks IsClusterScopedKind rather than keeping a copy.
const (
	KindNode            = "Node"
	KindComponentStatus = "ComponentStatus"
)

// IsClusterScopedKind reports whether kind is one the detectors report without a
// namespace because it has none. Any other kind with an empty namespace is one
// whose namespace could not be determined, and fails closed wherever that matters.
func IsClusterScopedKind(kind string) bool {
	return kind == KindNode || kind == KindComponentStatus
}

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

	// SignalReadinessNotReady is a running container that has stayed not Ready past
	// the readiness threshold: the readiness probe keeps failing, or the app never
	// starts serving. It does not need a restart, which is why the restart-based
	// signals above never saw it.
	SignalReadinessNotReady SignalType = "ReadinessNotReady"

	// SignalLivenessRestarts is a container the kubelet keeps restarting (liveness
	// kills) before it ever reaches CrashLoopBackOff: restarts within a window,
	// last terminated as Error or SIGKILL (137).
	SignalLivenessRestarts SignalType = "LivenessRestarts"

	// SignalRestartLoop is a container restarted repeatedly within the window whose
	// last exit was an Error or a SIGKILL, with no kubelet event saying a liveness
	// probe killed it. It is a restart loop of unknown cause, not a liveness
	// failure: an application that crashes quickly looks exactly like this.
	SignalRestartLoop SignalType = "RestartLoop"
)

// Resource signals.
const (
	SignalCPUSaturationHigh     SignalType = "CPUSaturationHigh"
	SignalMemorySaturationHigh  SignalType = "MemorySaturationHigh"
	SignalCPUSaturationCritical SignalType = "CPUSaturationCritical"
	SignalMemorySaturationCrit  SignalType = "MemorySaturationCritical"
	SignalCPUUsageHigh          SignalType = "CPUUsageHigh"
	SignalMemoryUsageHigh       SignalType = "MemoryUsageHigh"

	// SignalCPUThrottling is a container whose CPU is being held back by its own
	// limit: usage at or above 95% of the limit for three consecutive cycles
	// (metrics.k8s.io), or, when a Prometheus-compatible endpoint is configured,
	// the cfs throttled-period ratio at or above 25%. It names a container, not a
	// node, and the fix is the container's CPU limit.
	SignalCPUThrottling SignalType = "CPUThrottling"
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

// faultRank orders the signals of the progression of one container's fault, by how
// far along it is. A readiness failure is the mildest; the kubelet restarting the
// container for an unknown reason, then for a liveness probe, is worse; then
// CrashLoopBackOff; and an OOM kill is the worst, the one with a cause to fix.
// Liveness, with the kubelet's own event behind it, outranks a restart loop of
// unknown cause.
var faultRank = map[SignalType]int{
	SignalReadinessNotReady: 1,
	SignalRestartLoop:       2,
	SignalLivenessRestarts:  3,
	SignalCrashLoopBackOff:  4,
	SignalOOMKilled:         5,
}

// FaultRank returns a signal's rank in the progression of one container's fault,
// and whether it is part of it at all. Everything that treats the progression as
// one fault (merging incidents, the recurrence cap, the rejection cooldown) asks
// here, so there is one ordering.
func FaultRank(t SignalType) (int, bool) {
	rank, ok := faultRank[t]
	return rank, ok
}

// Family names a set of signals that are one fault in different shapes. A fault's
// identity is the application, the CONTAINER and the family, so everything that
// keys on identity (the incident a diagnosis merges into, the recurrence budget, the
// rejection cooldown) asks FamilyOf rather than listing signals.
type Family string

const (
	// FamilyFault is the progression of one container dying: not Ready, restarted
	// by the kubelet, crash-looping, OOM-killed. It is ranked (see FaultRank).
	FamilyFault Family = "fault"

	// FamilyCPUThrottling is a container held back by its own CPU limit. It has one
	// rung: there is nothing milder or worse in it, so it never escalates, and it
	// never merges with the fault progression, which is a different fix.
	FamilyCPUThrottling Family = "cpu-throttling"
)

// FamilyOf returns the family a signal belongs to, and whether it belongs to one.
// A signal in no family keeps the identity it always had: persona, category and
// signal.
func FamilyOf(t SignalType) (Family, bool) {
	if _, ok := faultRank[t]; ok {
		return FamilyFault, true
	}
	if t == SignalCPUThrottling {
		return FamilyCPUThrottling, true
	}
	return "", false
}
