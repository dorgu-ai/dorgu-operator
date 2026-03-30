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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestMetricsCollector_Name(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	// Use nil metricsClient — Name() doesn't need it
	mc := &MetricsCollector{logger: logger}
	assert.Equal(t, "metrics-usage-checker", mc.Name())
}

func TestMetricsCollector_Unavailable(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	mc := &MetricsCollector{
		logger: logger,
		// available defaults to zero value (false) for atomic.Bool
	}

	signals, err := mc.Collect(context.Background())
	require.NoError(t, err)
	assert.Empty(t, signals, "unavailable metrics-server should return empty")
}

func TestMetricsCollector_CheckPodMetrics_CPUHigh(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	mc := &MetricsCollector{
		logger:         logger,
		usageThreshold: 0.90,
	}

	podMetrics := metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Containers: []metricsv1beta1.ContainerMetrics{
			{
				Name: "app",
				Usage: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("950m"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		},
	}

	podSpecs := map[podSpecKey]corev1.PodSpec{
		{namespace: "default", name: "test-pod"}: {
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
	}

	signals := mc.checkPodMetrics(podMetrics, podSpecs, metav1.Now().Time)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalCPUUsageHigh, signals[0].Type)
	assert.Equal(t, SeverityWarning, signals[0].Severity)
	assert.Equal(t, CategoryResource, signals[0].Category)
	assert.InDelta(t, 95.0, *signals[0].Value, 1.0)
}

func TestMetricsCollector_CheckPodMetrics_MemoryHigh(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	mc := &MetricsCollector{
		logger:         logger,
		usageThreshold: 0.90,
	}

	podMetrics := metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Containers: []metricsv1beta1.ContainerMetrics{
			{
				Name: "app",
				Usage: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("950Mi"),
				},
			},
		},
	}

	podSpecs := map[podSpecKey]corev1.PodSpec{
		{namespace: "default", name: "test-pod"}: {
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
	}

	signals := mc.checkPodMetrics(podMetrics, podSpecs, metav1.Now().Time)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalMemoryUsageHigh, signals[0].Type)
}

func TestMetricsCollector_CheckPodMetrics_BelowThreshold(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	mc := &MetricsCollector{
		logger:         logger,
		usageThreshold: 0.90,
	}

	podMetrics := metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Containers: []metricsv1beta1.ContainerMetrics{
			{
				Name: "app",
				Usage: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("500Mi"),
				},
			},
		},
	}

	podSpecs := map[podSpecKey]corev1.PodSpec{
		{namespace: "default", name: "test-pod"}: {
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
	}

	signals := mc.checkPodMetrics(podMetrics, podSpecs, metav1.Now().Time)
	assert.Empty(t, signals, "50% usage should not trigger signals")
}

func TestMetricsCollector_CheckPodMetrics_NoLimits(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	mc := &MetricsCollector{
		logger:         logger,
		usageThreshold: 0.90,
	}

	podMetrics := metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Containers: []metricsv1beta1.ContainerMetrics{
			{
				Name: "app",
				Usage: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("950m"),
					corev1.ResourceMemory: resource.MustParse("950Mi"),
				},
			},
		},
	}

	podSpecs := map[podSpecKey]corev1.PodSpec{
		{namespace: "default", name: "test-pod"}: {
			Containers: []corev1.Container{
				{
					Name:      "app",
					Resources: corev1.ResourceRequirements{},
				},
			},
		},
	}

	signals := mc.checkPodMetrics(podMetrics, podSpecs, metav1.Now().Time)
	assert.Empty(t, signals, "pods without limits should not produce signals")
}

func TestMetricsCollector_CheckPodMetrics_UnknownPod(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	mc := &MetricsCollector{
		logger:         logger,
		usageThreshold: 0.90,
	}

	podMetrics := metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "unknown-pod", Namespace: "default"},
		Containers: []metricsv1beta1.ContainerMetrics{
			{
				Name: "app",
				Usage: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("950m"),
				},
			},
		},
	}

	signals := mc.checkPodMetrics(podMetrics, map[podSpecKey]corev1.PodSpec{}, metav1.Now().Time)
	assert.Empty(t, signals, "unknown pod should be skipped")
}

func TestMetricsCollector_GetPodSpecs(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(pod).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	mc := &MetricsCollector{
		client: fakeClient,
		logger: logger,
	}

	specs, err := mc.getPodSpecs(context.Background())
	require.NoError(t, err)
	assert.Len(t, specs, 1)

	key := podSpecKey{namespace: "default", name: "test-pod"}
	spec, ok := specs[key]
	require.True(t, ok)
	assert.Len(t, spec.Containers, 1)
	assert.Equal(t, "app", spec.Containers[0].Name)
}

func TestMetricsCollector_BothCPUAndMemoryHigh(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	mc := &MetricsCollector{
		logger:         logger,
		usageThreshold: 0.90,
	}

	podMetrics := metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Containers: []metricsv1beta1.ContainerMetrics{
			{
				Name: "app",
				Usage: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("950m"),
					corev1.ResourceMemory: resource.MustParse("950Mi"),
				},
			},
		},
	}

	podSpecs := map[podSpecKey]corev1.PodSpec{
		{namespace: "default", name: "test-pod"}: {
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
	}

	signals := mc.checkPodMetrics(podMetrics, podSpecs, metav1.Now().Time)
	assert.Len(t, signals, 2)

	var types []SignalType
	for _, s := range signals {
		types = append(types, s.Type)
	}
	assert.Contains(t, types, SignalCPUUsageHigh)
	assert.Contains(t, types, SignalMemoryUsageHigh)
}
