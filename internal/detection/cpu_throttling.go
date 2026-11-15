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
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// CPU throttling detection (C4, P2-D11).
//
// Two sources, one signal. The default reads metrics.k8s.io, which every
// metrics-server cluster has: a container whose CPU usage is at or above 95% of its
// CPU limit for three detection cycles in a row is, in practice, being throttled.
// The optional one reads the cfs throttled-period ratio from a Prometheus-compatible
// URL in the ClusterPersona, which is the throttling itself. Prometheus wins when
// both are present. Neither needs RBAC beyond get/list on pods.metrics.k8s.io, and
// nothing here ever touches a kubelet: nodes/proxy is an exec path into the node's
// pods and is never granted.
const (
	collectorNameCPUThrottling = "cpu-throttling-checker"

	// cpuUsageLimitPercent is the share of the limit a container must be using.
	cpuUsageLimitPercent = 95

	// CPUUsageLimitRatio is cpuUsageLimitPercent as a ratio.
	CPUUsageLimitRatio = float64(cpuUsageLimitPercent) / 100

	// CPUUsageConsecutiveCycles is how many detection cycles in a row a container
	// must be at the ratio before it is a signal. One busy sample is a burst; three
	// minutes at the limit is a ceiling.
	CPUUsageConsecutiveCycles = 3

	// DefaultMinCycleSpacing is how close two Collect calls may be before the second
	// is the same cycle. The verifier runs detection too, and a verification must
	// not count as another cycle toward the threshold.
	//
	// What counts as a cycle, exactly: a Collect call made at least the spacing after
	// the previous cycle ran (the operator sets it to half the health-check interval).
	// A call closer than that is answered from the previous cycle and counts for
	// nothing. A cycle that could not look (an error reading pods or metrics) resets
	// every container's streak, so "3 consecutive cycles" means three consecutive
	// cycles that all read the metrics. The window is wall-clock, not the caller's
	// identity: a verifier call that lands later than the spacing after a cycle is
	// counted as one, and the next real cycle shortly after is then answered from it.
	// The effect is a signal one cycle earlier or later, never a missed one.
	DefaultMinCycleSpacing = 30 * time.Second

	// PrometheusCriticalRatio is the throttled-period ratio at or above which the
	// signal is Critical: half the container's scheduling periods are throttled. It is
	// the only way to a Critical CPUThrottling signal. Usage at the limit from
	// metrics.k8s.io is evidence of throttling and not a measurement of it, so that path
	// is always a Warning.
	PrometheusCriticalRatio = 0.5
)

// Sources and reasons of CPU throttling detection (see SourceStatus).
const (
	CPUThrottlingSourceMetricsServer = dorguv1.CPUThrottlingSourceMetricsServer
	CPUThrottlingSourcePrometheus    = dorguv1.CPUThrottlingSourcePrometheus
	CPUThrottlingSourceNone          = dorguv1.CPUThrottlingSourceNone

	ReasonMetricsServerActive   = "MetricsServerActive"
	ReasonPrometheusActive      = "PrometheusActive"
	ReasonPrometheusUnreachable = "PrometheusUnreachable"
	ReasonPrometheusNoData      = "PrometheusNoData"
	ReasonPrometheusURLInvalid  = "PrometheusURLInvalid"
	ReasonNoMetricsSource       = "NoMetricsSource"
	ReasonMetricsServerDisabled = "MetricsServerDisabled"
	ReasonMetricsSourceLost     = "MetricsSourceLost"
	ReasonMetricsReadFailed     = "MetricsReadFailed"
)

