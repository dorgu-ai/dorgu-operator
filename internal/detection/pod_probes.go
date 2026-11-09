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
	"sort"
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
//  2. LivenessRestarts and RestartLoop beat ReadinessNotReady. A container the
//     kubelet keeps killing is also not Ready, and that is the same fault.
//  3. Only application containers are examined. An init container that is not
//     Ready is just an init container that has not finished.

const (
	eventReasonUnhealthy = "Unhealthy"
	readinessEventPrefix = "Readiness probe failed"
	livenessEventPrefix  = "Liveness probe failed"

	// eventReasonKilling and livenessKillMarker identify the kubelet's record that
	// it killed a container for failing its liveness probe: reason Killing, message
	// "Container <name> failed liveness probe, will be restarted". It is the primary
	// evidence that a restart was a liveness failure, because the exit code cannot
	// say so: a liveness kill is a SIGTERM first, and an application that handles
	// it exits 0.
	eventReasonKilling = "Killing"
	livenessKillMarker = "failed liveness probe"

	// exitSigkill is the exit code of a container killed by SIGKILL, which is how
	// a liveness kill that outlasts the grace period ends.
	exitSigkill = 137

	terminatedReasonError = "Error"

	// maxEventReadsPerCycle bounds the kubelet-event lookups one collection makes.
	maxEventReadsPerCycle = 25

	// evidenceUnavailable is recorded in place of an event count when the events
	// could not be read, so a missing count is never mistaken for zero.
	evidenceUnavailable = "unavailable"
)

