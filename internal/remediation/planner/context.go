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

package planner

import (
	"context"
	"fmt"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

const (
	// labelPersonaName is the label the incident controller stamps on every
	// IncidentMemory and RemediationAction, linking it to its persona. Mirrors
	// controller.LabelPersonaName (duplicated here to avoid importing the
	// controller package and creating an import cycle).
	labelPersonaName = "dorgu.io/persona-name"

	// MaxPastIncidents caps how many recent incidents are pulled into context,
	// keeping the rendered prompt within a sane token budget.
	MaxPastIncidents = 10

	// kindApplicationPersona is the only persona kind BuildContext loads an app
	// persona for.
	kindApplicationPersona = "ApplicationPersona"
)

// BuildContext assembles a RemediationContext for the diagnosed incident by
// loading the affected ApplicationPersona, the singleton ClusterPersona, the
// recent IncidentMemory records for the same application, and the
// RemediationActions those incidents reference (carried with their status so
// the planner can see which past fixes succeeded or failed).
//
// It is best-effort: a missing ClusterPersona, an unreadable past remediation,
// or an empty incident history are not fatal — the corresponding context fields
// are simply left empty. Only a failure to load the (required) application
// persona is returned as an error, since the planner cannot patch a persona it
// cannot read.
func BuildContext(
	ctx context.Context,
	c client.Client,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
) (*RemediationContext, error) {
	if incident == nil {
		return nil, fmt.Errorf("incident is required to build remediation context")
	}

	personaRef := personaReference(diag, incident)
	if personaRef == nil {
		return nil, fmt.Errorf("no persona reference available to build remediation context")
	}

	rc := &RemediationContext{
		Diagnosis: diag,
		Signals:   signalsFromDiagnosis(diag),
	}

	appPersona, err := loadAppPersona(ctx, c, personaRef)
	if err != nil {
		return nil, fmt.Errorf("loading application persona: %w", err)
	}
	rc.AppPersona = appPersona

	rc.ClusterPersona = loadClusterPersona(ctx, c)

	pastIncidents := loadPastIncidents(ctx, c, personaRef)
	rc.PastIncidents = pastIncidents
	rc.PastRemediations = loadPastRemediations(ctx, c, personaRef, pastIncidents)

	return rc, nil
}

// personaReference prefers the diagnosis's persona ref, falling back to the
// incident's.
func personaReference(diag diagnosis.Diagnosis, incident *dorguv1.IncidentMemory) *dorguv1.PersonaReference {
	if diag.PersonaRef != nil {
		return diag.PersonaRef
	}
	ref := incident.Spec.PersonaRef
	if ref.Name == "" {
		return nil
	}
	return &ref
}

// signalsFromDiagnosis flattens the diagnosis's contributing signals.
func signalsFromDiagnosis(diag diagnosis.Diagnosis) []detection.Signal {
	signals := make([]detection.Signal, 0, len(diag.Contributing))
	for _, cs := range diag.Contributing {
		signals = append(signals, cs.Signal)
	}
	return signals
}

// loadAppPersona fetches the ApplicationPersona referenced by the diagnosis.
func loadAppPersona(ctx context.Context, c client.Client, ref *dorguv1.PersonaReference) (*dorguv1.ApplicationPersona, error) {
	if ref.Kind != "" && ref.Kind != kindApplicationPersona {
		return nil, fmt.Errorf("unsupported persona kind %q for remediation planning", ref.Kind)
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = "default"
	}

	var persona dorguv1.ApplicationPersona
	key := client.ObjectKey{Name: ref.Name, Namespace: namespace}
	if err := c.Get(ctx, key, &persona); err != nil {
		return nil, fmt.Errorf("getting ApplicationPersona %s/%s: %w", namespace, ref.Name, err)
	}
	return &persona, nil
}

// loadClusterPersona returns the singleton ClusterPersona, or nil if none exists
// or listing fails (best-effort — the planner can still produce a plan without
// cluster policy, just with less context).
func loadClusterPersona(ctx context.Context, c client.Client) *dorguv1.ClusterPersona {
	var list dorguv1.ClusterPersonaList
	if err := c.List(ctx, &list); err != nil || len(list.Items) == 0 {
		return nil
	}
	// Return a copy to avoid aliasing the list's backing array.
	cp := list.Items[0]
	return &cp
}

// loadPastIncidents lists recent IncidentMemory records for the application,
// sorted most-recent first and capped at MaxPastIncidents.
func loadPastIncidents(ctx context.Context, c client.Client, ref *dorguv1.PersonaReference) []dorguv1.IncidentMemory {
	var list dorguv1.IncidentMemoryList
	opts := []client.ListOption{
		client.MatchingLabels{labelPersonaName: ref.Name},
	}
	if ref.Namespace != "" {
		opts = append(opts, client.InNamespace(ref.Namespace))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return nil
	}

	incidents := list.Items
	sort.SliceStable(incidents, func(i, j int) bool {
		return incidentRecency(incidents[i]).After(incidentRecency(incidents[j]))
	})

	if len(incidents) > MaxPastIncidents {
		incidents = incidents[:MaxPastIncidents]
	}
	return incidents
}

// incidentRecency returns the timestamp used to sort incidents — the last
// occurrence when known, otherwise the creation time.
func incidentRecency(im dorguv1.IncidentMemory) time.Time {
	if im.Status.LastOccurrence != nil {
		return im.Status.LastOccurrence.Time
	}
	return im.CreationTimestamp.Time
}

// loadPastRemediations gathers the RemediationActions referenced by the given
// incidents, carrying their status (phase + verification result). It resolves
// references two ways and de-duplicates by namespace/name:
//
//   - direct references via IncidentMemory.Spec.Resolution.RemediationRef, and
//   - a label-match fallback listing RemediationActions for the same persona
//     (covers actions created before resolution was recorded).
func loadPastRemediations(
	ctx context.Context,
	c client.Client,
	ref *dorguv1.PersonaReference,
	incidents []dorguv1.IncidentMemory,
) []dorguv1.RemediationAction {
	seen := make(map[string]struct{})
	var out []dorguv1.RemediationAction

	add := func(ra dorguv1.RemediationAction) {
		key := ra.Namespace + "/" + ra.Name
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, ra)
	}

	// 1. Direct references from each incident's resolution.
	for i := range incidents {
		res := incidents[i].Spec.Resolution
		if res == nil || res.RemediationRef == nil || res.RemediationRef.Name == "" {
			continue
		}
		namespace := res.RemediationRef.Namespace
		if namespace == "" {
			namespace = incidents[i].Namespace
		}
		var ra dorguv1.RemediationAction
		key := client.ObjectKey{Name: res.RemediationRef.Name, Namespace: namespace}
		if err := c.Get(ctx, key, &ra); err != nil {
			continue // best-effort
		}
		add(ra)
	}

	// 2. Label-match fallback for the same persona.
	var list dorguv1.RemediationActionList
	opts := []client.ListOption{
		client.MatchingLabels{labelPersonaName: ref.Name},
	}
	if ref.Namespace != "" {
		opts = append(opts, client.InNamespace(ref.Namespace))
	}
	if err := c.List(ctx, &list, opts...); err == nil {
		for i := range list.Items {
			add(list.Items[i])
		}
	}

	return out
}