// The sentences a status carries. They are fixed text: they say what the operator
// knows, never what a remote server wrote, because they are shown to people and read
// by tools.
const (
	msgMetricsServerActive = "CPU throttling detection reads metrics.k8s.io: a container at 95% of its CPU limit for 3 cycles is throttled."
	msgPrometheusActive    = "CPU throttling detection reads the throttled-period ratio from Prometheus: 25% or more of periods throttled."
	msgFallback            = "The Prometheus endpoint in spec.metrics.prometheus.url could not be read, so CPU throttling detection reads metrics.k8s.io instead."
	msgFallbackNoData      = "The Prometheus endpoint in spec.metrics.prometheus.url answered with no CPU throttling series for the containers in this cluster " +
		"(cAdvisor metrics may not be scraped, or their CFS counters never advance), so CPU throttling detection reads metrics.k8s.io instead."
	msgFallbackURLInvalid = "spec.metrics.prometheus.url is not a valid http or https URL, so CPU throttling detection reads metrics.k8s.io instead."
	msgNoSource           = "CPU throttling detection is off: metrics.k8s.io is not served and spec.metrics.prometheus.url is not set. " +
		"Install metrics-server or set spec.metrics.prometheus.url on the ClusterPersona."
	msgServerDisabled = "CPU throttling detection is off: metrics-server detection is disabled (--enable-metrics-server=false) and " +
		"spec.metrics.prometheus.url is not set. Enable it or set spec.metrics.prometheus.url on the ClusterPersona."
	msgPrometheusDown = "CPU throttling detection is off: the Prometheus endpoint in spec.metrics.prometheus.url could not be read and " +
		"metrics.k8s.io is not available."
	msgPrometheusNoData = "CPU throttling detection is off: the Prometheus endpoint in spec.metrics.prometheus.url answered with no CPU throttling series " +
		"for the containers in this cluster, or only series with no CFS periods, and metrics.k8s.io is not available."
	msgPrometheusURLInvalid = "CPU throttling detection is off: spec.metrics.prometheus.url is not a valid http or https URL and " +
		"metrics.k8s.io is not available."
	msgSourceLost = "CPU throttling detection is off: metrics.k8s.io was served and is not any more, so a quiet cycle proves nothing."
	msgReadFailed = "CPU throttling detection is off this cycle: metrics.k8s.io or the pod list could not be read."
)

// SourceStatus is what the collector says about itself each cycle: whether it can
// see, through which source, and why not. Active false with a Reason is "detection is
// off", not "all is well".
type SourceStatus struct {
	// Active is true when a source was read this cycle.
	Active bool
	// Source is one of the CPUThrottlingSource* values.
	Source string
	// Reason is one of the Reason* tokens.
	Reason string
	// Message is a fixed sentence for people.
	Message string
}

// CPUThrottlingStatusSink receives the collector's SourceStatus every cycle. It is how
// the ClusterPersona status, and with it `dorgu health`, learns that CPU throttling
// detection is off and why. Implementations must not block for long and must not fail
// the caller.
type CPUThrottlingStatusSink interface {
	ReportCPUThrottlingSource(ctx context.Context, status SourceStatus)
}

// CPUThrottlingCollector raises SignalCPUThrottling for containers held back by
// their own CPU limit.
//
// A source that does not exist (no metrics-server, no Prometheus URL) is a missing
// capability: detection is off, loudly in the status and quietly in the logs, and
// Collect returns no error. A source that exists and cannot be read, or one that was
// there and is gone, is a collector that could not look, and Collect returns the
// error.
//
// Either way the collector is Blind, and it is a FamilyCollector: the engine does not
// turn its failure into a whole-cycle failure, because a CPU metrics outage says
// nothing about a crash loop. It blinds CPUThrottling only: a CPUThrottling incident
// does not resolve and a CPU fix does not verify Healthy while Blind is true (B2b,
// scoped to the family), and nothing else is held back.
type CPUThrottlingCollector struct {
	pods       client.Reader
	logger     logr.Logger
	metrics    MetricsSource
	throttle   ThrottleSource
	namespaces NamespaceFilter
	sink       CPUThrottlingStatusSink

	metricsDisabled bool
	minSpacing      time.Duration
	now             func() time.Time
	logs            *LogThrottle

	mu             sync.Mutex
	counts         map[countKey]int
	everSawMetrics bool
	haveLast       bool
	lastRun        time.Time
	lastSignals    []Signal
	lastErr        error
	lastActive     string

	// view is what Blind reads. It has its own lock, so asking whether the collector is
	// blind never waits behind a Collect that is in the middle of a Prometheus query.
	viewMu     sync.RWMutex
	haveStatus bool
	lastStatus SourceStatus
	// warm is true when the latest cycle's silence can be trusted for resolution: the
	// source has produced as many consecutive good cycles as its signal needs.
	warm bool

	// warmCycles counts consecutive good metrics.k8s.io cycles (guarded by mu).
	warmCycles int
	warmSource string
	// noData is set by a cycle in which metrics.k8s.io answered but named none of the
	// containers that have a CPU limit (guarded by mu).
	noData bool

	// reads counts, per container, the consecutive metrics.k8s.io cycles that listed it,
	// hot or quiet. A container needs CPUUsageConsecutiveCycles of them before its silence
	// is evidence (guarded by mu).
	reads map[countKey]int
	// throttled remembers the containers that have raised a signal and are still in the
	// cluster. One that drops out of the source is unknown, not quiet (guarded by mu).
	throttled map[countKey]bool
	// pendingNow is what the latest good cycle found pending (guarded by mu); pending is
	// the copy ResolutionPending reads (guarded by viewMu).
	pendingNow []ContainerRef
	pending    []ContainerRef
}

