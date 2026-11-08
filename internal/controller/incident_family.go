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
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

// One fault, one incident, as it worsens (B9 review F-1).
//
// An incident is an application and a signal. A container whose probes fail does
// not stay in one shape: it is not Ready, then the kubelet starts restarting it,
// then it is in CrashLoopBackOff, and one of those restarts may be an OOM kill.
// Detection reports one shape per container per cycle, but each shape used to be
// its own incident, each with its own diagnosis stream and proposal, while the
// earlier ones waited out the six minute recovery window.
//
// The signals of that progression form a family, ranked by how far along the fault
// is. When a diagnosis arrives whose signal is in the family and the application
// already has an open incident of another signal in it that names a pod the
// diagnosis also names:
//
//   - a higher rank ESCALATES the open incident: its signal, category and severity
//     become the new ones, and the incident keeps its name, firstSeen, count and
//     history (the signals it had are listed in dorgu.io/escalated-from);
//   - an equal or lower rank is FOLDED into it: the sighting is counted and
//     lastSeen refreshed, nothing else changes, and the incident stays active. A
//     fault does not get less serious because one container is, for a moment, at an
//     earlier rung.
//
// The identity of a fault is therefore the application, the pod and the family.
// The alternative, a pod-and-container key replacing the incident name, was
// rejected: incident names, lineages and every cooldown are keyed on the
// application and signal, so a new key would have to be threaded through all of
// them, and an incident has never recorded containers. Merging at the incident,
// by shared pod, gets the same result without touching identity: two pods of one
// application failing in different ways are still two incidents (a pod that is not
// the one named is a different fault), while one pod's fault, whichever shape it
// is in, is one. Containers of one pod share a pod, so they merge; a pod replaced
// by a rollout starts a new fault, as it always did.
//
// A known consequence: an outstanding proposal stays the incident's proposal when
// it escalates, so the proposal for the worse shape waits for the earlier one to be
// answered or to expire, as any second proposal for one incident does.

// faultRank orders the signals of the family. Liveness, with the kubelet's own
// event behind it, outranks a restart loop of unknown cause.
var faultRank = map[detection.SignalType]int{
	detection.SignalReadinessNotReady: 1,
	detection.SignalRestartLoop:       2,
	detection.SignalLivenessRestarts:  3,
	detection.SignalCrashLoopBackOff:  4,
	detection.SignalOOMKilled:         5,
}

func rankOf(signal string) (int, bool) {
	rank, ok := faultRank[detection.SignalType(signal)]
	return rank, ok
}

// findFamilyPeer returns the application's open incident of a different signal in
// the same family as the diagnosis, the highest ranked if there are several, or nil.
func (r *HealthCheckReconciler) findFamilyPeer(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
) (*dorguv1.IncidentMemory, error) {
	signal := primarySignalType(diag)
	if _, ok := faultRank[signal]; !ok {
		return nil, nil
	}

	opts := []client.ListOption{client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{
		LabelPersonaKind: subject.personaRef.Kind,
		LabelPersonaName: subject.personaRef.Name,
	})}}
	if subject.namespace != "" {
		opts = append(opts, client.InNamespace(subject.namespace))
	}
	var list dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing IncidentMemories of the same family: %w", err)
	}

	var best *dorguv1.IncidentMemory
	bestRank := 0
	for i := range list.Items {
		im := &list.Items[i]
		rank, inFamily := rankOf(im.Spec.Detection.Signal)
		if !inFamily || isResolvedIncident(im) || !subject.matchesIncident(im) || im.Spec.Detection.Signal == string(signal) {
			continue
		}
		if !sharesPod(im.Spec.Detection.AffectedResources, diag.AffectedResources) {
			continue
		}
		if rank > bestRank {
			best, bestRank = im, rank
		}
	}
	return best, nil
}

// sharesPod reports whether two resource lists name a pod in common.
func sharesPod(a, b []dorguv1.ResourceReference) bool {
	for i := range a {
		if a[i].Kind != "Pod" {
			continue
		}
		for j := range b {
			if b[j].Kind == "Pod" && b[j].Name == a[i].Name && b[j].Namespace == a[i].Namespace {
				return true
			}
		}
	}
	return false
}

// mergeIntoPeer folds a diagnosis into the open incident of the same fault, and
// reports whether the incident should now be proposed for: only an escalation is,
// since a fold adds nothing a proposal has not already answered.
func (r *HealthCheckReconciler) mergeIntoPeer(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	peer *dorguv1.IncidentMemory,
	now metav1.Time,
) (propose bool, err error) {
	newRank, _ := rankOf(string(primarySignalType(diag)))
	peerRank, _ := rankOf(peer.Spec.Detection.Signal)

	if newRank > peerRank {
		from := peer.Spec.Detection.Signal
		if err := r.writeIncident(ctx, peer, diag, now, incidentEdit{
			spec: func(fresh *dorguv1.IncidentMemory) { escalateSpec(fresh, subject, diag, now) },
		}); err != nil {
			return false, err
		}
		r.Logger.Info("escalated an open incident instead of opening another",
			"name", peer.Name, "from", from, "to", peer.Spec.Detection.Signal, "category", peer.Spec.Category)
		r.storeSignalEvents(ctx, diag, peer)
		if diag.Severity == detection.SeverityWarning || diag.Severity == detection.SeverityCritical {
			r.emitK8sEvents(ctx, diag)
		}
		return true, nil
	}

	err = r.writeIncident(ctx, peer, diag, now, incidentEdit{
		spec: func(fresh *dorguv1.IncidentMemory) { fresh.Spec.Detection.LastSeen = now },
	})
	return false, err
}

// escalateSpec rewrites an incident's spec for the worse shape of its fault. It
// never lowers the severity, and it keeps the incident's identity, firstSeen and
// the trail of signals it passed through.
func escalateSpec(im *dorguv1.IncidentMemory, subject incidentSubject, diag *diagnosis.Diagnosis, now metav1.Time) {
	from := im.Spec.Detection.Signal
	applyDiagnosisToSpec(im, subject, diag, now)

	signal := string(primarySignalType(diag))
	im.Spec.Detection.Signal = signal
	im.Spec.Category = diag.Category
	if detection.SeverityRank(diag.Severity) > detection.SeverityRank(detection.Severity(im.Spec.Severity)) {
		im.Spec.Severity = string(diag.Severity)
	}
	// The earlier root cause described the earlier shape; the new one replaces it
	// outright, where an ordinary refresh would keep a more confident old one.
	if rc := annotateUnattributed(buildRootCause(diag), subject); rc != nil {
		im.Spec.RootCause = rc
	}

	im.Labels[LabelCategory] = im.Spec.Category
	im.Labels[LabelSignal] = signal
	im.Labels[LabelSeverity] = im.Spec.Severity

	if im.Annotations == nil {
		im.Annotations = map[string]string{}
	}
	trail := []string{from}
	if prior := im.Annotations[AnnotationEscalatedFrom]; prior != "" {
		trail = append(strings.Split(prior, ","), from)
	}
	im.Annotations[AnnotationEscalatedFrom] = strings.Join(trail, ",")
}
