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

package controller

import (
	"context"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// sidecarMetrics is metrics.k8s.io for a pod whose main container idles and whose
// sidecar sits at its limit.
type sidecarMetrics struct{ ns, pod string }

func (sidecarMetrics) Available(context.Context) (bool, error) { return true, nil }

func (m sidecarMetrics) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	cpu := func(q string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(q)}
	}
	return []metricsv1beta1.PodMetrics{{
		ObjectMeta: metav1.ObjectMeta{Name: m.pod, Namespace: m.ns},
		Containers: []metricsv1beta1.ContainerMetrics{
			{Name: "app", Usage: cpu("100m")},
			{Name: "sidecar", Usage: cpu("99m")},
		},
	}}, nil
}

// C4b N-7 (and N-3 on a real API server): a throttled sidecar of a workload whose
// persona describes its main container gets no proposal, and the incident is still
// recorded, with the advice on it as an Event.
var _ = Describe("CPU throttling of a sidecar on a real API server (C4b)", func() {
	const (
		ns  = "c4b-sidecar"
		app = "c4b-multi"
	)
	bg := context.Background()

	It("records the incident, proposes nothing, and says why as an Event", func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(bg, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))).To(Succeed())
		cpuLimit := func(q string) corev1.ResourceRequirements {
			return corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(q)}}
		}
		containers := []corev1.Container{
			{Name: "app", Image: "nginx:1.27", Resources: cpuLimit("2")},
			{Name: "sidecar", Image: "envoy:1", Resources: cpuLimit("100m")},
		}

		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: ns},
			Spec: dorguv1.ApplicationPersonaSpec{
				Name: app, Type: "api",
				Resources: &dorguv1.ResourceConstraints{Limits: &dorguv1.ResourceValues{CPU: "2", Memory: "256Mi"}},
			},
		}
		Expect(k8sClient.Create(bg, persona)).To(Succeed())
		replicas := int32(1)
		labels := map[string]string{"app.kubernetes.io/name": app}
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: ns, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: containers},
				},
			},
		}
		Expect(k8sClient.Create(bg, deploy)).To(Succeed())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: app + "-7d9f", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: containers},
		}
		Expect(k8sClient.Create(bg, pod)).To(Succeed())

		collector := detection.NewCPUThrottlingCollector(k8sClient, logr.Discard(),
			detection.WithMetricsSource(sidecarMetrics{ns: ns, pod: pod.Name}), detection.WithMinCycleSpacing(0))
		engine := detection.NewEngine(logr.Discard(), collector)
		engine.SetPersonaCorrelator(detection.NewPersonaCorrelator(k8sClient, logr.Discard()))
		rec := record.NewFakeRecorder(32)
		r := &HealthCheckReconciler{
			Client: k8sClient, Detection: engine, Logger: logr.Discard(), Recorder: rec,
			Diagnosis:    diagnosis.NewEngine(logr.Discard(), diagnosis.NewRuleBasedProvider(logr.Discard())),
			EventStore:   &noopEventStore{},
			EventEmitter: &noopEmitter{},
			Proposer:     remediation.NewProposer(k8sClient, envtestGate(logr.Discard()), logr.Discard()),
		}
		incidents := func() []dorguv1.IncidentMemory {
			var list dorguv1.IncidentMemoryList
			Expect(k8sClient.List(bg, &list, client.InNamespace(ns))).To(Succeed())
			return list.Items
		}
		actions := func() []dorguv1.RemediationAction {
			var list dorguv1.RemediationActionList
			Expect(k8sClient.List(bg, &list, client.InNamespace(ns))).To(Succeed())
			return list.Items
		}

		for range detection.CPUUsageConsecutiveCycles {
			r.reconcile(bg)
		}

		Expect(incidents()).To(HaveLen(1), "the throttled sidecar is an incident")
		Expect(incidents()[0].Spec.Detection.Signal).To(Equal("CPUThrottling"))
		Expect(incidents()[0].Spec.PersonaRef.Name).To(Equal(app))
		Expect(actions()).To(BeEmpty(), "and the main container's limit is not raised for it")

		events := drainEvents(rec)
		Expect(mismatchWarnings(events)).To(Equal(1), "events: %v", events)

		r.reconcile(bg)
		r.reconcile(bg)
		Expect(incidents()).To(HaveLen(1))
		Expect(actions()).To(BeEmpty())
		Expect(mismatchWarnings(drainEvents(rec))).To(BeZero(), "one Event, not one per cycle")
	})
})
