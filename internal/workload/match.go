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

// Package workload resolves an ApplicationPersona to the Deployment it
// describes.
//
// Matching used to require app.kubernetes.io/name on the Deployment object.
// Helm, kustomize and most hand-written YAML put labels on the pod template
// only, so for real clusters that requirement matched nothing and the persona
// sat Pending forever. Resolution now walks an ordered fallback chain and
// reports which rung matched, so the failure mode is "we looked here, here and
// here" rather than silence.
package workload

import (
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
)

const (
	// LabelAppName is the Kubernetes recommended app label.
	LabelAppName = "app.kubernetes.io/name"
	// LabelApp is the common short-form app label.
	LabelApp = "app"
)

// Rung names, used verbatim in status messages and logs.
const (
	RungLabelAppName = "label " + LabelAppName
	RungLabelApp     = "label " + LabelApp
	RungName         = "metadata.name"
	RungSelector     = "spec.selector.matchLabels"
)

// matcher is one rung of the fallback chain: a human-readable name and the
// predicate that decides whether a Deployment belongs to the named persona.
type matcher struct {
	rung  string
	match func(*appsv1.Deployment, string) bool
}

// chain is the ordered fallback chain. Earlier rungs are more explicit
// statements of intent, so they win over later ones.
var chain = []matcher{
	{RungLabelAppName, func(d *appsv1.Deployment, name string) bool {
		return d.Labels[LabelAppName] == name
	}},
	{RungLabelApp, func(d *appsv1.Deployment, name string) bool {
		return d.Labels[LabelApp] == name
	}},
	{RungName, func(d *appsv1.Deployment, name string) bool {
		return d.Name == name
	}},
	{RungSelector, func(d *appsv1.Deployment, name string) bool {
		if d.Spec.Selector == nil {
			return false
		}
		ml := d.Spec.Selector.MatchLabels
		return ml[LabelAppName] == name || ml[LabelApp] == name
	}},
}

// AmbiguousError reports that a single rung matched more than one Deployment.
// Picking one arbitrarily is how a reliability tool ends up patching the wrong
// workload, so this is an error rather than a first-match-wins.
type AmbiguousError struct {
	// PersonaName is the persona spec.name that was being resolved.
	PersonaName string
	// Rung is the chain rung that produced the tie.
	Rung string
	// Candidates holds the matching Deployment names, sorted.
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%d Deployments match persona %q by %s (%s); set %s=%s on exactly one",
		len(e.Candidates), e.PersonaName, e.Rung, strings.Join(e.Candidates, ", "),
		LabelAppName, e.PersonaName)
}

// ChainDescription lists the rungs that were tried, for "we found nothing"
// messages. Naming the rungs is the difference between an actionable error and
// a dead end.
func ChainDescription() string {
	names := make([]string, 0, len(chain))
	for _, m := range chain {
		names = append(names, m.rung)
	}
	return strings.Join(names, ", ")
}

// Matches reports whether a Deployment belongs to the named persona by any rung
// of the chain. Used for event mapping and for "is this workload monitored?"
// questions, where ambiguity does not matter.
func Matches(deploy *appsv1.Deployment, personaName string) bool {
	if deploy == nil || personaName == "" {
		return false
	}
	for _, m := range chain {
		if m.match(deploy, personaName) {
			return true
		}
	}
	return false
}

// Resolve picks the Deployment described by personaName from the candidate set,
// walking the fallback chain in order. It returns the match and the rung that
// found it. No match at any rung returns (nil, "", nil): the caller decides how
// to report an unfound workload. A rung matching several Deployments returns an
// *AmbiguousError.
func Resolve(deployments []appsv1.Deployment, personaName string) (*appsv1.Deployment, string, error) {
	if personaName == "" {
		return nil, "", nil
	}

	for _, m := range chain {
		var matched []int
		for i := range deployments {
			if m.match(&deployments[i], personaName) {
				matched = append(matched, i)
			}
		}

		switch len(matched) {
		case 0:
			continue
		case 1:
			return &deployments[matched[0]], m.rung, nil
		default:
			candidates := make([]string, 0, len(matched))
			for _, i := range matched {
				candidates = append(candidates, deployments[i].Name)
			}
			sort.Strings(candidates)
			return nil, m.rung, &AmbiguousError{
				PersonaName: personaName,
				Rung:        m.rung,
				Candidates:  candidates,
			}
		}
	}

	return nil, "", nil
}
