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
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Probe failures as health signals (B9, D0.8 finding 1).
//
// Before this, a container that sat not Ready for four minutes, or one the
// kubelet killed six times on a failing liveness probe, raised nothing: the pod
// signals were OOMKilled, CrashLoopBackOff and ImagePullBackOff, and a probe
// failure shows up as none of them until the kubelet's back-off finally turns it
// into a crash loop.
//
// Precedence, so one container is one finding and never two:
//
//  1. CrashLoopBackOff or a live OOMKilled on the container owns it. The probe
//     signals below are not raised for that container; the rules already explain
//     a crash loop through its probe and OOM correlations.
//  2. LivenessRestarts beats ReadinessNotReady. A container the kubelet keeps
//     killing is also not Ready, and that is the same fault.
//  3. Only application containers are examined. An init container that is not
//     Ready is just an init container that has not finished.

const (
	eventReasonUnhealthy = "Unhealthy"
	readinessEventPrefix = "Readiness probe failed"
	livenessEventPrefix  = "Liveness probe failed"

	// exitSigkill is the exit code of a container killed by SIGKILL, which is how
	// a liveness kill that outlasts the grace period ends.
	exitSigkill = 137

	terminatedReasonError = "Error"

	// evidenceUnavailable is recorded in place of an event count when the events
	// could not be read, so a missing count is never mistaken for zero.
	evidenceUnavailable = "unavailable"
)

// probeSignals returns the readiness and liveness signal for a container, if any.
// found is what checkContainer already raised for it, which decides precedence.
// It always observes the restart count, even when it raises nothing, so the
// history is continuous.
func (p *PodCollector) probeSignals(ctx context.Context, pod corev1.Pod, cs corev1.ContainerStatus, now time.Time, found []Signal) []Signal {
	delta, history := p.restarts.observe(restartKey(pod, cs), now, cs.RestartCount)

	if ownedByCrashOrOOM(found) {
		return nil
	}
	if sig, ok := p.checkLiveness(ctx, pod, cs, now, delta, history); ok {
		return []Signal{sig}
	}
	if sig, ok := p.checkReadiness(ctx, pod, cs, now); ok {
		return []Signal{sig}
	}
	return nil
}

// ownedByCrashOrOOM reports whether the container already has the signal that
// outranks the probe signals.
func ownedByCrashOrOOM(found []Signal) bool {
	for i := range found {
		if found[i].Type == SignalCrashLoopBackOff || found[i].Type == SignalOOMKilled {
			return true
		}
	}
	return false
}

// checkLiveness detects a container restarted at least livenessRestarts times
// within livenessWindow whose last termination was an Error or a SIGKILL.
//
// The pod status holds a lifetime restart count, not a history, so the delta
// comes from samples the collector took on earlier cycles. A container the
// collector has never seen (the operator has just started) falls back to what
// the status does prove: a count of at least the threshold and a last kill
// inside the window. A container it has seen but not within the window (cycles
// were skipped) proves nothing either way and raises nothing.
func (p *PodCollector) checkLiveness(
	ctx context.Context, pod corev1.Pod, cs corev1.ContainerStatus, now time.Time, delta int32, history restartHistory,
) (Signal, bool) {
	last := cs.LastTerminationState.Terminated
	if cs.State.Running == nil || last == nil || p.staleTermination(cs, last, now) || !killedByProbeOrError(last) {
		return Signal{}, false
	}

	restarts := delta
	switch history {
	case historyNone:
		return Signal{}, false
	case historyFirstSight:
		recent := !last.FinishedAt.IsZero() && now.Sub(last.FinishedAt.Time) <= p.livenessWindow
		if !recent {
			return Signal{}, false
		}
		restarts = cs.RestartCount
	case historyInWindow:
	}
	if restarts < p.livenessRestarts {
		return Signal{}, false
	}

	events, evidence := p.probeEvents(ctx, pod, cs, livenessEventPrefix, now.Add(-p.livenessWindow))
	meta := containerMetadata(pod, cs)
	meta["restartsInWindow"] = strconv.Itoa(int(restarts))
	meta["livenessWindow"] = p.livenessWindow.String()
	meta["lastTerminationReason"] = last.Reason
	meta["lastTerminationExitCode"] = strconv.Itoa(int(last.ExitCode))
	meta["livenessProbeEvents"] = evidence

	msg := fmt.Sprintf("Container %s in pod %s/%s restarted %d times in %s (last exit: %s)",
		cs.Name, pod.Namespace, pod.Name, restarts, p.livenessWindow, terminationSummary(last))
	if events > 0 {
		msg += fmt.Sprintf("; %d kubelet liveness probe failure events", events)
	}

	return Signal{
		Type:       SignalLivenessRestarts,
		Severity:   SeverityWarning,
		Category:   CategoryHealth,
		Source:     podCollectorName,
		Message:    msg,
		Resource:   podResource(pod),
		DetectedAt: now,
		Metadata:   meta,
	}, true
}

