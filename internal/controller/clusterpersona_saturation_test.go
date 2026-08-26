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

// F-09: `dorgu health` printed "CPU: n/a requests / allocatable ( / 3860m)" on
// every cluster. The empty left operand was not a formatting slip alone: the
// ClusterPersona summary declared usedCPU/usedMemory/utilization and nothing ever
// wrote them, so the CLI had nothing to render.
package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

var _ = Describe("ClusterPersona resource saturation", func() {
	// requestingPod builds a running pod, scheduled onto a node, requesting the
	// given CPU and memory. The nodeName is load-bearing rather than decoration:
	// it is what makes the pod hold an allocation at all (CR-03).
	requestingPod := func(name, cpu, memory string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
				Containers: []corev1.Container{{
					Name:  "app",
					Image: "app:1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpu),
							corev1.ResourceMemory: resource.MustParse(memory),
						},
					},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}

	Context("setClaimedResources", func() {
		It("totals pod requests and reports utilization against allocatable", func() {
			summary := &dorguv1.ClusterResourceSummary{}
			pods := []corev1.Pod{
				requestingPod("a", "500m", "512Mi"),
				requestingPod("b", "1500m", "1Gi"),
			}

			r := &ClusterPersonaReconciler{}
			r.setClaimedResources(summary, pods,
				resource.MustParse("4"), resource.MustParse("8Gi"))

			Expect(summary.UsedCPU).To(Equal("2"))
			Expect(summary.UsedMemory).To(Equal("1536Mi"))
			Expect(summary.CPUUtilization).To(Equal("50%"))
			Expect(summary.MemoryUtilization).To(Equal("19%"))
		})

		It("ignores terminal pods, which hold no allocation", func() {
			summary := &dorguv1.ClusterResourceSummary{}
			succeeded := requestingPod("done", "2", "2Gi")
			succeeded.Status.Phase = corev1.PodSucceeded
			failed := requestingPod("dead", "2", "2Gi")
			failed.Status.Phase = corev1.PodFailed

			r := &ClusterPersonaReconciler{}
			r.setClaimedResources(summary,
				[]corev1.Pod{requestingPod("live", "500m", "512Mi"), succeeded, failed},
				resource.MustParse("1"), resource.MustParse("1Gi"))

			Expect(summary.UsedCPU).To(Equal("500m"))
			Expect(summary.CPUUtilization).To(Equal("50%"))
		})

		It("reports zero rather than an empty string on an idle cluster", func() {
			summary := &dorguv1.ClusterResourceSummary{}

			r := &ClusterPersonaReconciler{}
			r.setClaimedResources(summary, nil,
				resource.MustParse("4"), resource.MustParse("8Gi"))

			Expect(summary.UsedCPU).To(Equal("0"))
			Expect(summary.UsedMemory).To(Equal("0"))
			Expect(summary.CPUUtilization).To(Equal("0%"))
		})

		It("leaves utilization empty when allocatable is unknown, instead of claiming 0%", func() {
			summary := &dorguv1.ClusterResourceSummary{}

			r := &ClusterPersonaReconciler{}
			r.setClaimedResources(summary, []corev1.Pod{requestingPod("a", "500m", "512Mi")},
				resource.Quantity{}, resource.Quantity{})

			Expect(summary.UsedCPU).To(Equal("500m"))
			Expect(summary.CPUUtilization).To(BeEmpty())
			Expect(summary.MemoryUtilization).To(BeEmpty())
		})
	})

	// CF6-2 / CR-03. The clean-room cluster reported 1689% CPU where 25% was
	// requested, because pods no node had accepted were summed against node
	// allocatable. A pod that cannot be placed can request more than the cluster
	// owns, so the error had no ceiling.
	Context("unschedulable pods", func() {
		// queuedPod builds a Pending pod the scheduler has not placed: no
		// nodeName, and a request larger than the whole cluster.
		queuedPod := func(name, cpu string) corev1.Pod {
			pod := requestingPod(name, cpu, "64Gi")
			pod.Spec.NodeName = ""
			pod.Status.Phase = corev1.PodPending
			return pod
		}

		It("excludes them from allocatable-based math", func() {
			summary := &dorguv1.ClusterResourceSummary{}
			pods := []corev1.Pod{
				requestingPod("web", "965m", "512Mi"),
				queuedPod("big-1", "21"),
				queuedPod("big-2", "21"),
				queuedPod("big-3", "21"),
			}

			r := &ClusterPersonaReconciler{}
			r.setClaimedResources(summary, pods,
				resource.MustParse("3860m"), resource.MustParse("8Gi"))

			Expect(summary.UsedCPU).To(Equal("965m"))
			Expect(summary.CPUUtilization).To(Equal("25%"))
		})

		It("never reports more than 100% from requests alone on a healthy cluster", func() {
			summary := &dorguv1.ClusterResourceSummary{}
			pods := []corev1.Pod{queuedPod("enormous", "1000")}

			r := &ClusterPersonaReconciler{}
			r.setClaimedResources(summary, pods,
				resource.MustParse("4"), resource.MustParse("8Gi"))

			// Nothing is placed, so nothing is claimed.
			Expect(summary.UsedCPU).To(Equal("0"))
			Expect(summary.CPUUtilization).To(Equal("0%"))
		})

		It("counts a Pending pod that has been bound to a node", func() {
			// Bound but not yet started (pulling an image, for instance) still
			// holds its reservation on that node.
			summary := &dorguv1.ClusterResourceSummary{}
			binding := requestingPod("starting", "1", "512Mi")
			binding.Status.Phase = corev1.PodPending

			r := &ClusterPersonaReconciler{}
			r.setClaimedResources(summary, []corev1.Pod{binding},
				resource.MustParse("4"), resource.MustParse("8Gi"))

			Expect(summary.UsedCPU).To(Equal("1"))
			Expect(summary.CPUUtilization).To(Equal("25%"))
		})
	})

	Context("podHoldsAllocation", func() {
		It("distinguishes released from never-granted", func() {
			scheduled := requestingPod("live", "100m", "64Mi")
			Expect(podHoldsAllocation(&scheduled)).To(BeTrue())

			done := requestingPod("done", "100m", "64Mi")
			done.Status.Phase = corev1.PodSucceeded
			Expect(podHoldsAllocation(&done)).To(BeFalse())

			dead := requestingPod("dead", "100m", "64Mi")
			dead.Status.Phase = corev1.PodFailed
			Expect(podHoldsAllocation(&dead)).To(BeFalse())

			queued := requestingPod("queued", "100m", "64Mi")
			queued.Spec.NodeName = ""
			Expect(podHoldsAllocation(&queued)).To(BeFalse())
		})
	})

	Context("podRequests", func() {
		It("takes the larger of the app-container sum and the biggest init container", func() {
			pod := requestingPod("with-init", "200m", "128Mi")
			pod.Spec.InitContainers = []corev1.Container{{
				Name:  "migrate",
				Image: "migrate:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			}}

			cpu, memory := podRequests(&pod)

			// Init container dominates CPU; the app containers dominate memory.
			Expect(cpu.String()).To(Equal("1"))
			Expect(memory.String()).To(Equal("128Mi"))
		})

		It("returns zero for a pod that requests nothing", func() {
			pod := corev1.Pod{
				Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1"}}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}

			cpu, memory := podRequests(&pod)

			Expect(cpu.IsZero()).To(BeTrue())
			Expect(memory.IsZero()).To(BeTrue())
		})
	})
})
