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

const (
	podCollectorName = "pod-failure-detector"

	defaultPendingTimeout         = 5 * time.Minute
	defaultRestartThreshold int32 = 5
)

// PodCollector detects pod-level failures across the cluster.
type PodCollector struct {
	client           client.Reader
	logger           logr.Logger
	pendingTimeout   time.Duration
	restartThreshold int32
	excludeNS        map[string]bool
}

// NewPodCollector creates a collector that detects pod failures.
func NewPodCollector(c client.Reader, logger logr.Logger) *PodCollector {
	return &PodCollector{
		client:           c,
		logger:           logger.WithName(podCollectorName),
		pendingTimeout:   defaultPendingTimeout,
		restartThreshold: defaultRestartThreshold,
		excludeNS: map[string]bool{
			"kube-system": true,
		},
	}
}

func (p *PodCollector) Name() string { return podCollectorName }

func (p *PodCollector) Collect(ctx context.Context) ([]Signal, error) {
	podList := &corev1.PodList{}
	if err := p.client.List(ctx, podList); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	now := time.Now()
	var signals []Signal

	for _, pod := range podList.Items {
		if p.excludeNS[pod.Namespace] {
			continue
		}

		signals = append(signals, p.checkPod(pod, now)...)
	}

	return signals, nil
}

// checkPod examines a single pod for all failure modes.
func (p *PodCollector) checkPod(pod corev1.Pod, now time.Time) []Signal {
	var signals []Signal

	// Check eviction
	if signal, ok := p.checkEviction(pod, now); ok {
		signals = append(signals, signal)
	}

	// Check long-pending
	if signal, ok := p.checkPendingLong(pod, now); ok {
		signals = append(signals, signal)
	}

	// Check container statuses
	for _, cs := range pod.Status.ContainerStatuses {
		signals = append(signals, p.checkContainer(pod, cs, now)...)
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		signals = append(signals, p.checkContainer(pod, cs, now)...)
	}

	return signals
}

// checkEviction detects evicted pods.
func (p *PodCollector) checkEviction(pod corev1.Pod, now time.Time) (Signal, bool) {
	if pod.Status.Phase != corev1.PodFailed || pod.Status.Reason != "Evicted" {
		return Signal{}, false
	}

	return Signal{
		Type:       SignalPodEvicted,
		Severity:   SeverityCritical,
		Category:   CategoryHealth,
		Source:     podCollectorName,
		Message:    fmt.Sprintf("Pod %s/%s was evicted: %s", pod.Namespace, pod.Name, pod.Status.Message),
		Resource:   podResource(pod),
		DetectedAt: now,
		Metadata:   podMetadata(pod),
	}, true
}

// checkPendingLong detects pods stuck in Pending state.
func (p *PodCollector) checkPendingLong(pod corev1.Pod, now time.Time) (Signal, bool) {
	if pod.Status.Phase != corev1.PodPending {
		return Signal{}, false
	}

	if pod.CreationTimestamp.IsZero() {
		return Signal{}, false
	}

	age := now.Sub(pod.CreationTimestamp.Time)
	if age < p.pendingTimeout {
		return Signal{}, false
	}

	pendingMinutes := age.Minutes()
	threshold := p.pendingTimeout.Minutes()

	return Signal{
		Type:       SignalPodPendingLong,
		Severity:   SeverityWarning,
		Category:   CategoryHealth,
		Source:     podCollectorName,
		Message:    fmt.Sprintf("Pod %s/%s has been pending for %.0f minutes", pod.Namespace, pod.Name, pendingMinutes),
		Resource:   podResource(pod),
		Value:      &pendingMinutes,
		Threshold:  &threshold,
		DetectedAt: pod.CreationTimestamp.Time,
		Metadata:   podMetadata(pod),
	}, true
}

// checkContainer examines a container status for failures.
func (p *PodCollector) checkContainer(pod corev1.Pod, cs corev1.ContainerStatus, now time.Time) []Signal {
	var signals []Signal

	// Check waiting state
	if cs.State.Waiting != nil {
		if signal, ok := p.checkWaiting(pod, cs, now); ok {
			signals = append(signals, signal)
		}
	}

	// Check for OOMKilled (current or last termination)
	if signal, ok := p.checkOOMKilled(pod, cs, now); ok {
		signals = append(signals, signal)
	}

	// Check liveness/readiness probe failures via container restart + waiting
	if signal, ok := p.checkProbeFailure(pod, cs, now); ok {
		signals = append(signals, signal)
	}

	// Check high restart count
	if signal, ok := p.checkHighRestarts(pod, cs, now); ok {
		signals = append(signals, signal)
	}

	return signals
}

// checkWaiting detects CrashLoopBackOff and ImagePullBackOff.
func (p *PodCollector) checkWaiting(pod corev1.Pod, cs corev1.ContainerStatus, now time.Time) (Signal, bool) {
	reason := cs.State.Waiting.Reason

	switch reason {
	case "CrashLoopBackOff":
		return Signal{
			Type:       SignalCrashLoopBackOff,
			Severity:   SeverityCritical,
			Category:   CategoryHealth,
			Source:     podCollectorName,
			Message:    fmt.Sprintf("Container %s in pod %s/%s is in CrashLoopBackOff", cs.Name, pod.Namespace, pod.Name),
			Resource:   podResource(pod),
			DetectedAt: now,
			Metadata:   containerMetadata(pod, cs),
		}, true

	case "ImagePullBackOff", "ErrImagePull":
		return Signal{
			Type:       SignalImagePullBackOff,
			Severity:   SeverityWarning,
			Category:   CategoryHealth,
			Source:     podCollectorName,
			Message:    fmt.Sprintf("Container %s in pod %s/%s cannot pull image: %s", cs.Name, pod.Namespace, pod.Name, cs.State.Waiting.Message),
			Resource:   podResource(pod),
			DetectedAt: now,
			Metadata:   containerMetadata(pod, cs),
		}, true
	}

	return Signal{}, false
}

