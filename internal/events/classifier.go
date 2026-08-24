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
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
)

// Classifier classifies raw K8s Events into InternalEvents with severity and category.
type Classifier interface {
	// Classify converts a K8s Event into an InternalEvent.
	// Returns nil for events that should be discarded (Normal events with no significance).
	Classify(event *corev1.Event) *InternalEvent
}

// classificationRule maps a K8s event reason to severity and category.
type classificationRule struct {
	category Category
	sev      Severity
	match    func(event *corev1.Event) bool
}

// RuleBasedClassifier classifies events using a static rule table.
type RuleBasedClassifier struct {
	rules []classificationRule
}

// NewClassifier creates a new RuleBasedClassifier with the default classification rules.
func NewClassifier() *RuleBasedClassifier {
	c := &RuleBasedClassifier{}
	c.rules = c.defaultRules()
	return c
}

// Classify converts a K8s Event into an InternalEvent.
// Returns nil for events that should be discarded.
func (c *RuleBasedClassifier) Classify(event *corev1.Event) *InternalEvent {
	if event == nil {
		return nil
	}

	// Never re-ingest an event dorgu recorded itself. The emitter writes a
	// Kubernetes Event for every record it stores, so the watcher saw that Event
	// and stored a second DorguEvent saying dorgu had said something. DorguDetected
	// and DorguDiagnosisDiscarded together accounted for 24 of F-06's duplicate-key
	// errors, and every one of those records was an echo of a record dorgu already
	// held. Nothing is lost by dropping them: the Kubernetes Event alerting selects
	// on is still emitted, and the reconciler writes its own DorguEvent directly.
	if isSelfRecorded(event) {
		return nil
	}

	for _, rule := range c.rules {
		if rule.match(event) {
			return c.toInternalEvent(event, rule.sev, rule.category)
		}
	}

	// Unmatched Warning events get classified as info/health.
	if event.Type == corev1.EventTypeWarning {
		return c.toInternalEvent(event, SeverityInfo, CategoryHealth)
	}

	// Normal events with no matching rule are discarded.
	return nil
}

func (c *RuleBasedClassifier) toInternalEvent(event *corev1.Event, severity Severity, category Category) *InternalEvent {
	eventTime := event.LastTimestamp.Time
	if eventTime.IsZero() {
		eventTime = event.CreationTimestamp.Time
	}
	if eventTime.IsZero() {
		eventTime = time.Now()
	}

	return &InternalEvent{
		ID:       generateEventID(event),
		Severity: severity,
		Category: category,
		Source:   event.Source.Component,
		Reason:   event.Reason,
		Message:  event.Message,
		InvolvedObject: dorguv1.ResourceReference{
			Kind:      event.InvolvedObject.Kind,
			Name:      event.InvolvedObject.Name,
			Namespace: event.InvolvedObject.Namespace,
		},
		EventTime:   eventTime,
		K8sEventUID: string(event.UID),
		Raw:         event,
	}
}

func generateEventID(event *corev1.Event) string {
	data := fmt.Sprintf("%s/%s/%s/%s/%s",
		event.InvolvedObject.Namespace,
		event.InvolvedObject.Kind,
		event.InvolvedObject.Name,
		event.Reason,
		event.LastTimestamp.Format(time.RFC3339),
	)
	hash := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", hash[:8])
}

func (c *RuleBasedClassifier) defaultRules() []classificationRule {
	return []classificationRule{
		{
			sev:      SeverityCritical,
			category: CategoryResource,
			match:    reasonEquals("OOMKilling"),
		},
		{
			sev:      SeverityCritical,
			category: CategoryHealth,
			match:    reasonEquals("BackOff"),
		},
		{
			sev:      SeverityWarning,
			category: CategoryDeployment,
			match: func(event *corev1.Event) bool {
				return event.Reason == "Failed" && strings.Contains(event.Message, "ImagePullBackOff")
			},
		},
		{
			sev:      SeverityWarning,
			category: CategoryDeployment,
			match: func(event *corev1.Event) bool {
				return event.Reason == "Failed" && strings.Contains(event.Message, "ErrImagePull")
			},
		},
		{
			sev:      SeverityCritical,
			category: CategoryResource,
			match:    reasonEquals("Evicted"),
		},
		{
			sev:      SeverityWarning,
			category: CategoryResource,
			match:    reasonEquals("FailedScheduling"),
		},
		{
			sev:      SeverityCritical,
			category: CategoryNode,
			match:    reasonEquals("NodeNotReady"),
		},
		{
			sev:      SeverityWarning,
			category: CategoryNode,
			match:    reasonEquals("MemoryPressure"),
		},
		{
			sev:      SeverityWarning,
			category: CategoryNode,
			match:    reasonEquals("DiskPressure"),
		},
		{
			sev:      SeverityWarning,
			category: CategoryNode,
			match:    reasonEquals("PIDPressure"),
		},
		{
			sev:      SeverityCritical,
			category: CategoryNode,
			match:    reasonEquals("NetworkUnavailable"),
		},
		{
			sev:      SeverityWarning,
			category: CategoryDependency,
			match:    reasonEquals("FailedMount"),
		},
		{
			sev:      SeverityWarning,
			category: CategoryHealth,
			match:    reasonEquals("Unhealthy"),
		},
		{
			sev:      SeverityInfo,
			category: CategoryScaling,
			match:    reasonEquals("ScalingReplicaSet"),
		},
		{
			sev:      SeverityInfo,
			category: CategoryDeployment,
			match:    reasonEquals("SuccessfulCreate"),
		},
	}
}

// isSelfRecorded reports whether the operator recorded this Kubernetes Event.
// record.EventRecorder fills Source.Component; the newer events API fills
// ReportingController instead, so both are checked and either one is enough.
func isSelfRecorded(event *corev1.Event) bool {
	return event.Source.Component == OperatorEventSource ||
		event.ReportingController == OperatorEventSource
}

// reasonEquals returns a match function that checks the event reason.
func reasonEquals(reason string) func(*corev1.Event) bool {
	return func(event *corev1.Event) bool {
		return event.Reason == reason
	}
}