// probeSignals returns the restart-loop or readiness signal for a container, if
// any. found is what checkContainer already raised for it, which decides
// precedence. It always observes the restart count, even when it raises nothing,
// so the history is continuous.
func (p *PodCollector) probeSignals(
	ctx context.Context, pod corev1.Pod, cs corev1.ContainerStatus, now time.Time, found []Signal, book *eventBook,
) []Signal {
	delta, history := p.restarts.observe(restartKey(pod, cs), now, cs.RestartCount)

	if ownedByCrashOrOOM(found) {
		return nil
	}
	if sig, ok := p.checkRestartLoop(ctx, pod, cs, now, delta, history, book); ok {
		return []Signal{sig}
	}
	if sig, ok := p.checkReadiness(ctx, pod, cs, now, book); ok {
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

// checkRestartLoop detects a container restarted at least livenessRestarts times
// within livenessWindow, and says what kind of loop it is.
//
// The kubelet's own Killing events decide that. With one or more "failed liveness
// probe, will be restarted" events in the window, it is LivenessRestarts, whatever
// the exit code: a liveness kill is a SIGTERM first, so an application that
// handles it exits 0. With no such event, a restart loop that ends in an Error or
// a SIGKILL is reported as RestartLoop: an application that crashes quickly looks
// exactly like this, and calling it a probe failure would mislead.
//
// The pod status holds a lifetime restart count, not a history, so the delta comes
// from what the tracker observed on earlier cycles. First sight after an operator
// restart only seeds that history (a lifetime count is not evidence of anything in
// the window), but the kubelet's events are cluster state, not operator memory: at
// least livenessRestarts kill events in the window are enough on their own.
func (p *PodCollector) checkRestartLoop(
	ctx context.Context, pod corev1.Pod, cs corev1.ContainerStatus, now time.Time,
	delta int32, history restartHistory, book *eventBook,
) (Signal, bool) {
	last := cs.LastTerminationState.Terminated
	known := history == historyInWindow
	observedLoop := known && delta >= p.livenessRestarts
	if !p.loopWorthALookup(cs, now, observedLoop) {
		return Signal{}, false
	}

	since := now.Add(-p.livenessWindow)
	events := book.forPod(ctx, pod)
	kills, killEvidence := events.count(pod, cs, since, killMatch)
	unhealthy, unhealthyEvidence := events.count(pod, cs, since, unhealthyMatch(livenessEventPrefix))

	var typ SignalType
	switch {
	case kills >= 1 && observedLoop, kills >= p.livenessRestarts:
		typ = SignalLivenessRestarts
	case observedLoop && kills == 0 && killedByProbeOrError(last):
		typ = SignalRestartLoop
	default:
		return Signal{}, false
	}

	meta := containerMetadata(pod, cs)
	meta["livenessWindow"] = p.livenessWindow.String()
	meta["lastTerminationReason"] = last.Reason
	meta["lastTerminationExitCode"] = strconv.Itoa(int(last.ExitCode))
	meta["livenessKillEvents"] = killEvidence
	meta["livenessProbeEvents"] = unhealthyEvidence
	if known {
		meta["restartsInWindow"] = strconv.Itoa(int(delta))
	}

	return Signal{
		Type:       typ,
		Severity:   SeverityWarning,
		Category:   CategoryHealth,
		Source:     podCollectorName,
		Message:    p.loopMessage(typ, pod, cs, last, delta, known, kills, unhealthy),
		Resource:   podResource(pod),
		DetectedAt: now,
		Metadata:   meta,
	}, true
}

// loopWorthALookup is the cheap test for a restart loop: the container is running,
// its last exit was not an OOM kill or history, and either the delta says it is
// looping or it has at least the threshold of restarts and was killed within the
// window. Only then is the kubelet's evidence read.
func (p *PodCollector) loopWorthALookup(cs corev1.ContainerStatus, now time.Time, observedLoop bool) bool {
	last := cs.LastTerminationState.Terminated
	if cs.State.Running == nil || last == nil || last.Reason == string(SignalOOMKilled) || p.staleTermination(cs, last, now) {
		return false
	}
	recentKill := cs.RestartCount >= p.livenessRestarts && !last.FinishedAt.IsZero() &&
		now.Sub(last.FinishedAt.Time) <= p.livenessWindow
	return observedLoop || recentKill
}

// loopWorthALookup is also asked, before the signals are built, which pods will
// need their events read: see planEventReads.

// loopMessage words the signal. Counts only: nothing from an event's text.
func (p *PodCollector) loopMessage(
	typ SignalType, pod corev1.Pod, cs corev1.ContainerStatus, last *corev1.ContainerStateTerminated,
	delta int32, known bool, kills, unhealthy int32,
) string {
	subject := fmt.Sprintf("Container %s in pod %s/%s", cs.Name, pod.Namespace, pod.Name)
	restarts := ""
	if known {
		restarts = fmt.Sprintf(" restarted %d times within %s and", delta, p.livenessWindow)
	}
	if typ == SignalRestartLoop {
		return fmt.Sprintf("%s%s keeps restarting (last exit: %s); no kubelet event blames a probe",
			subject, restarts, terminationSummary(last))
	}
	msg := fmt.Sprintf("%s%s was killed for failing its liveness probe (%d kubelet liveness kill events, last exit: %s)",
		subject, restarts, kills, terminationSummary(last))
	if unhealthy > 0 {
		msg += fmt.Sprintf("; %d kubelet liveness probe failure events", unhealthy)
	}
	return msg
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
// The clock starts at the latest of the pod's ContainersReady transition, the
// container's own start, and the end of the readiness probe's initialDelaySeconds:
// a container that has just restarted is warming up whatever the pod condition
// says, and a probe that has not yet run has not failed. A container whose startup
// probe has not passed is still starting, not failing readiness, and a
// terminating pod is expected to go not Ready.
func (p *PodCollector) checkReadiness(
	ctx context.Context, pod corev1.Pod, cs corev1.ContainerStatus, now time.Time, book *eventBook,
) (Signal, bool) {
	since, notReadyFor, ok := p.readinessClock(pod, cs, now)
	if !ok {
		return Signal{}, false
	}

	events, evidence := book.forPod(ctx, pod).count(pod, cs, since, unhealthyMatch(readinessEventPrefix))
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

// readinessClock says whether a container has been not Ready for at least the
// threshold, and since when. It is the whole decision, without the evidence, so
// the collection can ask it up front which pods will want their events read.
func (p *PodCollector) readinessClock(pod corev1.Pod, cs corev1.ContainerStatus, now time.Time) (time.Time, time.Duration, bool) {
	running := cs.State.Running
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || running == nil || cs.Ready {
		return time.Time{}, 0, false
	}
	if cs.Started != nil && !*cs.Started {
		return time.Time{}, 0, false
	}

	since := running.StartedAt.Time
	if delay := readinessInitialDelay(pod, cs.Name); delay > 0 {
		since = since.Add(delay)
	}
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.ContainersReady && c.Status == corev1.ConditionFalse && c.LastTransitionTime.After(since) {
			since = c.LastTransitionTime.Time
		}
	}
	if since.IsZero() {
		return time.Time{}, 0, false
	}
	notReadyFor := now.Sub(since)
	if notReadyFor < p.readinessThreshold {
		return time.Time{}, 0, false
	}
	return since, notReadyFor, true
}

// readinessInitialDelay is the readiness probe's initialDelaySeconds for the named
// container, or zero when it has no probe or no delay.
func readinessInitialDelay(pod corev1.Pod, container string) time.Duration {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name == container && c.ReadinessProbe != nil {
			return time.Duration(c.ReadinessProbe.InitialDelaySeconds) * time.Second
		}
	}
	return 0
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
//
// Known limit: the container's start time is the node's clock and "now" is the
// operator's. A node whose clock runs behind by more than the grace makes a fresh
// kill look old, and it is then not reported; the other direction only keeps a
// signal alive longer. Clock skew on a managed cluster is normally milliseconds.
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

// eventBook reads the kubelet events of the pods a collection looks at, once per
// pod, and only for the pods planEventReads chose: at most maxEventReadsPerCycle
// pods per collection. Events are supporting
// evidence for most signals and the deciding evidence for one, and a node loss
// leaves hundreds of pods not Ready: the bound keeps that from becoming hundreds
// of API calls a minute. A pod past the bound has its evidence marked unavailable,
// exactly as if the read had failed, and keeps its signal.
type eventBook struct {
	p       *PodCollector
	allowed map[string]bool
	skipped int
	pods    map[string]podEvents
}

func newEventBook(p *PodCollector, allowed map[string]bool) *eventBook {
	return &eventBook{p: p, allowed: allowed, pods: make(map[string]podEvents)}
}

// podKey identifies a pod incarnation for the event reads.
func podKey(pod corev1.Pod) string {
	return fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, pod.UID)
}

// planEventReads decides which pods get their kubelet events read this
// collection. Reads are bounded, and not every pod that reaches the lookup needs
// them equally: for a restart loop that exits 0 the events are the ONLY evidence,
// and for readiness they are only supporting. So the pods that need them go first
// (a restart-loop candidate), the readiness-only pods take what is left, and each
// group rotates through a cursor across collections, so that when there are more
// pods than reads nobody is starved and nothing depends on the order the cache
// happens to list pods in.
func (p *PodCollector) planEventReads(pods []corev1.Pod, now time.Time) map[string]bool {
	var prio, rest []string
	for i := range pods {
		pod := pods[i]
		key := podKey(pod)
		isPrio, isRest := false, false
		for _, cs := range pod.Status.ContainerStatuses {
			if delta, history := p.restarts.peek(restartKey(pod, cs), now, cs.RestartCount); p.loopWorthALookup(
				cs, now, history == historyInWindow && delta >= p.livenessRestarts) {
				isPrio = true
				break
			}
			if _, _, ok := p.readinessClock(pod, cs, now); ok {
				isRest = true
			}
		}
		switch {
		case isPrio:
			prio = append(prio, key)
		case isRest:
			rest = append(rest, key)
		}
	}
	p.cursorMu.Lock()
	defer p.cursorMu.Unlock()
	return chooseReads(prio, rest, maxEventReadsPerCycle, &p.cursors)
}

// readCursors remember, per group, the last pod served, so the next collection
// carries on after it.
type readCursors struct {
	prio string
	rest string
}

// chooseReads picks up to budget pods: the priority ones first, then the rest with
// what remains. Each group is taken in sort order starting after the last pod it
// served, wrapping round, and the cursor moves to the last one taken. A cursor that
// is a key and not an index stays right when pods come and go between collections.
func chooseReads(prio, rest []string, budget int, cursors *readCursors) map[string]bool {
	chosen := make(map[string]bool, budget)
	for _, k := range takeFrom(prio, budget, &cursors.prio) {
		chosen[k] = true
	}
	for _, k := range takeFrom(rest, budget-len(chosen), &cursors.rest) {
		chosen[k] = true
	}
	return chosen
}

// takeFrom takes up to n keys in sort order, starting after *last and wrapping.
func takeFrom(keys []string, n int, last *string) []string {
	if n <= 0 || len(keys) == 0 {
		return nil
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	if len(sorted) <= n {
		*last = ""
		return sorted
	}
	start := sort.SearchStrings(sorted, *last)
	for start < len(sorted) && sorted[start] <= *last {
		start++
	}
	taken := make([]string, 0, n)
	for i := range n {
		taken = append(taken, sorted[(start+i)%len(sorted)])
	}
	*last = taken[len(taken)-1]
	return taken
}

// podEvents is one pod's events, or the fact that they could not be read.
type podEvents struct {
	items []corev1.Event
	ok    bool
}

// forPod returns the pod's events, reading them on first use.
func (b *eventBook) forPod(ctx context.Context, pod corev1.Pod) podEvents {
	key := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, pod.UID)
	if cached, ok := b.pods[key]; ok {
		return cached
	}
	result := podEvents{}
	switch {
	case b.p.events == nil:
	case !b.allowed[key]:
		b.skipped++
		b.p.logger.V(1).Info("this pod's kubelet events are not read this cycle; the signal stands without them",
			"namespace", pod.Namespace, "pod", pod.Name, "budget", maxEventReadsPerCycle)
	default:
		var list corev1.EventList
		if err := b.p.events.List(ctx, &list,
			client.InNamespace(pod.Namespace),
			client.MatchingFields{"involvedObject.name": pod.Name},
		); err != nil {
			b.p.logger.V(1).Info("could not read kubelet probe events; the signal stands without them",
				"namespace", pod.Namespace, "pod", pod.Name, "error", err.Error())
		} else {
			result = podEvents{items: list.Items, ok: true}
		}
	}
	b.pods[key] = result
	return result
}

// eventMatcher selects events by reason and message.
type eventMatcher struct {
	reason  string
	message func(string) bool
}

var killMatch = eventMatcher{
	reason:  eventReasonKilling,
	message: func(m string) bool { return strings.Contains(m, livenessKillMarker) },
}

func unhealthyMatch(prefix string) eventMatcher {
	return eventMatcher{reason: eventReasonUnhealthy, message: func(m string) bool { return strings.HasPrefix(m, prefix) }}
}

// count adds up the matching events for the container no older than since, and
// returns it as text for the signal's metadata: a number, or "unavailable" when the
// events could not be read, so a missing count is never mistaken for zero.
func (e podEvents) count(pod corev1.Pod, cs corev1.ContainerStatus, since time.Time, m eventMatcher) (int32, string) {
	if !e.ok {
		return 0, evidenceUnavailable
	}
	var total int32
	for i := range e.items {
		if ev := &e.items[i]; eventMatches(ev, pod, cs, m, since) {
			total += eventCountSince(ev, since)
		}
	}
	return total, strconv.Itoa(int(total))
}

func probeEventMatches(ev *corev1.Event, pod corev1.Pod, cs corev1.ContainerStatus, prefix string, since time.Time) bool {
	return eventMatches(ev, pod, cs, unhealthyMatch(prefix), since)
}

func eventMatches(ev *corev1.Event, pod corev1.Pod, cs corev1.ContainerStatus, m eventMatcher, since time.Time) bool {
	if ev.Reason != m.reason || !m.message(ev.Message) {
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

// eventCountSince is how many times an event happened inside the window. An
// aggregated event reports a count since its first occurrence, which can be days
// ago: when it began before the window, all that is known is that it happened at
// least once inside it (its last occurrence is, or the event would not match).
func eventCountSince(ev *corev1.Event, since time.Time) int32 {
	first := ev.FirstTimestamp.Time
	if ev.Series != nil {
		first = ev.EventTime.Time
	}
	if first.IsZero() || !first.Before(since) {
		return eventCount(ev)
	}
	return 1
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
//
// It stores the points where a container's count changed, plus the baseline at
// the edge of the window, not one sample per cycle: memory is proportional to the
// restarts in the window, however long the window or short the interval.
type restartTracker struct {
	window   time.Duration
	mu       sync.Mutex
	samples  map[string][]restartSample
	observed map[string]time.Time
}

// restartSample is a count and when it was first observed.
type restartSample struct {
	at    time.Time
	count int32
}

func newRestartTracker(window time.Duration) *restartTracker {
	return &restartTracker{
		window:   window,
		samples:  make(map[string][]restartSample),
		observed: make(map[string]time.Time),
	}
}

// restartHistory says what observe knows about a container.
type restartHistory int

const (
	// historyFirstSight: the collector has never seen this container.
	historyFirstSight restartHistory = iota
	// historyNone: it has, but no usable history: the count went down (a new
	// container under the same key) or the collector was not looking for longer
	// than the window, so a delta would span time it cannot account for.
	historyNone
	// historyInWindow: the delta is valid.
	historyInWindow
)

// observe records the current count and returns how many restarts happened since
// the window's start, and what history that rests on.
//
// Restarts are dated by when they were observed, so a restart can read up to one
// cycle younger than it is; and the delta counts from the earliest observation
// inside the window when the collector has not been looking that long, so it is a
// lower bound, never a claim about time the collector did not see.
func (t *restartTracker) observe(key string, now time.Time, count int32) (delta int32, history restartHistory) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delta, history, points := t.evaluate(key, now, count)
	t.samples[key] = points
	t.observed[key] = now
	return delta, history
}

// peek is observe without recording: what the tracker would say about this count
// now. The collection uses it to plan its event reads before it builds signals.
func (t *restartTracker) peek(key string, now time.Time, count int32) (int32, restartHistory) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delta, history, _ := t.evaluate(key, now, count)
	return delta, history
}

// evaluate computes the delta and history for a count, and the points to keep. It
// changes nothing; the caller holds the lock.
func (t *restartTracker) evaluate(key string, now time.Time, count int32) (int32, restartHistory, []restartSample) {
	seed := []restartSample{{at: now, count: count}}
	points, seen := t.samples[key]
	switch {
	case !seen:
		return 0, historyFirstSight, seed
	case now.Sub(t.observed[key]) > t.window, count < points[len(points)-1].count:
		return 0, historyNone, seed
	}

	points = append([]restartSample(nil), points...)
	if count > points[len(points)-1].count {
		points = append(points, restartSample{at: now, count: count})
	}
	// The baseline is the count at the window's edge: the latest point at or before
	// the cutoff, or the earliest one when the collector has not been looking that
	// long. Points before it can never be a baseline again.
	cutoff := now.Add(-t.window)
	base := 0
	for i := range points {
		if points[i].at.After(cutoff) {
			break
		}
		base = i
	}
	return count - points[base].count, historyInWindow, points[base:]
}

// retain drops every container not in seen.
func (t *restartTracker) retain(seen map[string]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.samples {
		if _, ok := seen[key]; !ok {
			delete(t.samples, key)
			delete(t.observed, key)
		}
	}
}

func (t *restartTracker) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.samples)
}