// killedByProbeOrError reports whether a termination looks like a liveness kill
// or an application error: reason Error, or a SIGKILL that is not an OOM kill
// (that one is the resource signal's).
func killedByProbeOrError(t *corev1.ContainerStateTerminated) bool {
	if t.Reason == string(SignalOOMKilled) {
		return false
	}
	return t.Reason == terminatedReasonError || t.ExitCode == exitSigkill
}

func terminationSummary(t *corev1.ContainerStateTerminated) string {
	reason := t.Reason
	if reason == "" {
		reason = "terminated"
	}
	return fmt.Sprintf("%s, exit code %d", reason, t.ExitCode)
}

// checkReadiness detects a running container that has stayed not Ready for at
// least readinessThreshold.
//
// The clock starts at the later of the pod's ContainersReady transition and the
// container's own start: a container that has just restarted is warming up
// whatever the pod condition says. A container whose startup probe has not
// passed is still starting, not failing readiness, and a terminating pod is
// expected to go not Ready.
func (p *PodCollector) checkReadiness(ctx context.Context, pod corev1.Pod, cs corev1.ContainerStatus, now time.Time) (Signal, bool) {
	running := cs.State.Running
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || running == nil || cs.Ready {
		return Signal{}, false
	}
	if cs.Started != nil && !*cs.Started {
		return Signal{}, false
	}

	since := running.StartedAt.Time
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.ContainersReady && c.Status == corev1.ConditionFalse && c.LastTransitionTime.After(since) {
			since = c.LastTransitionTime.Time
		}
	}
	if since.IsZero() {
		return Signal{}, false
	}
	notReadyFor := now.Sub(since)
	if notReadyFor < p.readinessThreshold {
		return Signal{}, false
	}

	events, evidence := p.probeEvents(ctx, pod, cs, readinessEventPrefix, since)
	meta := containerMetadata(pod, cs)
	meta["notReadyFor"] = notReadyFor.Round(time.Second).String()
	meta["readinessThreshold"] = p.readinessThreshold.String()
	meta["readinessProbeEvents"] = evidence

	msg := fmt.Sprintf("Container %s in pod %s/%s has not been Ready for %s (threshold %s)",
		cs.Name, pod.Namespace, pod.Name, notReadyFor.Round(time.Second), p.readinessThreshold)
	if events > 0 {
		msg += fmt.Sprintf("; %d kubelet readiness probe failure events", events)
	}

	return Signal{
		Type:       SignalReadinessNotReady,
		Severity:   SeverityWarning,
		Category:   CategoryHealth,
		Source:     podCollectorName,
		Message:    msg,
		Resource:   podResource(pod),
		DetectedAt: since,
		Metadata:   meta,
	}, true
}

// staleTermination reports whether a last-terminated state is history rather
// than a live fact.
//
// A pod keeps lastState.terminated until the pod is replaced, so an OOM kill from
// an hour ago kept raising a signal, an incident and a model call every cycle for
// a container that had run fine since (D0.8 finding 3). A termination is history
// when the container is Running and Ready, started after the termination
// finished, and has stayed up for staleAfter. The grace is what keeps a kill the
// operator has not yet looked at live: see DefaultTerminatedStaleAfter.
func (p *PodCollector) staleTermination(cs corev1.ContainerStatus, t *corev1.ContainerStateTerminated, now time.Time) bool {
	running := cs.State.Running
	if running == nil || !cs.Ready || t.FinishedAt.IsZero() || running.StartedAt.IsZero() {
		return false
	}
	if !t.FinishedAt.Time.Before(running.StartedAt.Time) {
		return false
	}
	return now.Sub(running.StartedAt.Time) >= p.staleAfter
}