// checkOOMKilled detects OOMKilled containers via current or last termination state.
func (p *PodCollector) checkOOMKilled(pod corev1.Pod, cs corev1.ContainerStatus, now time.Time) (Signal, bool) {
	var terminated *corev1.ContainerStateTerminated

	if cs.State.Terminated != nil && cs.State.Terminated.Reason == string(SignalOOMKilled) {
		terminated = cs.State.Terminated
	} else if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == string(SignalOOMKilled) {
		terminated = cs.LastTerminationState.Terminated
	}

	if terminated == nil {
		return Signal{}, false
	}

	detectedAt := now
	if !terminated.FinishedAt.IsZero() {
		detectedAt = terminated.FinishedAt.Time
	}

	meta := containerMetadata(pod, cs)
	meta["lastTerminationReason"] = string(SignalOOMKilled)

	// Extract memory limit from pod spec
	for _, c := range pod.Spec.Containers {
		if c.Name == cs.Name {
			if memLimit, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				meta["memoryLimit"] = memLimit.String()
			}
			break
		}
	}

	return Signal{
		Type:       SignalOOMKilled,
		Severity:   SeverityCritical,
		Category:   CategoryHealth,
		Source:     podCollectorName,
		Message:    fmt.Sprintf("Container %s in pod %s/%s was OOMKilled", cs.Name, pod.Namespace, pod.Name),
		Resource:   podResource(pod),
		DetectedAt: detectedAt,
		Metadata:   meta,
	}, true
}

// checkProbeFailure detects liveness/readiness probe failures.
// Indicated by a container that is not ready and is waiting with reason ContainerNotReady or has been restarted.
func (p *PodCollector) checkProbeFailure(pod corev1.Pod, cs corev1.ContainerStatus, now time.Time) (Signal, bool) {
	// A container that is running but not ready with restarts suggests probe failures
	if cs.State.Running == nil || cs.Ready || cs.RestartCount == 0 {
		return Signal{}, false
	}

	// Check if last termination was not OOM (OOM is already handled separately)
	if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == string(SignalOOMKilled) {
		return Signal{}, false
	}

	return Signal{
		Type:       SignalProbeFailure,
		Severity:   SeverityWarning,
		Category:   CategoryHealth,
		Source:     podCollectorName,
		Message:    fmt.Sprintf("Container %s in pod %s/%s is failing probes (restarts: %d)", cs.Name, pod.Namespace, pod.Name, cs.RestartCount),
		Resource:   podResource(pod),
		DetectedAt: now,
		Metadata:   containerMetadata(pod, cs),
	}, true
}

// checkHighRestarts detects containers with excessive restart counts.
func (p *PodCollector) checkHighRestarts(pod corev1.Pod, cs corev1.ContainerStatus, now time.Time) (Signal, bool) {
	if cs.RestartCount <= p.restartThreshold {
		return Signal{}, false
	}

	restarts := float64(cs.RestartCount)
	threshold := float64(p.restartThreshold)

	return Signal{
		Type:       SignalContainerRestart,
		Severity:   SeverityWarning,
		Category:   CategoryHealth,
		Source:     podCollectorName,
		Message:    fmt.Sprintf("Container %s in pod %s/%s has restarted %d times", cs.Name, pod.Namespace, pod.Name, cs.RestartCount),
		Resource:   podResource(pod),
		Value:      &restarts,
		Threshold:  &threshold,
		DetectedAt: now,
		Metadata:   containerMetadata(pod, cs),
	}, true
}

func podResource(pod corev1.Pod) dorguv1.ResourceReference {
	return dorguv1.ResourceReference{
		Kind:      "Pod",
		Name:      pod.Name,
		Namespace: pod.Namespace,
	}
}

func podMetadata(pod corev1.Pod) map[string]string {
	meta := map[string]string{
		"namespace": pod.Namespace,
		"nodeName":  pod.Spec.NodeName,
	}
	if deploy := ownerDeployment(pod); deploy != "" {
		meta[MetadataKeyDeployment] = deploy
	}
	return meta
}

func containerMetadata(pod corev1.Pod, cs corev1.ContainerStatus) map[string]string {
	meta := podMetadata(pod)
	meta["container"] = cs.Name
	meta["image"] = cs.Image
	meta["restartCount"] = fmt.Sprintf("%d", cs.RestartCount)
	return meta
}

// ownerDeployment extracts the deployment name from pod owner references.
// Walks the chain: Pod -> ReplicaSet -> Deployment (via naming convention).
func ownerDeployment(pod corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "ReplicaSet" {
			// ReplicaSet names follow the pattern <deployment>-<hash>
			name := ref.Name
			if lastDash := strings.LastIndexByte(name, '-'); lastDash > 0 {
				return name[:lastDash]
			}
			return name
		}
	}
	return ""
}
