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
	"context"
	"strings"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// PersonaCorrelator matches detection signals to ApplicationPersonas.
type PersonaCorrelator interface {
	Correlate(ctx context.Context, signals []Signal)
}

// personaCorrelator matches detection signals to ApplicationPersonas
// by matching the signal's resource namespace and name patterns.
type personaCorrelator struct {
	client client.Reader
	logger logr.Logger
}

// NewPersonaCorrelator creates a correlator that links signals to personas.
func NewPersonaCorrelator(c client.Reader, logger logr.Logger) PersonaCorrelator {
	return &personaCorrelator{client: c, logger: logger}
}

// Correlate sets PersonaRef on signals that can be matched to an ApplicationPersona.
// Matching strategy:
//  1. List all ApplicationPersonas in the signal's namespace
//  2. Match by persona name == resource name prefix (handles pod suffixes)
//  3. If matched, set signal.PersonaRef
func (pc *personaCorrelator) Correlate(ctx context.Context, signals []Signal) {
	cache := make(map[string][]dorguv1.ApplicationPersona)

	for i := range signals {
		sig := &signals[i]
		if sig.PersonaRef != nil {
			continue
		}

		ns := sig.Resource.Namespace
		if ns == "" {
			continue
		}

		personas, ok := cache[ns]
		if !ok {
			var list dorguv1.ApplicationPersonaList
			if err := pc.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
				pc.logger.V(1).Info("failed to list personas for correlation",
					"namespace", ns, "error", err)
				cache[ns] = nil
				continue
			}
			personas = list.Items
			cache[ns] = personas
		}

		for _, p := range personas {
			if matchesPersona(sig, &p) {
				sig.PersonaRef = &dorguv1.PersonaReference{
					Kind:      "ApplicationPersona",
					Name:      p.Name,
					Namespace: p.Namespace,
				}
				break
			}
		}
	}
}

// matchesPersona checks if a signal's resource matches an ApplicationPersona.
// Match criteria:
//   - Resource name equals persona name, or starts with persona name + "-"
//   - Also checks spec.Name if different from metadata.Name
func matchesPersona(sig *Signal, persona *dorguv1.ApplicationPersona) bool {
	resourceName := sig.Resource.Name
	if resourceName == "" {
		return false
	}
	personaName := persona.Name

	if resourceName == personaName || strings.HasPrefix(resourceName, personaName+"-") {
		return true
	}

	if persona.Spec.Name != "" && persona.Spec.Name != personaName {
		if resourceName == persona.Spec.Name || strings.HasPrefix(resourceName, persona.Spec.Name+"-") {
			return true
		}
	}

	return false
}
