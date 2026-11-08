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

package controller

import (
	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// incidentSubject is the application an incident is filed against.
type incidentSubject struct {
	// personaRef is written to the incident. On an unattributed subject it
	// names the workload rather than a persona that exists, which is why the
	// unattributed flag is recorded next to it instead of inferred from it.
	personaRef dorguv1.PersonaReference

	// unattributed marks a subject no single persona claimed.
	unattributed bool

	// namespace is where the incident lives.
	namespace string
}

// personaSubject builds the subject for an application Dorgu knows by persona.
func personaSubject(ref dorguv1.PersonaReference) incidentSubject {
	return incidentSubject{personaRef: ref, namespace: ref.Namespace}
}

// incidentSubjectFor derives the subject of a signal group, reporting false
// when the group is about no application at all.
func incidentSubjectFor(group *detection.SignalGroup) (incidentSubject, bool) {
	switch group.Scope {
	case detection.ScopePersona:
		if group.PersonaRef == nil {
			return incidentSubject{}, false
		}
		subject := personaSubject(*group.PersonaRef)
		if subject.namespace == "" {
			subject.namespace = group.Namespace
		}
		return subject, true

	case detection.ScopeUnattributed:
		// Recorded against the workload, and honest about it. An incident
		// Dorgu cannot attribute is still an outage the user needs to see;
		// folding it into a neighbouring app to give it an owner is what
		// poisoned every plan in the clean-room run.
		return incidentSubject{
			personaRef: dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      group.Workload,
				Namespace: group.Namespace,
			},
			unattributed: true,
			namespace:    group.Namespace,
		}, true

	default:
		return incidentSubject{}, false
	}
}

// attribution names the subject's attribution for a label and for the spec.
func (s incidentSubject) attribution() string {
	if s.unattributed {
		return AttributionUnattributed
	}
	return AttributionPersona
}

// incidentNameKey is the persona segment of the generated incident name. An
// unattributed incident says so in its own name, so it can never collide with
// the incident raised once a persona does claim the workload.
func (s incidentSubject) incidentNameKey() string {
	if s.unattributed {
		return s.personaRef.Name + "-unattributed"
	}
	return s.personaRef.Name
}
