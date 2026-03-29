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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	metricsCollectorName  = "metrics-usage-checker"
	defaultUsageThreshold = 0.90
)

// MetricsCollector detects high actual CPU/memory usage by comparing
// metrics-server data against container resource limits.
type MetricsCollector struct {
	metricsClient  metricsclient.Interface
	client         client.Reader
	logger         logr.Logger
	available      bool
	usageThreshold float64
}

// NewMetricsCollector creates a collector that checks actual resource usage
// via metrics-server. It probes for availability on construction.
func NewMetricsCollector(metricsClient metricsclient.Interface, c client.Reader, logger logr.Logger) *MetricsCollector {
	mc := &MetricsCollector{
		metricsClient:  metricsClient,
		client:         c,
		logger:         logger.WithName(metricsCollectorName),
		usageThreshold: defaultUsageThreshold,
	}

	// Probe metrics-server availability
	mc.available = mc.probeAvailability()
	if !mc.available {
		mc.logger.Info("metrics-server not available, usage-based detection disabled")
	}

	return mc
}

func (m *MetricsCollector) Name() string { return metricsCollectorName }

// Collect returns usage signals if metrics-server is available.
// Returns an empty slice (not an error) when metrics-server is unavailable.
func (m *MetricsCollector) Collect(ctx context.Context) ([]Signal, error) {
	if !m.available {
		return nil, nil
	}

	// Re-probe on each collection in case metrics-server went away or came back
	if !m.probeAvailability() {
		m.available = false
		m.logger.V(1).Info("metrics-server became unavailable")
		return nil, nil
	}

	podMetricsList, err := m.metricsClient.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		m.logger.Error(err, "failed to list pod metrics")
		return nil, nil // graceful degradation
	}

	now := time.Now()

	// Build a map of pod specs for limit lookup
	podSpecs, err := m.getPodSpecs(ctx)
	if err != nil {
		return nil, nil // graceful degradation
	}

	var signals []Signal
	for _, pm := range podMetricsList.Items {
		signals = append(signals, m.checkPodMetrics(pm, podSpecs, now)...)
	}

	return signals, nil
}

// probeAvailability checks if metrics-server API is reachable.
func (m *MetricsCollector) probeAvailability() bool {
	_, err := m.metricsClient.Discovery().ServerGroups()
	if err != nil {
		return false
	}

	// Check if metrics.k8s.io API group exists
	groups, err := m.metricsClient.Discovery().ServerGroups()
	if err != nil {
		return false
	}
	for _, group := range groups.Groups {
		if group.Name == "metrics.k8s.io" {
			return true
		}
	}
	return false
}

// podSpecKey creates a lookup key for a pod.
type podSpecKey struct {
	namespace string
	name      string
}

// getPodSpecs builds a map of pod namespace/name to container limits.
func (m *MetricsCollector) getPodSpecs(ctx context.Context) (map[podSpecKey]corev1.PodSpec, error) {
	podList := &corev1.PodList{}
	if err := m.client.List(ctx, podList); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	specs := make(map[podSpecKey]corev1.PodSpec, len(podList.Items))
	for _, pod := range podList.Items {
		specs[podSpecKey{namespace: pod.Namespace, name: pod.Name}] = pod.Spec
	}
	return specs, nil
}

// checkPodMetrics compares actual usage against limits for each container.
func (m *MetricsCollector) checkPodMetrics(
	pm metricsv1beta1.PodMetrics,
	podSpecs map[podSpecKey]corev1.PodSpec,
	now time.Time,
) []Signal {
	spec, ok := podSpecs[podSpecKey{namespace: pm.Namespace, name: pm.Name}]
	if !ok {
		return nil
	}

	// Build container limit map
	containerLimits := make(map[string]corev1.ResourceList)
	for _, c := range spec.Containers {
		containerLimits[c.Name] = c.Resources.Limits
	}

	var signals []Signal

	for _, cm := range pm.Containers {
		limits, ok := containerLimits[cm.Name]
		if !ok {
			continue
		}

		// Check CPU usage vs limit
		if cpuLimit, ok := limits[corev1.ResourceCPU]; ok && cpuLimit.MilliValue() > 0 {
			cpuUsage := cm.Usage.Cpu().MilliValue()
			ratio := float64(cpuUsage) / float64(cpuLimit.MilliValue())
			if ratio >= m.usageThreshold {
				pct := ratio * 100
				threshold := m.usageThreshold * 100
				signals = append(signals, Signal{
					Type:     SignalCPUUsageHigh,
					Severity: SeverityWarning,
					Category: CategoryResource,
					Source:   metricsCollectorName,
					Message:  fmt.Sprintf("Container %s in %s/%s CPU usage at %.1f%% of limit", cm.Name, pm.Namespace, pm.Name, pct),
					Resource: dorguv1.ResourceReference{
						Kind:      "Pod",
						Name:      pm.Name,
						Namespace: pm.Namespace,
					},
					Value:      &pct,
					Threshold:  &threshold,
					DetectedAt: now,
					Metadata: map[string]string{
						"container":  cm.Name,
						"cpuUsage":   cm.Usage.Cpu().String(),
						"cpuLimit":   cpuLimit.String(),
						"deployment": ownerDeploymentFromSpec(spec),
					},
				})
			}
		}

		// Check memory usage vs limit
		if memLimit, ok := limits[corev1.ResourceMemory]; ok && memLimit.Value() > 0 {
			memUsage := cm.Usage.Memory().Value()
			ratio := float64(memUsage) / float64(memLimit.Value())
			if ratio >= m.usageThreshold {
				pct := ratio * 100
				threshold := m.usageThreshold * 100
				signals = append(signals, Signal{
					Type:     SignalMemoryUsageHigh,
					Severity: SeverityWarning,
					Category: CategoryResource,
					Source:   metricsCollectorName,
					Message:  fmt.Sprintf("Container %s in %s/%s memory usage at %.1f%% of limit", cm.Name, pm.Namespace, pm.Name, pct),
					Resource: dorguv1.ResourceReference{
						Kind:      "Pod",
						Name:      pm.Name,
						Namespace: pm.Namespace,
					},
					Value:      &pct,
					Threshold:  &threshold,
					DetectedAt: now,
					Metadata: map[string]string{
						"container":   cm.Name,
						"memoryUsage": cm.Usage.Memory().String(),
						"memoryLimit": memLimit.String(),
						"deployment":  ownerDeploymentFromSpec(spec),
					},
				})
			}
		}
	}

	return signals
}

// ownerDeploymentFromSpec is a best-effort extraction of deployment name from pod spec.
// Since PodMetrics doesn't have owner references, we attempt to get it from the spec's
// service account name or hostname (which often matches the deployment).
func ownerDeploymentFromSpec(spec corev1.PodSpec) string {
	if spec.Hostname != "" {
		return spec.Hostname
	}
	return ""
}