type containerKey struct{ namespace, pod, container string }

func (k containerKey) ref() ContainerRef {
	return ContainerRef{Namespace: k.namespace, Pod: k.pod, Container: k.container}
}

// countKey is what a streak belongs to: one container of one pod instance. The UID
// makes a StatefulSet's recreated db-0 a new pod that starts from zero, where the
// name alone would let it inherit its predecessor's cycles.
type countKey struct {
	uid types.UID
	containerKey
}

var (
	_ FamilyCollector   = (*CPUThrottlingCollector)(nil)
	_ ResolutionBlinder = (*CPUThrottlingCollector)(nil)
	_ ResolutionPender  = (*CPUThrottlingCollector)(nil)
)

// CPUThrottlingOption configures a CPUThrottlingCollector.
type CPUThrottlingOption func(*CPUThrottlingCollector)

// WithMetricsSource sets the metrics.k8s.io source.
func WithMetricsSource(m MetricsSource) CPUThrottlingOption {
	return func(c *CPUThrottlingCollector) { c.metrics = m }
}

// WithThrottleSource sets the optional Prometheus source.
func WithThrottleSource(s ThrottleSource) CPUThrottlingOption {
	return func(c *CPUThrottlingCollector) { c.throttle = s }
}

// WithCPUNamespaceFilter makes the collector skip the namespaces the gate would
// refuse, as the pod collector does.
func WithCPUNamespaceFilter(f NamespaceFilter) CPUThrottlingOption {
	return func(c *CPUThrottlingCollector) {
		if f != nil {
			c.namespaces = f
		}
	}
}

// WithCPUStatusSink sets where the collector reports its source state.
func WithCPUStatusSink(s CPUThrottlingStatusSink) CPUThrottlingOption {
	return func(c *CPUThrottlingCollector) { c.sink = s }
}

// WithMetricsServerDisabled turns the metrics.k8s.io source off (the operator's
// --enable-metrics-server=false). The state is reported as such, not as absent.
func WithMetricsServerDisabled() CPUThrottlingOption {
	return func(c *CPUThrottlingCollector) { c.metricsDisabled = true }
}

// WithMinCycleSpacing sets how close two Collect calls may be before the second is
// treated as the same cycle. Zero makes every call a cycle.
func WithMinCycleSpacing(d time.Duration) CPUThrottlingOption {
	return func(c *CPUThrottlingCollector) { c.minSpacing = max(d, 0) }
}

// WithCPUClock replaces the clock, for tests.
func WithCPUClock(now func() time.Time) CPUThrottlingOption {
	return func(c *CPUThrottlingCollector) {
		if now != nil {
			c.now = now
		}
	}
}

