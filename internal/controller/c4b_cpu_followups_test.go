package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// C4b (re-review of PR 64, N-1).

// podMetricsAt is a metrics.k8s.io that is served and reports one container's CPU usage.
type podMetricsAt struct {
	namespace, pod, container, usage string
}

func (podMetricsAt) Available(context.Context) (bool, error) { return true, nil }
func (p podMetricsAt) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	return []metricsv1beta1.PodMetrics{{
		ObjectMeta: metav1.ObjectMeta{Name: p.pod, Namespace: p.namespace},
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: p.container, Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(p.usage)},
		}},
	}}, nil
}

// limitedCheckoutPod is basePod with the 500m CPU limit the review's probe used.
func limitedCheckoutPod() *corev1.Pod {
	pod := basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout")
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}
	return pod
}

// The reviewer's probe (N-1): the source was gone for 5 minutes or more, the operator
// comes back (a fresh collector), the pod is still pinned at its 500m limit, and metrics
// have not yet produced the three cycles that would raise the signal. The first two cycles
// are silence from a source that has not yet been able to say anything.
func TestC4bN1_AStillThrottledIncidentDoesNotResolveDuringTheWarmUp(t *testing.T) {
	ctx := context.Background()
	im := cpuIncidentFor() // last seen six minutes ago
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), limitedCheckoutPod())

	hot := podMetricsAt{namespace: "platform", pod: "checkout-57c95bf9b8-47vp9", container: "checkout", usage: "500m"}
	collector := detection.NewCPUThrottlingCollector(c, logr.Discard(),
		detection.WithMinCycleSpacing(0), detection.WithMetricsSource(hot))
	r.Detection = detection.NewEngine(logr.Discard(), collector)

	for cycle := 1; cycle < detection.CPUUsageConsecutiveCycles; cycle++ {
		r.runCycle(ctx)
		assert.Equal(t, PhaseDetected, phaseOf(t, c, im),
			"cycle %d: the source cannot yet say a hot pod is hot, so its silence is not resolution evidence", cycle)
	}
}

func TestC4bN1_AQuietPodResolvesOnTheThirdGoodCycle(t *testing.T) {
	ctx := context.Background()
	im := cpuIncidentFor()
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), limitedCheckoutPod())

	quiet := podMetricsAt{namespace: "platform", pod: "checkout-57c95bf9b8-47vp9", container: "checkout", usage: "50m"}
	collector := detection.NewCPUThrottlingCollector(c, logr.Discard(),
		detection.WithMinCycleSpacing(0), detection.WithMetricsSource(quiet))
	r.Detection = detection.NewEngine(logr.Discard(), collector)

	for cycle := 1; cycle < detection.CPUUsageConsecutiveCycles; cycle++ {
		r.runCycle(ctx)
		require.Equal(t, PhaseDetected, phaseOf(t, c, im), "cycle %d", cycle)
	}
	r.runCycle(ctx)
	assert.Equal(t, PhaseResolved, phaseOf(t, c, im), "three good cycles with the pod seen and quiet")
}

// An answer that is served and names none of the limited pods is "no data yet" (a
// metrics-server that just restarted), the same logic as an empty Prometheus vector.
func TestC4bN1_AnEmptyAnswerForAClusterWithLimitedPodsIsNotResolutionEvidence(t *testing.T) {
	ctx := context.Background()
	im := cpuIncidentFor()
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), limitedCheckoutPod())

	collector := detection.NewCPUThrottlingCollector(c, logr.Discard(),
		detection.WithMinCycleSpacing(0), detection.WithMetricsSource(quietMetrics{present: true}))
	r.Detection = detection.NewEngine(logr.Discard(), collector)

	for range 2 * detection.CPUUsageConsecutiveCycles {
		r.runCycle(ctx)
	}
	assert.Equal(t, PhaseDetected, phaseOf(t, c, im),
		"metrics.k8s.io has answered with an empty list for a pod that has a CPU limit: no data, not quiet")
}
