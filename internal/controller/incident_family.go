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
	"slices"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// One fault, one incident, as it worsens and as it recovers (B9 review F-1, N-1).
//
// An incident is an application and a signal, so a fault that changes shape (not
// Ready, then restarted by the kubelet, then CrashLoopBackOff, then OOM-killed) used
// to open an incident per shape. The signals of that progression are a family,
// ranked by detection.FaultRank, and the identity of a fault is the application, the
// CONTAINER and the family.
//
// When a diagnosis arrives whose signal is in the family and the application already
// has an open incident of another signal in it, for the same container:
//
//   - a worse signal ESCALATES the incident: signal, category and severity become the
//     new ones, a Pending proposal for the milder shape is superseded (see
//     incident_supersede.go), and the signals it had are kept in dorgu.io/escalated-from;
//   - a milder signal while the worse one is still being seen for the container is a
//     rung of the same fault and is FOLDED in: counted, lastSeen refreshed, nothing
//     else changes;
//   - a milder signal once the worse one has cleared DE-ESCALATES it: the signal,
//     category and severity follow the current state down, and the trail records what
//     it was.
//
// The incident keeps its name, firstSeen and count throughout.
//
// Containers are separate faults. Two containers of one pod, a sidecar included, are
// two incidents whichever pod they share: the incident records the containers it is
// about (dorgu.io/containers), and a diagnosis only merges into one that names one of
// them (or none, for an incident written before the annotation). The pod is NOT part
// of the key, so a pod replaced by a rollout still merges. What keeps two pods of one
// application, failing in different ways at the same time, apart is time: a peer whose
// pods are still showing a signal in this very cycle is a different fault from one on
// a pod nobody else is reporting.
//
// The alternative to merging at the incident, a new identity in the incident name,
// was rejected: names, lineages and every cooldown are keyed on application and
// signal, so a new key would have to be threaded through all of them.

// rankOf is a signal's place in the progression, and whether it is in it.
func rankOf(signal string) (int, bool) { return detection.FaultRank(detection.SignalType(signal)) }

const kindPod = "Pod"

// familyView is what one application's signals say this cycle: which pods are
// showing a signal of the family, and which signals are live for which containers.
// It is what lets a merge tell a pod that was replaced from a second pod failing.
type familyView struct {
	pods    map[string]bool
	signals map[string]bool
}

type familyViewKey struct{}

// newFamilyView builds the view from one application's signals.
func newFamilyView(signals []detection.Signal) *familyView {
	v := &familyView{pods: map[string]bool{}, signals: map[string]bool{}}
	for i := range signals {
		sig := &signals[i]
		if _, ok := detection.FaultRank(sig.Type); !ok {
			continue
		}
		if sig.Resource.Kind == kindPod {
			v.pods[sig.Resource.Namespace+"/"+sig.Resource.Name] = true
		}
		v.signals[sig.Metadata["container"]+"|"+string(sig.Type)] = true
	}
	return v
}

func withFamilyView(ctx context.Context, v *familyView) context.Context {
	return context.WithValue(ctx, familyViewKey{}, v)
}

// familyViewFrom returns the view the cycle stored, or an empty one: with nothing
// known to be live, nothing is held apart and nothing is held together.
func familyViewFrom(ctx context.Context) *familyView {
	if v, ok := ctx.Value(familyViewKey{}).(*familyView); ok && v != nil {
		return v
	}
	return &familyView{pods: map[string]bool{}, signals: map[string]bool{}}
}

// signalLive reports whether the signal is being seen this cycle for a container the
// incident is about.
func (v *familyView) signalLive(signal string, containers []string) bool {
	for key := range v.signals {
		container, typ, _ := strings.Cut(key, "|")
		if typ == signal && containersOverlap(containers, []string{container}) {
			return true
		}
	}
	return false
}

