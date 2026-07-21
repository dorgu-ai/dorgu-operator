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
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	resourceCollectorName = "resource-saturation-checker"

	defaultWarnPct = 0.85
	defaultCritPct = 0.95
)

// ResourceCollector detects resource saturation at the node level by comparing
// total pod requests against node allocatable resources.
type ResourceCollector struct {
	client  client.Reader
	logger  logr.Logger
	warnPct float64
	critPct float64
}

// NewResourceCollector creates a collector that checks resource saturation.
func NewResourceCollector(c client.Reader, logger logr.Logger) *ResourceCollector {
	return &ResourceCollector{
		client:  c,
		logger:  logger.WithName(resourceCollectorName),
		warnPct: defaultWarnPct,
		critPct: defaultCritPct,
	}
}

func (r *ResourceCollector) Name() string { return resourceCollectorName }

func (r *ResourceCollector) Collect(ctx context.Context) ([]Signal, error) {
	nodeList := &corev1.NodeList{}
	if err := r.client.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	now := time.Now()
	var signals []Signal

	for _, node := range nodeList.Items {
		nodeSignals, err := r.checkNode(ctx, node, now)
		if err != nil {
			r.logger.Error(err, "failed to check node resource saturation", "node", node.Name)
			continue
		}
		signals = append(signals, nodeSignals...)
	}

	return signals, nil
}

// checkNode calculates resource saturation for a single node.
func (r *ResourceCollector) checkNode(ctx context.Context, node corev1.Node, now time.Time) ([]Signal, error) {
	// Get all non-terminated pods on this node
	podList := &corev1.PodList{}
	if err := r.client.List(ctx, podList, &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(PodNodeNameIndex, node.Name),
	}); err != nil {
		return nil, fmt.Errorf("failed to list pods on node %s: %w", node.Name, err)
	}

	// Sum CPU and memory requests across all non-terminated pods
	var totalCPUMillis, totalMemoryBytes int64
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				totalCPUMillis += cpuReq.MilliValue()
			}
			if memReq, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				totalMemoryBytes += memReq.Value()
			}
		}
		for _, c := range pod.Spec.InitContainers {
			if cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				totalCPUMillis += cpuReq.MilliValue()
			}
			if memReq, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				totalMemoryBytes += memReq.Value()
			}
		}
	}

	allocatableCPU := node.Status.Allocatable.Cpu().MilliValue()
	allocatableMem := node.Status.Allocatable.Memory().Value()

	var signals []Signal

	// Check CPU saturation
	if allocatableCPU > 0 {
		cpuRatio := float64(totalCPUMillis) / float64(allocatableCPU)
		if signal, ok := r.saturationSignal(node, "CPU", cpuRatio, SignalCPUSaturationCritical, SignalCPUSaturationHigh, now); ok {
			signals = append(signals, signal)
		}
	}

	// Check memory saturation
	if allocatableMem > 0 {
		memRatio := float64(totalMemoryBytes) / float64(allocatableMem)
		if signal, ok := r.saturationSignal(node, "memory", memRatio, SignalMemorySaturationCrit, SignalMemorySaturationHigh, now); ok {
			signals = append(signals, signal)
		}
	}

	return signals, nil
}

// saturationSignal creates a signal if the ratio exceeds warning or critical thresholds.
func (r *ResourceCollector) saturationSignal(
	node corev1.Node,
	resourceName string,
	ratio float64,
	critType, warnType SignalType,
	now time.Time,
) (Signal, bool) {
	pct := ratio * 100

	var signalType SignalType
	var severity Severity
	var threshold float64

	switch {
	case ratio >= r.critPct:
		signalType = critType
		severity = SeverityCritical
		threshold = r.critPct * 100
	case ratio >= r.warnPct:
		signalType = warnType
		severity = SeverityWarning
		threshold = r.warnPct * 100
	default:
		return Signal{}, false
	}

	return Signal{
		Type:     signalType,
		Severity: severity,
		Category: CategoryResource,
		Source:   resourceCollectorName,
		Message:  fmt.Sprintf("Node %s %s request saturation at %.1f%%", node.Name, resourceName, pct),
		Resource: dorguv1.ResourceReference{
			Kind: "Node",
			Name: node.Name,
		},
		Value:      &pct,
		Threshold:  &threshold,
		DetectedAt: now,
		Metadata: map[string]string{
			"resourceType": resourceName,
			"node":         node.Name,
		},
	}, true
}