// NewCPUThrottlingCollector builds the collector. pods is read for the container
// limits and owners; with no source set, it reports detection off.
func NewCPUThrottlingCollector(pods client.Reader, logger logr.Logger, opts ...CPUThrottlingOption) *CPUThrottlingCollector {
	c := &CPUThrottlingCollector{
		pods:       pods,
		logger:     logger.WithName(collectorNameCPUThrottling),
		namespaces: kubeSystemOnly{},
		minSpacing: DefaultMinCycleSpacing,
		now:        time.Now,
		logs:       NewLogThrottle(LogRepeatInterval),
		counts:     map[countKey]int{},
		reads:      map[countKey]int{},
		throttled:  map[countKey]bool{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Name implements SignalCollector.
func (c *CPUThrottlingCollector) Name() string { return collectorNameCPUThrottling }

// Families implements FamilyCollector: this collector is the only observer of CPU
// throttling.
func (c *CPUThrottlingCollector) Families() []Family { return []Family{FamilyCPUThrottling} }

// Blind implements FamilyCollector. It is true until a cycle has read a source, and
// whenever the latest cycle could not (no source, source lost, read failed). A call
// answered from the cache inside the spacing window keeps the status of the cycle
// that produced it.
func (c *CPUThrottlingCollector) Blind() bool {
	c.viewMu.RLock()
	defer c.viewMu.RUnlock()
	return !c.haveStatus || !c.lastStatus.Active
}

// ResolutionBlind implements ResolutionBlinder. It is Blind, and also true while a
// metrics.k8s.io source has not yet produced CPUUsageConsecutiveCycles consecutive good
// cycles: until then a container pinned at its limit reads as quiet, because the streak
// that would say otherwise has not had time to build. A cycle that read metrics.k8s.io
// and found none of the containers that have a CPU limit is no data and does not count
// (a metrics-server that just restarted answers with an empty list). Prometheus needs no
// warm-up: its ratio is a five-minute rate already.
//
// The verifier asks Blind, not this: it cannot afford three Unknown cycles in a row.
func (c *CPUThrottlingCollector) ResolutionBlind() bool {
	c.viewMu.RLock()
	defer c.viewMu.RUnlock()
	return !c.haveStatus || !c.lastStatus.Active || !c.warm
}

// ResolutionPending implements ResolutionPender: the containers that read at or above the
// usage line in the latest cycle without yet having the CPUUsageConsecutiveCycles that
// raise a signal, and the containers that did raise one and have not been read for that
// many cycles since (they left the source, or a cycle could not look). A pod pinned at its
// limit that is new to a source that is warm as a whole is the case it exists for: the
// source's warm-up says it can see, and this says it has not yet seen this container.
// Containers of pods that are gone, finished or terminating are not listed. The result is
// sorted and the caller's to keep.
func (c *CPUThrottlingCollector) ResolutionPending() []ContainerRef {
	c.viewMu.RLock()
	defer c.viewMu.RUnlock()
	return slices.Clone(c.pending)
}

// advanceWarmUp folds one finished cycle into the warm-up count and reports whether the
// source is warm. Called with c.mu held.
func (c *CPUThrottlingCollector) advanceWarmUp(status SourceStatus, err error) bool {
	switch {
	case err != nil || !status.Active || c.noData:
		c.warmCycles, c.warmSource = 0, ""
		return false
	case status.Source == CPUThrottlingSourcePrometheus:
		c.warmCycles, c.warmSource = 0, status.Source
		return true
	}
	if c.warmSource != status.Source {
		c.warmCycles = 0 // a source switch: the usage streaks started over with it
	}
	c.warmSource = status.Source
	c.warmCycles++
	return c.warmCycles >= CPUUsageConsecutiveCycles
}

// Collect implements SignalCollector.
func (c *CPUThrottlingCollector) Collect(ctx context.Context) ([]Signal, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.haveLast && c.minSpacing > 0 && now.Sub(c.lastRun) < c.minSpacing {
		return slices.Clone(c.lastSignals), c.lastErr
	}

	c.noData = false
	c.pendingNow = nil
	signals, status, err := c.observe(ctx, now)
	c.haveLast, c.lastRun, c.lastSignals, c.lastErr = true, now, slices.Clone(signals), err
	warm := c.advanceWarmUp(status, err)
	if err != nil {
		// A cycle that could not look breaks every streak: three consecutive cycles
		// means three that all read the metrics.
		c.resetStreaks()
	}
	pending := slices.Clone(c.pendingNow)
	c.viewMu.Lock()
	c.haveStatus, c.lastStatus, c.warm, c.pending = true, status, warm, pending
	c.viewMu.Unlock()

	if c.sink != nil {
		c.sink.ReportCPUThrottlingSource(ctx, status)
	}
	c.logStatus(status, err, now)
	return signals, err
}

// resetStreaks starts every usage streak and reading streak over. What a throttled
// container is remembered for survives: not having looked is not having seen it quiet.
func (c *CPUThrottlingCollector) resetStreaks() {
	c.counts = map[countKey]int{}
	c.reads = map[countKey]int{}
}

// errPrometheusNoData says the Prometheus endpoint answered, with nothing measured about
// any container in this cluster that has a CPU limit: no series for one, or only 0/0.
var errPrometheusNoData = errors.New("prometheus returned no series for any container with a CPU limit")

// observe runs one cycle. Prometheus, when it answers with data, decides; otherwise
// metrics.k8s.io.
func (c *CPUThrottlingCollector) observe(ctx context.Context, now time.Time) ([]Signal, SourceStatus, error) {
	var promErr error
	var samples []ThrottleSample
	var pods podIndex
	promUsed := false
	if c.throttle != nil {
		got, err := c.throttle.Query(ctx)
		switch {
		case errors.Is(err, ErrPrometheusNotConfigured):
			// Not set: not a fault.
		case err != nil:
			promErr = err
		default:
			idx, perr := c.podIndex(ctx)
			if perr != nil {
				return nil, readFailed(), perr
			}
			pods = idx
			// The query answers for every container, including quiet ones, so an answer
			// that names none of the containers that could be throttled means the endpoint
			// has no cAdvisor data (not scraped, relabelled, an outage). It is not a source
			// that saw "nothing throttled". Neither is one whose every known container is
			// 0/0: the CFS counters exist and never advance. A mix, with at least one
			// container that measured a ratio, is a cluster that is quiet.
			if len(pods) > 0 && !measuresAKnownContainer(got, pods) {
				promErr = errPrometheusNoData
			} else {
				samples, promUsed = got, true
			}
		}
	}

	if promUsed {
		return c.fromPrometheus(ctx, samples, pods, now)
	}
	return c.fromMetricsServer(ctx, promErr, pods, now)
}

// measuresAKnownContainer reports whether any sample is about a container in the index and
// carries a measured ratio (not 0/0).
func measuresAKnownContainer(samples []ThrottleSample, pods podIndex) bool {
	for _, s := range samples {
		if s.NoPeriods {
			continue
		}
		if _, _, ok := pods.lookup(containerKey{s.Namespace, s.Pod, s.Container}); ok {
			return true
		}
	}
	return false
}

// metricsNameAKnownContainer reports whether any metrics item is about a container in the index.
func metricsNameAKnownContainer(items []metricsv1beta1.PodMetrics, pods podIndex) bool {
	for i := range items {
		for _, cm := range items[i].Containers {
			if _, _, ok := pods.lookup(containerKey{items[i].Namespace, items[i].Name, cm.Name}); ok {
				return true
			}
		}
	}
	return false
}

// promFault is how a Prometheus failure is announced: the reason, and the fixed
// sentences for "using metrics.k8s.io instead" and "nothing to use instead".
type promFault struct {
	reason, fallback, down string
}

func classifyPromFault(err error) promFault {
	switch {
	case errors.Is(err, errPrometheusNoData):
		return promFault{ReasonPrometheusNoData, msgFallbackNoData, msgPrometheusNoData}
	case errors.Is(err, ErrPrometheusURLInvalid):
		return promFault{ReasonPrometheusURLInvalid, msgFallbackURLInvalid, msgPrometheusURLInvalid}
	default:
		return promFault{ReasonPrometheusUnreachable, msgFallback, msgPrometheusDown}
	}
}

func (f promFault) downStatus() SourceStatus {
	return SourceStatus{Source: CPUThrottlingSourceNone, Reason: f.reason, Message: f.down}
}

func (c *CPUThrottlingCollector) fromPrometheus(
	ctx context.Context, samples []ThrottleSample, pods podIndex, now time.Time,
) ([]Signal, SourceStatus, error) {
	status := SourceStatus{Active: true, Source: CPUThrottlingSourcePrometheus, Reason: ReasonPrometheusActive, Message: msgPrometheusActive}
	// The usage counts belong to metrics.k8s.io; switching source starts them over.
	// Prometheus measures throttling directly, so nothing is remembered or pending.
	c.resetStreaks()
	c.throttled = map[countKey]bool{}

	seen := map[containerKey]bool{}
	signals := make([]Signal, 0, len(samples))
	for _, s := range samples {
		// Checked again here, not only in the source: whatever implements ThrottleSource,
		// a ratio outside [0, 1] (or NaN, which fails both comparisons) is not a ratio.
		if !(s.Ratio >= PrometheusThrottleRatio && s.Ratio <= 1) {
			continue
		}
		key := containerKey{s.Namespace, s.Pod, s.Container}
		if seen[key] {
			continue
		}
		// The series is a claim by a remote server. It becomes a signal only if the
		// pod is in the cluster, the container is in the pod and has a CPU limit.
		pod, limit, ok := pods.lookup(key)
		if !ok {
			continue
		}
		excluded, err := c.namespaces.NamespaceExcluded(ctx, key.namespace)
		if err != nil {
			return nil, readFailed(), fmt.Errorf("checking namespace %q against the deny list: %w", key.namespace, err)
		}
		if excluded {
			continue
		}
		seen[key] = true
		severity := SeverityWarning
		if s.Ratio >= PrometheusCriticalRatio {
			severity = SeverityCritical
		}
		pct, threshold := s.Ratio*100, PrometheusThrottleRatio*100
		meta := containerMeta(pod, key.container, CPUThrottlingSourcePrometheus, limit)
		meta["throttledRatio"] = fmt.Sprintf("%.2f", s.Ratio)
		signals = append(signals, Signal{
			Type: SignalCPUThrottling, Severity: severity, Category: CategoryResource,
			Source:     collectorNameCPUThrottling,
			Message:    fmt.Sprintf("Container %s in %s/%s was CPU throttled in %.0f%% of its scheduling periods", key.container, key.namespace, key.pod, pct),
			Resource:   podResource(*pod),
			Value:      &pct,
			Threshold:  &threshold,
			DetectedAt: now,
			Metadata:   meta,
		})
	}
	return signals, status, nil
}

func (c *CPUThrottlingCollector) fromMetricsServer(
	ctx context.Context, promErr error, pods podIndex, now time.Time,
) ([]Signal, SourceStatus, error) {
	if c.metrics == nil || c.metricsDisabled {
		return c.noMetricsServer(promErr)
	}

	present, err := c.metrics.Available(ctx)
	if err != nil {
		return nil, readFailed(), fmt.Errorf("probing metrics.k8s.io: %w", err)
	}
	if !present {
		c.resetStreaks()
		switch {
		case c.everSawMetrics:
			return nil, SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonMetricsSourceLost, Message: msgSourceLost},
				errors.New("metrics.k8s.io was served and is not any more")
		case promErr != nil:
			return nil, classifyPromFault(promErr).downStatus(), fmt.Errorf("prometheus unusable and metrics.k8s.io is not served: %w", promErr)
		}
		return nil, SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonNoMetricsSource, Message: msgNoSource}, nil
	}
	c.everSawMetrics = true

	items, err := c.metrics.PodMetrics(ctx)
	if err != nil {
		return nil, readFailed(), fmt.Errorf("reading pod metrics: %w", err)
	}
	if pods == nil {
		if pods, err = c.podIndex(ctx); err != nil {
			return nil, readFailed(), err
		}
	}

	// An answer that names none of the containers that could be throttled is "no data
	// yet", not "all quiet": it does not count toward the resolution warm-up.
	c.noData = len(pods) > 0 && !metricsNameAKnownContainer(items, pods)

	signals, err := c.usageSignals(ctx, items, pods, now)
	if err != nil {
		return nil, readFailed(), err
	}

	status := SourceStatus{Active: true, Source: CPUThrottlingSourceMetricsServer, Reason: ReasonMetricsServerActive, Message: msgMetricsServerActive}
	if promErr != nil {
		fault := classifyPromFault(promErr)
		c.logFallback(promErr, fault, now)
		status.Reason, status.Message = fault.reason, fault.fallback
	}
	return signals, status, nil
}

// logFallback says, at the default log level and once per LogRepeatInterval, why the
// configured Prometheus is not being used. Without it a response over the size limit
// or an empty answer looks like "detection is on" while the true signal never arrives.
func (c *CPUThrottlingCollector) logFallback(err error, fault promFault, now time.Time) {
	ok, held := c.logs.Allow("prometheus-fallback/"+fault.reason, now)
	if !ok {
		return
	}
	kv := []any{"reason", fault.reason, "error", err.Error()}
	if held > 0 {
		kv = append(kv, "cyclesSinceLastLog", held)
	}
	c.logger.Info("Prometheus is not being used for CPU throttling detection; reading metrics.k8s.io instead", kv...)
}

// noMetricsServer is the case where metrics.k8s.io is not an option at all.
func (c *CPUThrottlingCollector) noMetricsServer(promErr error) ([]Signal, SourceStatus, error) {
	c.resetStreaks()
	if promErr != nil {
		return nil, classifyPromFault(promErr).downStatus(), fmt.Errorf("prometheus unusable and metrics.k8s.io is not in use: %w", promErr)
	}
	if c.metricsDisabled {
		return nil, SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonMetricsServerDisabled, Message: msgServerDisabled}, nil
	}
	return nil, SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonNoMetricsSource, Message: msgNoSource}, nil
}

