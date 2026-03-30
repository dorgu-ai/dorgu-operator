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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestPodCollector_Collect(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))

	tests := []struct {
		name      string
		pods      []corev1.Pod
		wantLen   int
		wantTypes []SignalType
	}{
		{
			name:    "empty cluster",
			pods:    nil,
			wantLen: 0,
		},
		{
			name: "healthy pods",
			pods: []corev1.Pod{
				makePod("default", "app-1", corev1.PodRunning, nil),
			},
			wantLen: 0,
		},
		{
			name: "OOMKilled container (current state)",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "main",
								Resources: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
								},
							},
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodFailed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "main",
								Image: "app:latest",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{
										Reason:   "OOMKilled",
										ExitCode: 137,
									},
								},
							},
						},
					},
				},
			},
			wantLen:   1,
			wantTypes: []SignalType{SignalOOMKilled},
		},
		{
			name: "OOMKilled container (last termination state)",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main"}},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:         "main",
								Image:        "app:latest",
								RestartCount: 3,
								State: corev1.ContainerState{
									Running: &corev1.ContainerStateRunning{},
								},
								LastTerminationState: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{
										Reason:   "OOMKilled",
										ExitCode: 137,
									},
								},
							},
						},
					},
				},
			},
			wantLen:   1,
			wantTypes: []SignalType{SignalOOMKilled},
		},
		{
			name: "CrashLoopBackOff",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main"}},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "main",
								Image: "app:latest",
								State: corev1.ContainerState{
									Waiting: &corev1.ContainerStateWaiting{
										Reason: "CrashLoopBackOff",
									},
								},
							},
						},
					},
				},
			},
			wantLen:   1,
			wantTypes: []SignalType{SignalCrashLoopBackOff},
		},
		{
			name: "ImagePullBackOff",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main"}},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "main",
								Image: "nonexistent:latest",
								State: corev1.ContainerState{
									Waiting: &corev1.ContainerStateWaiting{
										Reason:  "ImagePullBackOff",
										Message: "Back-off pulling image",
									},
								},
							},
						},
					},
				},
			},
			wantLen:   1,
			wantTypes: []SignalType{SignalImagePullBackOff},
		},
		{
			name: "ErrImagePull also detected as ImagePullBackOff signal",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main"}},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "main",
								Image: "nonexistent:latest",
								State: corev1.ContainerState{
									Waiting: &corev1.ContainerStateWaiting{
										Reason: "ErrImagePull",
									},
								},
							},
						},
					},
				},
			},
			wantLen:   1,
			wantTypes: []SignalType{SignalImagePullBackOff},
		},
		{
			name: "evicted pod",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
					Status: corev1.PodStatus{
						Phase:   corev1.PodFailed,
						Reason:  "Evicted",
						Message: "The node had condition: [DiskPressure]",
					},
				},
			},
			wantLen:   1,
			wantTypes: []SignalType{SignalPodEvicted},
		},
		{
			name: "pod pending too long",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "app-1",
						Namespace:         "default",
						CreationTimestamp:  metav1.NewTime(time.Now().Add(-10 * time.Minute)),
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				},
			},
			wantLen:   1,
			wantTypes: []SignalType{SignalPodPendingLong},
		},
		{
			name: "pod pending within threshold not detected",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "app-1",
						Namespace:         "default",
						CreationTimestamp:  metav1.NewTime(time.Now().Add(-2 * time.Minute)),
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				},
			},
			wantLen: 0,
		},
		{
			name: "high restart count",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:         "main",
								Image:        "app:latest",
								RestartCount: 10,
								Ready:        true,
								State: corev1.ContainerState{
									Running: &corev1.ContainerStateRunning{},
								},
							},
						},
					},
				},
			},
			wantLen:   1,
			wantTypes: []SignalType{SignalContainerRestart},
		},
		{
			name: "restart count at threshold not detected",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:         "main",
								Image:        "app:latest",
								RestartCount: 5,
								Ready:        true,
								State: corev1.ContainerState{
									Running: &corev1.ContainerStateRunning{},
								},
							},
						},
					},
				},
			},
			wantLen: 0,
		},
		{
			name: "kube-system pods excluded",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system"},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
					Status: corev1.PodStatus{
						Phase:  corev1.PodFailed,
						Reason: "Evicted",
					},
				},
			},
			wantLen: 0,
		},
		{
			name: "probe failure detected",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:         "main",
								Image:        "app:latest",
								RestartCount: 3,
								Ready:        false,
								State: corev1.ContainerState{
									Running: &corev1.ContainerStateRunning{},
								},
								LastTerminationState: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{
										ExitCode: 1,
										Reason:   "Error",
									},
								},
							},
						},
					},
				},
			},
			wantLen:   1,
			wantTypes: []SignalType{SignalProbeFailure},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.pods))
			for i := range tt.pods {
				objects = append(objects, &tt.pods[i])
			}

			fakeClient := fake.NewClientBuilder().WithRuntimeObjects(objects...).Build()
			collector := NewPodCollector(fakeClient, logger)

			signals, err := collector.Collect(context.Background())
			require.NoError(t, err)
			assert.Len(t, signals, tt.wantLen, "expected %d signals, got %d", tt.wantLen, len(signals))

			for i, wantType := range tt.wantTypes {
				if i < len(signals) {
					assert.Equal(t, wantType, signals[i].Type, "signal %d type", i)
					assert.Equal(t, podCollectorName, signals[i].Source)
				}
			}
		})
	}
}

func TestPodCollector_OOMKilledMetadata(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-1",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "myapp-abc123"},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "worker-1",
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "main",
					Image: "app:latest",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:   "OOMKilled",
							ExitCode: 137,
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithRuntimeObjects(&pod).Build()
	collector := NewPodCollector(fakeClient, logger)

	signals, err := collector.Collect(context.Background())
	require.NoError(t, err)
	require.Len(t, signals, 1)

	assert.Equal(t, SignalOOMKilled, signals[0].Type)
	assert.Equal(t, SeverityCritical, signals[0].Severity)
	assert.Equal(t, "OOMKilled", signals[0].Metadata["lastTerminationReason"])
	assert.Equal(t, "256Mi", signals[0].Metadata["memoryLimit"])
	assert.Equal(t, "myapp", signals[0].Metadata["deployment"])
	assert.Equal(t, "worker-1", signals[0].Metadata["nodeName"])
}

func TestPodCollector_Name(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	collector := NewPodCollector(nil, logger)
	assert.Equal(t, podCollectorName, collector.Name())
}

func TestOwnerDeployment(t *testing.T) {
	tests := []struct {
		name  string
		pod   corev1.Pod
		want  string
	}{
		{
			name: "pod owned by ReplicaSet",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "ReplicaSet", Name: "myapp-abc123"},
					},
				},
			},
			want: "myapp",
		},
		{
			name: "pod with no owner",
			pod:  corev1.Pod{},
			want: "",
		},
		{
			name: "pod owned by StatefulSet (not ReplicaSet)",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "StatefulSet", Name: "mydb-0"},
					},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ownerDeployment(tt.pod))
		})
	}
}

func makePod(ns, name string, phase corev1.PodPhase, containerStatuses []corev1.ContainerStatus) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		Status: corev1.PodStatus{
			Phase:             phase,
			ContainerStatuses: containerStatuses,
		},
	}
}