// anyPodLive reports whether any of the resources is a pod showing a signal this cycle.
func (v *familyView) anyPodLive(refs []dorguv1.ResourceReference) bool {
	for _, ref := range refs {
		if ref.Kind == kindPod && v.pods[ref.Namespace+"/"+ref.Name] {
			return true
		}
	}
	return false
}

// containersOf lists the containers a diagnosis is about, from its contributing
// signals, sorted and without repeats. A signal that names none contributes none.
func containersOf(diag *diagnosis.Diagnosis) []string {
	seen := map[string]bool{}
	for _, cs := range diag.Contributing {
		if c := cs.Signal.Metadata["container"]; c != "" {
			seen[c] = true
		}
	}
	return sortedKeys(seen)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// incidentContainers reads the containers an incident is about.
func incidentContainers(im *dorguv1.IncidentMemory) []string {
	raw := im.Annotations[AnnotationContainers]
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// containersOverlap reports whether two container lists are about a common
// container. An empty list means the container is unknown (an incident written
// before the annotation, a signal that names none) and overlaps with anything.
func containersOverlap(a, b []string) bool {
	a, b = nonEmpty(a), nonEmpty(b)
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// applicationIncidents lists every incident of the subject's application.
func (r *HealthCheckReconciler) applicationIncidents(ctx context.Context, subject incidentSubject) ([]dorguv1.IncidentMemory, error) {
	opts := []client.ListOption{client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{
		LabelPersonaKind: subject.personaRef.Kind,
		LabelPersonaName: subject.personaRef.Name,
	})}}
	if subject.namespace != "" {
		opts = append(opts, client.InNamespace(subject.namespace))
	}
	var list dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing the application's IncidentMemories: %w", err)
	}
	return list.Items, nil
}

