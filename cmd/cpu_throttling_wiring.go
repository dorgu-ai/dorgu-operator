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

package main

import (
	"time"

	"github.com/go-logr/logr"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// newCPUThrottlingCollector wires the CPU throttling collector the way main() runs it:
// metrics.k8s.io as the default source (unless --enable-metrics-server=false), the
// Prometheus URL read from the ClusterPersona every cycle as the optional true signal,
// the gate's deny-list as the namespace filter, and the ClusterPersona status as the
// place it says whether it can see.
//
// mc may be nil (a metrics client that could not be built): the collector then reports
// the source as absent rather than not existing. filter may be nil, which leaves only
// kube-system skipped.
func newCPUThrottlingCollector(
	c client.Client,
	filter detection.NamespaceFilter,
	mc metricsclient.Interface,
	cfg operatorConfig,
	log logr.Logger,
) *detection.CPUThrottlingCollector {
	opts := []detection.CPUThrottlingOption{
		detection.WithThrottleSource(detection.NewPrometheusThrottleSource(controller.PrometheusURLFromClusterPersona(c))),
		detection.WithCPUNamespaceFilter(filter),
		detection.WithCPUStatusSink(controller.NewCPUThrottlingStatusSink(c, log)),
		detection.WithMinCycleSpacing(cpuCycleSpacing(cfg.healthCheckInterval)),
	}
	if mc != nil {
		opts = append(opts, detection.WithMetricsSource(detection.NewMetricsAPISource(mc)))
	}
	if !cfg.enableMetricsServer {
		opts = append(opts, detection.WithMetricsServerDisabled())
	}
	return detection.NewCPUThrottlingCollector(c, log, opts...)
}

// cpuCycleSpacing is half the health-check interval, so two Collect calls a cycle
// apart both count and a verifier's call in between does not.
func cpuCycleSpacing(interval time.Duration) time.Duration {
	if interval <= 0 {
		return detection.DefaultMinCycleSpacing
	}
	return interval / 2
}
