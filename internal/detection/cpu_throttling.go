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
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	DefaultMinCycleSpacing = 30 * time.Second
)

// Sources and reasons of CPU throttling detection (see SourceStatus).
const (
	CPUThrottlingSourceMetricsServer = dorguv1.CPUThrottlingSourceMetricsServer
	CPUThrottlingSourcePrometheus    = dorguv1.CPUThrottlingSourcePrometheus
	CPUThrottlingSourceNone          = dorguv1.CPUThrottlingSourceNone

	ReasonMetricsServerActive   = "MetricsServerActive"
	ReasonPrometheusActive      = "PrometheusActive"
	ReasonPrometheusUnreachable = "PrometheusUnreachable"
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
	msgNoSource            = "CPU throttling detection is off: metrics.k8s.io is not served and spec.metrics.prometheus.url is not set. " +
		"Install metrics-server or set spec.metrics.prometheus.url on the ClusterPersona."
	msgServerDisabled = "CPU throttling detection is off: metrics-server detection is disabled (--enable-metrics-server=false) and " +
		"spec.metrics.prometheus.url is not set. Enable it or set spec.metrics.prometheus.url on the ClusterPersona."
	msgPrometheusDown = "CPU throttling detection is off: the Prometheus endpoint in spec.metrics.prometheus.url could not be read and " +
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
// Collect returns no error, or every verification on a cluster without metrics-server
// would read "unknown". A source that exists and cannot be read, or one that was
// there and is gone, is a collector that could not look, and Collect returns the
// error so it never reads as healthy (B2b).
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
	logs            *logThrottle

	mu             sync.Mutex
	counts         map[containerKey]int
	everSawMetrics bool
	haveLast       bool
	lastRun        time.Time
	lastSignals    []Signal
	lastErr        error
	lastActive     string
}