// findFamilyPeer returns the open incident of a different signal in the same family
// that this diagnosis is the same fault as, the highest ranked if there are several,
// or nil.
func (r *HealthCheckReconciler) findFamilyPeer(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
) (*dorguv1.IncidentMemory, error) {
	signal := primarySignalType(diag)
	if _, ok := detection.FaultRank(signal); !ok {
		return nil, nil
	}
	incidents, err := r.applicationIncidents(ctx, subject)
	if err != nil {
		return nil, err
	}

	view := familyViewFrom(ctx)
	containers := containersOf(diag)
	var best *dorguv1.IncidentMemory
	bestRank := 0
	for i := range incidents {
		im := &incidents[i]
		rank, inFamily := rankOf(im.Spec.Detection.Signal)
		if !inFamily || isResolvedIncident(im) || !subject.matchesIncident(im) || im.Spec.Detection.Signal == string(signal) {
			continue
		}
		if !containersOverlap(incidentContainers(im), containers) {
			continue
		}
		if !sharesPod(im.Spec.Detection.AffectedResources, diag.AffectedResources) &&
			view.anyPodLive(im.Spec.Detection.AffectedResources) {
			// Another pod, failing right now: a different fault.
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
		if a[i].Kind != kindPod {
			continue
		}
		for j := range b {
			if b[j].Kind == kindPod && b[j].Name == a[i].Name && b[j].Namespace == a[i].Namespace {
				return true
			}
		}
	}
	return false
}

// mergeIntoPeer records a diagnosis against the incident its fault already has, and
// reports whether the incident should now be proposed for: an escalation or a
// de-escalation is, since the incident now says something a proposal has not answered;
// a fold adds nothing.
func (r *HealthCheckReconciler) mergeIntoPeer(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	peer *dorguv1.IncidentMemory,
	now metav1.Time,
) (propose bool, err error) {
	newRank, _ := rankOf(string(primarySignalType(diag)))
	peerRank, _ := rankOf(peer.Spec.Detection.Signal)
	from := peer.Spec.Detection.Signal

	switch {
	case newRank > peerRank:
		if err := r.writeIncident(ctx, peer, diag, now, incidentEdit{
			spec: func(fresh *dorguv1.IncidentMemory) { reshapeSpec(fresh, subject, diag, now, false) },
		}); err != nil {
			return false, err
		}
		r.Logger.Info("escalated an open incident instead of opening another",
			"name", peer.Name, "from", from, "to", peer.Spec.Detection.Signal, "category", peer.Spec.Category)
		r.storeSignalEvents(ctx, diag, peer)
		if diag.Severity == detection.SeverityWarning || diag.Severity == detection.SeverityCritical {
			r.emitK8sEvents(ctx, diag)
		}
		// The proposal made for the milder shape is advice for a fault that is now a
		// worse one. A failure here only delays the proposal for the worse shape.
		if err := r.supersedePending(ctx, peer, from); err != nil {
			logFailure(r.Logger, err, "could not supersede the proposal for the milder shape", "incident", peer.Name)
		}
		return true, nil

	case familyViewFrom(ctx).signalLive(from, incidentContainers(peer)):
		err = r.writeIncident(ctx, peer, diag, now, incidentEdit{
			spec: func(fresh *dorguv1.IncidentMemory) { fresh.Spec.Detection.LastSeen = now },
		})
		return false, err

	default:
		// The worse signal has cleared; the fault is still there in a milder shape.
		if err := r.writeIncident(ctx, peer, diag, now, incidentEdit{
			spec: func(fresh *dorguv1.IncidentMemory) { reshapeSpec(fresh, subject, diag, now, true) },
		}); err != nil {
			return false, err
		}
		r.Logger.Info("de-escalated an open incident: its worse signal has cleared",
			"name", peer.Name, "from", from, "to", peer.Spec.Detection.Signal)
		return true, nil
	}
}

// reshapeSpec rewrites an incident's spec for the shape its fault has now: the signal,
// category, labels and root cause become the diagnosis's, the containers it is about
// are the union, and what it was is appended to the trail. Severity follows the
// diagnosis down when lower is set (a de-escalation) and only ever up otherwise. The
// incident keeps its identity, firstSeen and count.
func reshapeSpec(im *dorguv1.IncidentMemory, subject incidentSubject, diag *diagnosis.Diagnosis, now metav1.Time, lower bool) {
	from := im.Spec.Detection.Signal
	applyDiagnosisToSpec(im, subject, diag, now)

	signal := string(primarySignalType(diag))
	im.Spec.Detection.Signal = signal
	im.Spec.Category = diag.Category
	if lower || detection.SeverityRank(diag.Severity) > detection.SeverityRank(detection.Severity(im.Spec.Severity)) {
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
	if from != signal {
		im.Annotations[AnnotationEscalatedFrom] = appendTrail(im.Annotations[AnnotationEscalatedFrom], from)
	}
	if fromRank, _ := rankOf(from); !lower && fromRank < mustRank(signal) {
		im.Annotations[gate.AnnotationEscalatedAt] = now.UTC().Format(time.RFC3339)
	}
	im.Annotations[AnnotationContainers] = strings.Join(unionContainers(incidentContainers(im), containersOf(diag)), ",")
	if im.Annotations[AnnotationContainers] == "" {
		delete(im.Annotations, AnnotationContainers)
	}
}

func mustRank(signal string) int {
	rank, _ := rankOf(signal)
	return rank
}

// appendTrail adds a signal to the escalated-from trail, keeping the most recent
// maxEscalationTrail entries: a fault that flickers must not grow the object.
func appendTrail(trail, signal string) string {
	entries := []string{signal}
	if trail != "" {
		entries = append(strings.Split(trail, ","), signal)
	}
	if len(entries) > maxEscalationTrail {
		entries = entries[len(entries)-maxEscalationTrail:]
	}
	return strings.Join(entries, ",")
}

func unionContainers(a, b []string) []string {
	set := map[string]bool{}
	for _, c := range nonEmpty(a) {
		set[c] = true
	}
	for _, c := range nonEmpty(b) {
		set[c] = true
	}
	return sortedKeys(set)
}
