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

package events

import (
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// InternalEvent is the pipeline's working representation of an event.
// Richer than DorguEvent CRD — includes processing metadata.
type InternalEvent struct {
	// ID is a unique identifier for deduplication.
	ID string

	// Severity of the event.
	Severity Severity

	// Category classifies the event.
	Category Category

	// Source identifies the detector.
	Source string

	// Reason is the K8s Event reason. Empty means the default detection reason,
	// so a detection event needs to say nothing; an operator-internal failure
	// sets its own so alerting can tell "dorgu found a problem" apart from
	// "dorgu hit a problem".
	Reason string

	// Message is human-readable.
	Message string

	// InvolvedObject is the K8s resource.
	InvolvedObject dorguv1.ResourceReference

	// PersonaRef links to a Persona (may be nil if not yet correlated).
	PersonaRef *dorguv1.PersonaReference

	// EventTime is when the original event occurred.
	EventTime time.Time

	// K8sEventUID is the original K8s Event UID.
	K8sEventUID string

	// Raw is the original K8s Event (for enrichment).
	Raw any
}

// Severity represents event severity levels.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Category classifies events by domain.
type Category string

const (
	CategoryResource     Category = "resource"
	CategoryScaling      Category = "scaling"
	CategoryHealth       Category = "health"
	CategorySecurity     Category = "security"
	CategoryDeployment   Category = "deployment"
	CategoryDependency   Category = "dependency"
	CategoryNode         Category = "node"
	CategoryControlPlane Category = "controlplane"
)

// EventFilter is used for querying the event store.
type EventFilter struct {
	Namespace  string
	Severity   Severity
	Category   Category
	PersonaRef *dorguv1.PersonaReference
	Since      *time.Time
	Limit      int
}