func readFailed() SourceStatus {
	return SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonMetricsReadFailed, Message: msgReadFailed}
}

// usageSignals applies the default threshold: usage at or above 95% of the limit for
// CPUUsageConsecutiveCycles cycles in a row. A container that dips, or is missing from a
// cycle, starts again from zero.
func (c *CPUThrottlingCollector) usageSignals(
	ctx context.Context, items []metricsv1beta1.PodMetrics, pods podIndex, now time.Time,
) ([]Signal, error) {
	next := map[countKey]int{}
	reads := map[countKey]int{}
	hot := map[countKey]bool{} // at the line, streak still under CPUUsageConsecutiveCycles
	var signals []Signal
	excluded := map[string]bool{}

	for i := range items {
		pm := &items[i]
		for _, cm := range pm.Containers {
			key := containerKey{pm.Namespace, pm.Name, cm.Name}
			pod, limit, ok := pods.lookup(key)
			if !ok {
				continue // no such pod, or no CPU limit to be throttled by
			}
			ck := countKey{uid: pod.UID, containerKey: key}
			reads[ck] = c.reads[ck] + 1
			usage := cm.Usage.Cpu().MilliValue()
			if usage*100 < limit.MilliValue()*cpuUsageLimitPercent {
				continue
			}
			skip, seen := excluded[pm.Namespace]
			if !seen {
				var err error
				if skip, err = c.namespaces.NamespaceExcluded(ctx, pm.Namespace); err != nil {
					return nil, fmt.Errorf("checking namespace %q against the deny list: %w", pm.Namespace, err)
				}
				excluded[pm.Namespace] = skip
			}
			if skip {
				continue
			}

			cycles := c.counts[ck] + 1
			next[ck] = cycles
			if cycles < CPUUsageConsecutiveCycles {
				hot[ck] = true
				continue
			}
			pct := float64(usage) / float64(limit.MilliValue()) * 100
			threshold := CPUUsageLimitRatio * 100
			meta := containerMeta(pod, cm.Name, CPUThrottlingSourceMetricsServer, limit)
			meta["cpuUsage"] = cm.Usage.Cpu().String()
			meta["consecutiveCycles"] = fmt.Sprintf("%d", cycles)
			signals = append(signals, Signal{
				Type: SignalCPUThrottling, Severity: SeverityWarning, Category: CategoryResource,
				Source: collectorNameCPUThrottling,
				Message: fmt.Sprintf("Container %s in %s/%s has used %.0f%% of its CPU limit for %d cycles, so it is being throttled",
					cm.Name, pm.Namespace, pm.Name, pct, cycles),
				Resource:   podResource(*pod),
				Value:      &pct,
				Threshold:  &threshold,
				DetectedAt: now,
				Metadata:   meta,
			})
		}
	}
	c.counts = next
	c.reads = reads
	c.settlePending(pods, next, reads, hot)
	return signals, nil
}

