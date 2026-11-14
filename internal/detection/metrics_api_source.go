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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// metricsAPIGroup is the API group metrics-server serves.
const metricsAPIGroup = "metrics.k8s.io"

// MetricsSource is metrics.k8s.io as the CPU throttling collector sees it. The
// collector depends on this, not on the generated clientset, so the threshold logic
// is tested with a fake and the adapter is tested on its own.
type MetricsSource interface {
	// Available reports whether the metrics API is served. A discovery failure is an
	// error, not "no": a source that cannot be asked about is not an absent one.
	Available(ctx context.Context) (bool, error)

	// PodMetrics lists the usage of every pod.
	PodMetrics(ctx context.Context) ([]metricsv1beta1.PodMetrics, error)
}

// metricsAPISource adapts the metrics-server clientset to MetricsSource. It needs
// only get/list on pods.metrics.k8s.io; nothing here reaches a kubelet.
type metricsAPISource struct {
	client metricsclient.Interface
}

// NewMetricsAPISource wraps a metrics-server clientset.
func NewMetricsAPISource(c metricsclient.Interface) MetricsSource {
	return &metricsAPISource{client: c}
}

func (m *metricsAPISource) Available(context.Context) (bool, error) {
	groups, err := m.client.Discovery().ServerGroups()
	if err != nil {
		return false, fmt.Errorf("discovering API groups: %w", err)
	}
	for _, group := range groups.Groups {
		if group.Name == metricsAPIGroup {
			return true, nil
		}
	}
	return false, nil
}

func (m *metricsAPISource) PodMetrics(ctx context.Context) ([]metricsv1beta1.PodMetrics, error) {
	list, err := m.client.MetricsV1beta1().PodMetricses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pod metrics: %w", err)
	}
	return list.Items, nil
}