// probeEvents counts the kubelet's Unhealthy events for the pod's container whose
// message starts with prefix and that are no older than since. It is supporting
// evidence only: it is never what raises a signal, and a failure to read events
// leaves the signal standing with the count marked unavailable.
func (p *PodCollector) probeEvents(
	ctx context.Context, pod corev1.Pod, cs corev1.ContainerStatus, prefix string, since time.Time,
) (int32, string) {
	if p.events == nil {
		return 0, evidenceUnavailable
	}
	var list corev1.EventList
	if err := p.events.List(ctx, &list,
		client.InNamespace(pod.Namespace),
		client.MatchingFields{"involvedObject.name": pod.Name},
	); err != nil {
		p.logger.V(1).Info("could not read kubelet probe events; the signal stands without them",
			"namespace", pod.Namespace, "pod", pod.Name, "error", err.Error())
		return 0, evidenceUnavailable
	}

	var total int32
	for i := range list.Items {
		ev := &list.Items[i]
		if !probeEventMatches(ev, pod, cs, prefix, since) {
			continue
		}
		total += eventCount(ev)
	}
	return total, strconv.Itoa(int(total))
}

func probeEventMatches(ev *corev1.Event, pod corev1.Pod, cs corev1.ContainerStatus, prefix string, since time.Time) bool {
	if ev.Reason != eventReasonUnhealthy || !strings.HasPrefix(ev.Message, prefix) {
		return false
	}
	io := ev.InvolvedObject
	if io.Kind != "Pod" || io.Name != pod.Name || (io.UID != "" && pod.UID != "" && io.UID != pod.UID) {
		return false
	}
	// "spec.containers{main}" names the container when the kubelet says so.
	if io.FieldPath != "" && !strings.Contains(io.FieldPath, "{"+cs.Name+"}") {
		return false
	}
	return !eventTime(ev).Before(since)
}

func eventTime(ev *corev1.Event) time.Time {
	switch {
	case ev.Series != nil && !ev.Series.LastObservedTime.IsZero():
		return ev.Series.LastObservedTime.Time
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	default:
		return ev.CreationTimestamp.Time
	}
}

func eventCount(ev *corev1.Event) int32 {
	if ev.Series != nil && ev.Series.Count > 0 {
		return ev.Series.Count
	}
	if ev.Count > 0 {
		return ev.Count
	}
	return 1
}

// restartKey identifies one container of one pod incarnation. The UID matters: a
// replacement pod of the same name starts its count from zero.
func restartKey(pod corev1.Pod, cs corev1.ContainerStatus) string {
	return fmt.Sprintf("%s/%s/%s/%s", pod.Namespace, pod.Name, pod.UID, cs.Name)
}

// restartTracker remembers recent restart counts per container, because the pod
// status only holds the lifetime count. It is safe for concurrent use: the
// health-check loop and the remediation verifier both collect.
type restartTracker struct {
	window  time.Duration
	mu      sync.Mutex
	samples map[string][]restartSample
}

type restartSample struct {
	at    time.Time
	count int32
}

func newRestartTracker(window time.Duration) *restartTracker {
	return &restartTracker{window: window, samples: make(map[string][]restartSample)}
}

// restartHistory says what observe knows about a container.
type restartHistory int

const (
	// historyFirstSight: the collector has never seen this container.
	historyFirstSight restartHistory = iota
	// historyNone: it has, but not within the window, so no delta can be given.
	historyNone
	// historyInWindow: an earlier sample is inside the window; the delta is valid.
	historyInWindow
)

// observe records the current count and returns how many restarts happened since
// the earliest sample still inside the window, and what history that rests on.
//
// The delta is a lower bound by construction: it counts from the earliest sample
// in the window, never from before it, so it can miss a restart between the
// window's edge and that sample but can never claim one that was outside it.
func (t *restartTracker) observe(key string, now time.Time, count int32) (delta int32, history restartHistory) {
	t.mu.Lock()
	defer t.mu.Unlock()

	previous, seenBefore := t.samples[key]
	cutoff := now.Add(-t.window)
	kept := make([]restartSample, 0, len(previous)+1)
	for _, s := range previous {
		// A count that went down is a different container under the same key: start over.
		if s.at.Before(cutoff) || s.count > count {
			continue
		}
		kept = append(kept, s)
	}
	switch {
	case len(kept) > 0:
		history, delta = historyInWindow, count-kept[0].count
	case seenBefore:
		history = historyNone
	default:
		history = historyFirstSight
	}
	t.samples[key] = append(kept, restartSample{at: now, count: count})
	return delta, history
}

// retain drops every container not in seen.
func (t *restartTracker) retain(seen map[string]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.samples {
		if _, ok := seen[key]; !ok {
			delete(t.samples, key)
		}
	}
}

func (t *restartTracker) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.samples)
}