// settlePending updates what is remembered as throttled and what is pending after a good
// metrics.k8s.io cycle. A container is pending when it is at the usage line without a
// signal yet, or when it raised a signal and has not been read CPUUsageConsecutiveCycles
// cycles in a row since. A remembered container is forgotten when it has been read that
// long (and is not hot: a hot one keeps its signal), or when its pod is gone, finished or
// terminating.
func (c *CPUThrottlingCollector) settlePending(pods podIndex, streaks, reads map[countKey]int, hot map[countKey]bool) {
	pending := map[ContainerRef]bool{}
	remembered := map[countKey]bool{}
	for ck := range hot {
		pending[ck.ref()] = true
	}
	for ck := range c.throttled {
		if !stillRunning(pods, ck) {
			continue
		}
		if reads[ck] >= CPUUsageConsecutiveCycles && streaks[ck] < CPUUsageConsecutiveCycles {
			continue // read long enough, and quiet: its silence is evidence again
		}
		remembered[ck] = true
		if reads[ck] < CPUUsageConsecutiveCycles {
			pending[ck.ref()] = true
		}
	}
	for ck, n := range streaks {
		if n >= CPUUsageConsecutiveCycles {
			remembered[ck] = true
		}
	}
	c.throttled = remembered

	c.pendingNow = make([]ContainerRef, 0, len(pending))
	for ref := range pending {
		c.pendingNow = append(c.pendingNow, ref)
	}
	sort.Slice(c.pendingNow, func(i, j int) bool {
		a, b := c.pendingNow[i], c.pendingNow[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Pod != b.Pod {
			return a.Pod < b.Pod
		}
		return a.Container < b.Container
	})
}

// stillRunning reports whether the pod instance a streak belongs to is still in the
// cluster and can still report usage.
func stillRunning(pods podIndex, ck countKey) bool {
	pod, _, ok := pods.lookup(ck.containerKey)
	if !ok || pod.UID != ck.uid || pod.DeletionTimestamp != nil {
		return false
	}
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// containerMeta is the metadata every CPUThrottling signal carries. "container" is
// part of the fault's identity.
func containerMeta(pod *corev1.Pod, container, source string, limit *resource.Quantity) map[string]string {
	meta := podMetadata(*pod)
	meta["container"] = container
	meta["source"] = source
	meta["cpuLimit"] = limit.String()
	return meta
}

// podIndex finds a pod's container and its CPU limit.
type podIndex map[containerKey]podContainer

type podContainer struct {
	pod   *corev1.Pod
	limit *resource.Quantity
}

func (p podIndex) lookup(key containerKey) (*corev1.Pod, *resource.Quantity, bool) {
	pc, ok := p[key]
	if !ok {
		return nil, nil, false
	}
	return pc.pod, pc.limit, true
}

// podIndex lists the pods once and indexes every container that has a CPU limit.
// A container with none cannot be throttled and is never indexed.
func (c *CPUThrottlingCollector) podIndex(ctx context.Context) (podIndex, error) {
	var list corev1.PodList
	if err := c.pods.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	idx := podIndex{}
	for i := range list.Items {
		pod := &list.Items[i]
		for _, ctr := range pod.Spec.Containers {
			limit := ctr.Resources.Limits.Cpu()
			if limit.MilliValue() <= 0 {
				continue
			}
			idx[containerKey{pod.Namespace, pod.Name, ctr.Name}] = podContainer{pod: pod, limit: limit}
		}
	}
	return idx, nil
}

// logStatus says what state detection is in without a line per cycle: an active
// source is announced when it changes, and any other state at most once per
// LogRepeatInterval.
func (c *CPUThrottlingCollector) logStatus(status SourceStatus, err error, now time.Time) {
	if status.Active {
		if c.lastActive != status.Reason {
			c.logger.Info("CPU throttling detection is on", "source", status.Source, "reason", status.Reason)
		}
		c.lastActive = status.Reason
		return
	}
	c.lastActive = ""

	ok, held := c.logs.Allow(status.Reason, now)
	if !ok {
		return
	}
	kv := []any{"reason", status.Reason}
	if held > 0 {
		kv = append(kv, "cyclesSinceLastLog", held)
	}
	if err != nil {
		c.logger.Error(err, "CPU throttling detection could not look: this cycle is incomplete", kv...)
		return
	}
	c.logger.Info(status.Message, kv...)
}
