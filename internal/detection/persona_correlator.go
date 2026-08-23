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

// Correlate sets PersonaRef on signals that can be attributed to exactly one
// ApplicationPersona in the signal's own namespace.
//
// Two rules hold, and they are the reason a diagnosis can be trusted to be
// about one application:
//
//   - Namespace scoping. Only personas in the signal's namespace are
//     considered, so an app can never claim another namespace's pod.
//   - Exactly one owner. A resource that several personas claim is left
//     unattributed rather than handed to whichever persona the API server
//     happened to list first. An unattributed incident is honest; an incident
//     filed against the wrong app is not (F-02).
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

		matched := personasClaiming(sig, personas)
		switch len(matched) {
		case 0:
			// Left unattributed on purpose. The signal still becomes an
			// incident of its own; it just does not become somebody else's.
		case 1:
			sig.PersonaRef = &dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      matched[0].Name,
				Namespace: matched[0].Namespace,
			}
		default:
			pc.logger.Info("signal left unattributed: more than one persona claims the resource",
				"namespace", ns,
				"resource", sig.Resource.Name,
				"personas", personaNames(matched),
			)
		}
	}
}

// personasClaiming returns the personas that claim a signal's resource, keeping
// only the most specific claim.
//
// Personas "api" and "api-server" both match pod "api-server-7f9d-x2q" under
// the documented prefix rule, but only one of them is the pod's application:
// the longer name is the specific claim and the shorter one is a coincidence of
// prefixes. A tie at the same specificity is a genuine ambiguity and returns
// every tied persona, so the caller can decline to guess.
func personasClaiming(sig *Signal, personas []dorguv1.ApplicationPersona) []*dorguv1.ApplicationPersona {
	var matched []*dorguv1.ApplicationPersona
	best := 0

	for i := range personas {
		length := claimLength(sig.Resource.Name, &personas[i])
		switch {
		case length == 0 || length < best:
			continue
		case length > best:
			best = length
			matched = []*dorguv1.ApplicationPersona{&personas[i]}
		default:
			matched = append(matched, &personas[i])
		}
	}

	return matched
}

// claimLength returns the length of the persona name that claims resourceName,
// or 0 when the persona does not claim it. A resource is claimed when its name
// equals the persona name or starts with the persona name followed by a hyphen,
// checked against both metadata.name and spec.name.
func claimLength(resourceName string, persona *dorguv1.ApplicationPersona) int {
	if resourceName == "" {
		return 0
	}

	best := 0
	for _, name := range []string{persona.Name, persona.Spec.Name} {
		if name == "" || len(name) <= best {
			continue
		}
		if resourceName == name || strings.HasPrefix(resourceName, name+"-") {
			best = len(name)
		}
	}
	return best
}

// matchesPersona reports whether a persona claims a signal's resource at all,
// ignoring how specifically. Attribution uses personasClaiming; this is for
// callers asking the simpler "is this resource this app's?" question.
func matchesPersona(sig *Signal, persona *dorguv1.ApplicationPersona) bool {
	return claimLength(sig.Resource.Name, persona) > 0
}

// NameClaimedByPersona reports whether a resource name belongs to the named
// persona under the same prefix rule attribution uses. Callers outside
// detection use it to ask which pods an incident's workload owns.
func NameClaimedByPersona(resourceName, personaName string) bool {
	if resourceName == "" || personaName == "" {
		return false
	}
	return resourceName == personaName || strings.HasPrefix(resourceName, personaName+"-")
}

// personaNames lists persona names for a log line.
func personaNames(personas []*dorguv1.ApplicationPersona) []string {
	names := make([]string, 0, len(personas))
	for _, p := range personas {
		names = append(names, p.Name)
	}
	return names
}