type containerKey struct{ namespace, pod, container string }

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
		logs:       newLogThrottle(logRepeatInterval),
		counts:     map[containerKey]int{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Name implements SignalCollector.
func (c *CPUThrottlingCollector) Name() string { return collectorNameCPUThrottling }

// Collect implements SignalCollector.
func (c *CPUThrottlingCollector) Collect(ctx context.Context) ([]Signal, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.haveLast && c.minSpacing > 0 && now.Sub(c.lastRun) < c.minSpacing {
		return slices.Clone(c.lastSignals), c.lastErr
	}

	signals, status, err := c.observe(ctx, now)
	c.haveLast, c.lastRun, c.lastSignals, c.lastErr = true, now, slices.Clone(signals), err

	if c.sink != nil {
		c.sink.ReportCPUThrottlingSource(ctx, status)
	}
	c.logStatus(status, err, now)
	return signals, err
}

// observe runs one cycle. Prometheus, when it answers, decides; otherwise metrics.k8s.io.
func (c *CPUThrottlingCollector) observe(ctx context.Context, now time.Time) ([]Signal, SourceStatus, error) {
	var promErr error
	var samples []ThrottleSample
	promUsed := false
	if c.throttle != nil {
		got, err := c.throttle.Query(ctx)
		switch {
		case errors.Is(err, ErrPrometheusNotConfigured):
			// Not set: not a fault.
		case err != nil:
			promErr = err
		default:
			samples, promUsed = got, true
		}
	}

	if promUsed {
		return c.fromPrometheus(ctx, samples, now)
	}
	return c.fromMetricsServer(ctx, promErr, now)
}

func (c *CPUThrottlingCollector) fromPrometheus(ctx context.Context, samples []ThrottleSample, now time.Time) ([]Signal, SourceStatus, error) {
	status := SourceStatus{Active: true, Source: CPUThrottlingSourcePrometheus, Reason: ReasonPrometheusActive, Message: msgPrometheusActive}
	pods, err := c.podIndex(ctx)
	if err != nil {
		return nil, readFailed(), err
	}
	// The usage counts belong to metrics.k8s.io; switching source starts them over.
	c.counts = map[containerKey]int{}

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
		pct, threshold := s.Ratio*100, PrometheusThrottleRatio*100
		meta := containerMeta(pod, key.container, CPUThrottlingSourcePrometheus, limit)
		meta["throttledRatio"] = fmt.Sprintf("%.2f", s.Ratio)
		signals = append(signals, Signal{
			Type: SignalCPUThrottling, Severity: SeverityWarning, Category: CategoryResource,
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

func (c *CPUThrottlingCollector) fromMetricsServer(ctx context.Context, promErr error, now time.Time) ([]Signal, SourceStatus, error) {
	if c.metrics == nil || c.metricsDisabled {
		return c.noMetricsServer(promErr)
	}

	present, err := c.metrics.Available(ctx)
	if err != nil {
		return nil, readFailed(), fmt.Errorf("probing metrics.k8s.io: %w", err)
	}
	if !present {
		c.counts = map[containerKey]int{}
		switch {
		case c.everSawMetrics:
			return nil, SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonMetricsSourceLost, Message: msgSourceLost},
				errors.New("metrics.k8s.io was served and is not any more")
		case promErr != nil:
			return nil, prometheusDown(), fmt.Errorf("prometheus unreachable and metrics.k8s.io is not served: %w", promErr)
		}
		return nil, SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonNoMetricsSource, Message: msgNoSource}, nil
	}
	c.everSawMetrics = true

	items, err := c.metrics.PodMetrics(ctx)
	if err != nil {
		return nil, readFailed(), fmt.Errorf("reading pod metrics: %w", err)
	}
	pods, err := c.podIndex(ctx)
	if err != nil {
		return nil, readFailed(), err
	}

	signals, err := c.usageSignals(ctx, items, pods, now)
	if err != nil {
		return nil, readFailed(), err
	}

	status := SourceStatus{Active: true, Source: CPUThrottlingSourceMetricsServer, Reason: ReasonMetricsServerActive, Message: msgMetricsServerActive}
	if promErr != nil {
		c.logger.V(1).Info("prometheus could not be read; using metrics.k8s.io", "error", promErr.Error())
		status.Reason, status.Message = ReasonPrometheusUnreachable, msgFallback
	}
	return signals, status, nil
}

// noMetricsServer is the case where metrics.k8s.io is not an option at all.
func (c *CPUThrottlingCollector) noMetricsServer(promErr error) ([]Signal, SourceStatus, error) {
	c.counts = map[containerKey]int{}
	if promErr != nil {
		return nil, prometheusDown(), fmt.Errorf("prometheus unreachable and metrics.k8s.io is not in use: %w", promErr)
	}
	if c.metricsDisabled {
		return nil, SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonMetricsServerDisabled, Message: msgServerDisabled}, nil
	}
	return nil, SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonNoMetricsSource, Message: msgNoSource}, nil
}

func readFailed() SourceStatus {
	return SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonMetricsReadFailed, Message: msgReadFailed}
}

func prometheusDown() SourceStatus {
	return SourceStatus{Source: CPUThrottlingSourceNone, Reason: ReasonPrometheusUnreachable, Message: msgPrometheusDown}
}

// usageSignals applies the default threshold: usage at or above 95% of the limit for
// CPUUsageConsecutiveCycles cycles in a row. A container that dips, or is missing from a
// cycle, starts again from zero.
func (c *CPUThrottlingCollector) usageSignals(
	ctx context.Context, items []metricsv1beta1.PodMetrics, pods podIndex, now time.Time,
) ([]Signal, error) {
	next := map[containerKey]int{}
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

			cycles := c.counts[key] + 1
			next[key] = cycles
			if cycles < CPUUsageConsecutiveCycles {
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
	return signals, nil
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
// logRepeatInterval.
func (c *CPUThrottlingCollector) logStatus(status SourceStatus, err error, now time.Time) {
	if status.Active {
		if c.lastActive != status.Reason {
			c.logger.Info("CPU throttling detection is on", "source", status.Source, "reason", status.Reason)
		}
		c.lastActive = status.Reason
		return
	}
	c.lastActive = ""

	ok, held := c.logs.allow(status.Reason, now)
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
