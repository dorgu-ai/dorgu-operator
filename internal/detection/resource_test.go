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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Fixture values every test in this file shares.
const (
	testNodeName  = "node-1"
	testNamespace = "default"
)

func TestResourceCollector_Name(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	c := NewResourceCollector(fake.NewClientBuilder().Build(), logger)
	assert.Equal(t, "resource-saturation-checker", c.Name())
}

func TestResourceCollector_EmptyCluster(t *testing.T) {
	signals := collectResourceSignals(t, nil, nil)
	assert.Empty(t, signals)
}

func TestResourceCollector_NoSaturation(t *testing.T) {
	node := makeNode("4000m", "8Gi")
	pod := makePodOnNode("pod-1", "500m", "1Gi")

	signals := collectResourceSignals(t, []*corev1.Node{node}, []*corev1.Pod{pod})
	assert.Empty(t, signals, "50% saturation should not trigger any signals")
}

func TestResourceCollector_CPUWarning(t *testing.T) {
	node := makeNode("1000m", "8Gi")
	// 900m / 1000m = 90% > 85% warning threshold
	pod := makePodOnNode("pod-1", "900m", "1Gi")

	signals := collectResourceSignals(t, []*corev1.Node{node}, []*corev1.Pod{pod})
	require.Len(t, signals, 1)
	assert.Equal(t, SignalCPUSaturationHigh, signals[0].Type)
	assert.Equal(t, SeverityWarning, signals[0].Severity)
	assert.Equal(t, CategoryResource, signals[0].Category)
	assert.InDelta(t, 90.0, *signals[0].Value, 1.0)
}

func TestResourceCollector_CPUCritical(t *testing.T) {
	node := makeNode("1000m", "8Gi")
	// 960m / 1000m = 96% > 95% critical threshold
	pod := makePodOnNode("pod-1", "960m", "1Gi")

	signals := collectResourceSignals(t, []*corev1.Node{node}, []*corev1.Pod{pod})
	require.Len(t, signals, 1)
	assert.Equal(t, SignalCPUSaturationCritical, signals[0].Type)
	assert.Equal(t, SeverityCritical, signals[0].Severity)
}

func TestResourceCollector_MemoryWarning(t *testing.T) {
	node := makeNode("4000m", "1Gi")
	// 900Mi / 1Gi ≈ 87.9% > 85% warning
	pod := makePodOnNode("pod-1", "100m", "900Mi")

	signals := collectResourceSignals(t, []*corev1.Node{node}, []*corev1.Pod{pod})
	require.Len(t, signals, 1)
	assert.Equal(t, SignalMemorySaturationHigh, signals[0].Type)
	assert.Equal(t, SeverityWarning, signals[0].Severity)
}

func TestResourceCollector_MemoryCritical(t *testing.T) {
	node := makeNode("4000m", "1Gi")
	// 980Mi / 1Gi ≈ 95.7% > 95% critical
	pod := makePodOnNode("pod-1", "100m", "980Mi")

	signals := collectResourceSignals(t, []*corev1.Node{node}, []*corev1.Pod{pod})
	require.Len(t, signals, 1)
	assert.Equal(t, SignalMemorySaturationCrit, signals[0].Type)
	assert.Equal(t, SeverityCritical, signals[0].Severity)
}

func TestResourceCollector_BothCPUAndMemory(t *testing.T) {
	node := makeNode("1000m", "1Gi")
	// CPU: 900m/1000m = 90%, Memory: 900Mi/1Gi ≈ 87.9%
	pod := makePodOnNode("pod-1", "900m", "900Mi")

	signals := collectResourceSignals(t, []*corev1.Node{node}, []*corev1.Pod{pod})
	assert.Len(t, signals, 2)

	types := make([]SignalType, 0, len(signals))
	for _, s := range signals {
		types = append(types, s.Type)
	}
	assert.Contains(t, types, SignalCPUSaturationHigh)
	assert.Contains(t, types, SignalMemorySaturationHigh)
}

func TestResourceCollector_TerminatedPodsExcluded(t *testing.T) {
	node := makeNode("1000m", "1Gi")
	pod := makePodOnNode("pod-1", "960m", "980Mi")
	pod.Status.Phase = corev1.PodSucceeded // terminated pod

	signals := collectResourceSignals(t, []*corev1.Node{node}, []*corev1.Pod{pod})
	assert.Empty(t, signals, "terminated pods should not count toward saturation")
}

func TestResourceCollector_MultiplePodsSummed(t *testing.T) {
	node := makeNode("1000m", "8Gi")
	// 3 pods x 300m = 900m / 1000m = 90%
	pod1 := makePodOnNode("pod-1", "300m", "100Mi")
	pod2 := makePodOnNode("pod-2", "300m", "100Mi")
	pod3 := makePodOnNode("pod-3", "300m", "100Mi")

	signals := collectResourceSignals(t, []*corev1.Node{node}, []*corev1.Pod{pod1, pod2, pod3})
	require.Len(t, signals, 1)
	assert.Equal(t, SignalCPUSaturationHigh, signals[0].Type)
}

func TestResourceCollector_SignalHasValueAndThreshold(t *testing.T) {
	node := makeNode("1000m", "8Gi")
	pod := makePodOnNode("pod-1", "900m", "1Gi")

	signals := collectResourceSignals(t, []*corev1.Node{node}, []*corev1.Pod{pod})
	require.Len(t, signals, 1)
	require.NotNil(t, signals[0].Value)
	require.NotNil(t, signals[0].Threshold)
	assert.InDelta(t, 90.0, *signals[0].Value, 1.0)
	assert.InDelta(t, 85.0, *signals[0].Threshold, 1.0)
}

// --- helpers ---

func makeNode(cpu, memory string) *corev1.Node {
	const name = testNodeName
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
		},
	}
}

func makePodOnNode(name, cpu, memory string) *corev1.Pod {
	const (
		namespace = testNamespace
		nodeName  = testNodeName
	)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpu),
							corev1.ResourceMemory: resource.MustParse(memory),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

func collectResourceSignals(t *testing.T, nodes []*corev1.Node, pods []*corev1.Pod) []Signal {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)

	objs := make([]runtime.Object, 0, len(nodes)+len(pods))
	for _, n := range nodes {
		objs = append(objs, n)
	}
	for _, p := range pods {
		objs = append(objs, p)
	}
	builder = builder.WithRuntimeObjects(objs...)

	// Index spec.nodeName for field selector support, using the same index func
	// the manager registers at startup (detection.RegisterPodNodeNameIndex).
	fakeClient := builder.
		WithIndex(&corev1.Pod{}, PodNodeNameIndex, PodByNodeName).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	collector := NewResourceCollector(fakeClient, logger)

	signals, err := collector.Collect(context.Background())
	require.NoError(t, err)
	return signals
}
