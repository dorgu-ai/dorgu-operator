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
	"strings"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// envtestMetrics is metrics.k8s.io for the envtest world: there is no metrics-server
// in envtest, so usage is whatever the spec says it is.
type envtestMetrics struct {
	present bool
	usage   string
	ns, pod string
}

func (m *envtestMetrics) Available(context.Context) (bool, error) { return m.present, nil }

func (m *envtestMetrics) PodMetrics(context.Context) ([]metricsv1beta1.PodMetrics, error) {
	return []metricsv1beta1.PodMetrics{{
		ObjectMeta: metav1.ObjectMeta{Name: m.pod, Namespace: m.ns},
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "app", Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(m.usage)},
		}},
	}}, nil
}

var _ = Describe("CPU throttling on a real API server (C4)", func() {
	const ns = "c4-cpu-throttling"
	bg := context.Background()

	BeforeEach(func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(bg, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))).To(Succeed())
	})

	Context("ClusterPersona spec.metrics.prometheus.url", func() {
		DescribeTable("is validated by the CRD itself",
			func(name, url string, accepted bool) {
				cp := &dorguv1.ClusterPersona{
					ObjectMeta: metav1.ObjectMeta{Name: name},
					Spec: dorguv1.ClusterPersonaSpec{
						Name:    name,
						Metrics: &dorguv1.ClusterMetricsSpec{Prometheus: &dorguv1.PrometheusSpec{URL: url}},
					},
				}
				err := k8sClient.Create(bg, cp)
				DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(bg, cp)) })
				if accepted {
					Expect(err).NotTo(HaveOccurred())
				} else {
					Expect(err).To(HaveOccurred(), "the API server must refuse %q", url)
				}
			},
			Entry("http", "c4-url-http", "http://prometheus-server.monitoring:9090", true),
			Entry("https with a path", "c4-url-https", "https://prometheus.example.com/prom", true),
			Entry("ftp", "c4-url-ftp", "ftp://prometheus:21", false),
			Entry("file", "c4-url-file", "file:///etc/passwd", false),
			Entry("no scheme", "c4-url-bare", "prometheus:9090", false),
			Entry("credentials", "c4-url-creds", "http://u:p@prometheus:9090", false),
			Entry("a query string", "c4-url-query", "http://prometheus:9090/?q=1", false),
			Entry("whitespace", "c4-url-space", "http://prom etheus:9090", false),
			Entry("over-long", "c4-url-long", "http://"+strings.Repeat("a", 2100), false),
		)

		It("is optional", func() {
			cp := &dorguv1.ClusterPersona{ObjectMeta: metav1.ObjectMeta{Name: "c4-no-metrics"}, Spec: dorguv1.ClusterPersonaSpec{Name: "c4"}}
			Expect(k8sClient.Create(bg, cp)).To(Succeed())
			DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(bg, cp)) })
		})
	})

	Context("the status the CLI reads", func() {
		It("is written to the ClusterPersona and survives a read back", func() {
			cp := &dorguv1.ClusterPersona{ObjectMeta: metav1.ObjectMeta{Name: "c4-status"}, Spec: dorguv1.ClusterPersonaSpec{Name: "c4"}}
			Expect(k8sClient.Create(bg, cp)).To(Succeed())
			DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(bg, cp)) })

			NewCPUThrottlingStatusSink(k8sClient, logr.Discard()).ReportCPUThrottlingSource(bg, detection.SourceStatus{
				Active: false, Source: detection.CPUThrottlingSourceNone, Reason: detection.ReasonNoMetricsSource,
				Message: "CPU throttling detection is off: no metrics source",
			})

			var got dorguv1.ClusterPersona
			Expect(k8sClient.Get(bg, client.ObjectKeyFromObject(cp), &got)).To(Succeed())
			Expect(got.Status.CPUThrottlingDetection).NotTo(BeNil())
			Expect(got.Status.CPUThrottlingDetection.Enabled).To(BeFalse())
			Expect(got.Status.CPUThrottlingDetection.Reason).To(Equal("NoMetricsSource"))
			Expect(apimeta.IsStatusConditionFalse(got.Status.Conditions, ConditionCPUThrottlingDetection)).To(BeTrue())
		})

		It("refuses a source name outside the enum", func() {
			cp := &dorguv1.ClusterPersona{ObjectMeta: metav1.ObjectMeta{Name: "c4-status-enum"}, Spec: dorguv1.ClusterPersonaSpec{Name: "c4"}}
			Expect(k8sClient.Create(bg, cp)).To(Succeed())
			DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(bg, cp)) })
			cp.Status.CPUThrottlingDetection = &dorguv1.CPUThrottlingDetectionStatus{Source: "kubelet-proxy"}
			Expect(k8sClient.Status().Update(bg, cp)).NotTo(Succeed())
		})
	})

	Context("the incident path", func() {
		It("turns a container at its limit for three cycles into one incident and one CPU-limit proposal", func() {
			const app = "c4-api"
			persona := &dorguv1.ApplicationPersona{
				ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: ns},
				Spec: dorguv1.ApplicationPersonaSpec{
					Name:      app,
					Type:      "api",
					Resources: &dorguv1.ResourceConstraints{Limits: &dorguv1.ResourceValues{CPU: "500m", Memory: "256Mi"}},
				},
			}
			Expect(k8sClient.Create(bg, persona)).To(Succeed())
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: app + "-7d9f", Namespace: ns},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app", Image: "nginx:1.27",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}},
				}}},
			}
			Expect(k8sClient.Create(bg, pod)).To(Succeed())

			metrics := &envtestMetrics{present: true, usage: "490m", ns: ns, pod: pod.Name}
			collector := detection.NewCPUThrottlingCollector(k8sClient, logr.Discard(),
				detection.WithMetricsSource(metrics), detection.WithMinCycleSpacing(0))
			engine := detection.NewEngine(logr.Discard(), collector)
			engine.SetPersonaCorrelator(detection.NewPersonaCorrelator(k8sClient, logr.Discard()))
			r := &HealthCheckReconciler{
				Client: k8sClient, Detection: engine, Logger: logr.Discard(),
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

			r.reconcile(bg)
			r.reconcile(bg)
			Expect(incidents()).To(BeEmpty(), "two cycles at the limit is not yet throttling")

			r.reconcile(bg)
			Expect(incidents()).To(HaveLen(1))
			im := incidents()[0]
			Expect(im.Spec.Detection.Signal).To(Equal("CPUThrottling"))
			Expect(im.Annotations[AnnotationContainers]).To(Equal("app"))
			Expect(im.Spec.PersonaRef.Name).To(Equal(app))

			Expect(actions()).To(HaveLen(1))
			ra := actions()[0]
			Expect(ra.Status.Phase).To(Equal(RemediationPhasePending))
			Expect(ra.Status.Gate).NotTo(BeNil(), "the gate ruled on it")
			Expect(ra.Spec.Steps).To(HaveLen(1))
			Expect(string(ra.Spec.Steps[0].Patch.Raw)).To(ContainSubstring(`"cpu":"625m"`))

			// More cycles: the same fault is not a new incident or a new proposal.
			r.reconcile(bg)
			r.reconcile(bg)
			Expect(incidents()).To(HaveLen(1))
			Expect(actions()).To(HaveLen(1))

			// The metrics API was served and now is not. That is a collector that could not
			// look (B2b), scoped to its family: the engine does not call the whole cycle
			// incomplete, it says CPUThrottling is blind, and nothing about it is closed on
			// the silence. The incident is kept open and nothing new is invented.
			metrics.present = false
			_, collectErr := engine.CollectAll(bg)
			Expect(collectErr).NotTo(HaveOccurred(), "a CPU source outage is not a whole-cycle failure")
			Expect(engine.SignalBlind(detection.SignalCPUThrottling)).To(BeTrue(), "the lost source blinds its family")
			Expect(engine.SignalBlind(detection.SignalOOMKilled)).To(BeFalse(), "and no other")
			r.reconcile(bg)
			Expect(incidents()).To(HaveLen(1))
			Expect(incidents()[0].Status.Phase).NotTo(Equal(PhaseResolved))
			Expect(actions()).To(HaveLen(1))
		})
	})
})
