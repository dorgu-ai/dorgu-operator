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
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const nodeCollectorName = "node-health-checker"

// nodeConditionMapping maps a K8s node condition to the signal it produces.
type nodeConditionMapping struct {
	conditionType corev1.NodeConditionType
	// unhealthyStatus is the condition status that indicates a problem.
	unhealthyStatus corev1.ConditionStatus
	signalType      SignalType
	severity        Severity
	message         string
}

var nodeConditionMappings = []nodeConditionMapping{
	{
		conditionType:   corev1.NodeReady,
		unhealthyStatus: corev1.ConditionFalse,
		signalType:      SignalNodeNotReady,
		severity:        SeverityCritical,
		message:         "Node %s is not ready",
	},
	{
		conditionType:   corev1.NodeReady,
		unhealthyStatus: corev1.ConditionUnknown,
		signalType:      SignalNodeNotReady,
		severity:        SeverityWarning,
		message:         "Node %s ready status is unknown",
	},
	{
		conditionType:   corev1.NodeMemoryPressure,
		unhealthyStatus: corev1.ConditionTrue,
		signalType:      SignalNodeMemoryPressure,
		severity:        SeverityWarning,
		message:         "Node %s has memory pressure",
	},
	{
		conditionType:   corev1.NodeDiskPressure,
		unhealthyStatus: corev1.ConditionTrue,
		signalType:      SignalNodeDiskPressure,
		severity:        SeverityWarning,
		message:         "Node %s has disk pressure",
	},
	{
		conditionType:   corev1.NodePIDPressure,
		unhealthyStatus: corev1.ConditionTrue,
		signalType:      SignalNodePIDPressure,
		severity:        SeverityWarning,
		message:         "Node %s has PID pressure",
	},
	{
		conditionType:   corev1.NodeNetworkUnavailable,
		unhealthyStatus: corev1.ConditionTrue,
		signalType:      SignalNodeNetworkDown,
		severity:        SeverityCritical,
		message:         "Node %s network is unavailable",
	},
}

// NodeCollector detects node health issues by checking node conditions.
type NodeCollector struct {
	client client.Reader
	logger logr.Logger
}

// NewNodeCollector creates a collector that checks node conditions.
func NewNodeCollector(c client.Reader, logger logr.Logger) *NodeCollector {
	return &NodeCollector{
		client: c,
		logger: logger.WithName(nodeCollectorName),
	}
}

func (n *NodeCollector) Name() string { return nodeCollectorName }

func (n *NodeCollector) Collect(ctx context.Context) ([]Signal, error) {
	nodeList := &corev1.NodeList{}
	if err := n.client.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	now := time.Now()
	var signals []Signal

	for _, node := range nodeList.Items {
		for _, mapping := range nodeConditionMappings {
			if signal, ok := n.checkCondition(node, mapping, now); ok {
				signals = append(signals, signal)
			}
		}
	}

	return signals, nil
}

// checkCondition checks a single node condition against its mapping.
func (n *NodeCollector) checkCondition(node corev1.Node, m nodeConditionMapping, now time.Time) (Signal, bool) {
	for _, cond := range node.Status.Conditions {
		if cond.Type != m.conditionType {
			continue
		}
		if cond.Status != m.unhealthyStatus {
			return Signal{}, false
		}

		detectedAt := now
		if !cond.LastTransitionTime.IsZero() {
			detectedAt = cond.LastTransitionTime.Time
		}

		return Signal{
			Type:     m.signalType,
			Severity: m.severity,
			Category: CategoryNode,
			Source:   nodeCollectorName,
			Message:  fmt.Sprintf(m.message, node.Name),
			Resource: dorguv1.ResourceReference{
				Kind: "Node",
				Name: node.Name,
			},
			DetectedAt: detectedAt,
			Metadata:   nodeMetadata(node),
		}, true
	}
	return Signal{}, false
}

// nodeMetadata extracts useful context from a node's labels.
func nodeMetadata(node corev1.Node) map[string]string {
	meta := map[string]string{
		"role": nodeRole(node),
	}
	if zone, ok := node.Labels["topology.kubernetes.io/zone"]; ok {
		meta["zone"] = zone
	}
	if region, ok := node.Labels["topology.kubernetes.io/region"]; ok {
		meta["region"] = region
	}
	return meta
}

// nodeRole determines the role of a node from its labels.
func nodeRole(node corev1.Node) string {
	for key := range node.Labels {
		if role, ok := strings.CutPrefix(key, "node-role.kubernetes.io/"); ok {
			return role
		}
	}
	return "worker"
}
