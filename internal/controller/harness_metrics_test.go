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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// counterValue reads one series of a dorgu_* counter from the registry the
// operator serves, or 0 if the series does not exist yet.
func counterValue(name string, want map[string]string) float64 {
	families, err := ctrlmetrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatch(m.GetLabel(), want) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(pairs) != len(want) {
		return false
	}
	for _, l := range pairs {
		if want[l.GetName()] != l.GetValue() {
			return false
		}
	}
	return true
}

func transitionCount(from, to string) float64 {
	return counterValue("dorgu_remediation_transitions_total", map[string]string{"from": from, "to": to})
}

var _ = Describe("Harness metrics wiring", func() {
	It("counts the phase transitions the RemediationController writes", func() {
		testCtx := context.Background()
		const ns = "default"
		log := zap.New(zap.UseDevMode(true))

		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: "metrics-persona", Namespace: ns},
			Spec: dorguv1.ApplicationPersonaSpec{
				Name: "metrics-persona", Type: "api",
				Resources: &dorguv1.ResourceConstraints{Limits: &dorguv1.ResourceValues{Memory: "256Mi"}},
			},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())

		ra := &dorguv1.RemediationAction{
			ObjectMeta: metav1.ObjectMeta{Name: "metrics-ra", Namespace: ns},
			Spec: dorguv1.RemediationActionSpec{
				IncidentRef: dorguv1.IncidentReference{Name: "metrics-incident", Namespace: ns},
				PersonaRef:  dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: persona.Name, Namespace: ns},
				TrustLevel:  2, Confidence: "0.85", Explanation: "raise memory",
				Action: dorguv1.RemediationActionDetail{
					Type:          "persona-update",
					Patch:         &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
					PrePatchState: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, ra)).To(Succeed())
		ra.Status.Phase = RemediationPhaseApproved
		Expect(k8sClient.Status().Update(testCtx, ra)).To(Succeed())

		rc := &RemediationController{
			Client:   k8sClient,
			Executor: remediation.NewExecutor(k8sClient, log),
			Verifier: remediation.NewVerifier(detection.NewEngine(log), k8sClient, log),
			Rollback: remediation.NewRollback(k8sClient, log),
			Logger:   log,
		}

		before := transitionCount(RemediationPhaseApproved, RemediationPhaseApplying)
		_, err := rc.Reconcile(testCtx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
		Expect(err).NotTo(HaveOccurred())
		Expect(transitionCount(RemediationPhaseApproved, RemediationPhaseApplying) - before).To(Equal(1.0))

		// A pass that changes nothing counts nothing.
		before = transitionCount(RemediationPhaseApplying, RemediationPhaseVerifying)
		_, err = rc.Reconcile(testCtx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
		Expect(err).NotTo(HaveOccurred())
		Expect(transitionCount(RemediationPhaseApplying, RemediationPhaseVerifying) - before).To(Equal(0.0))
	})

	It("counts the incidents it opens and the proposals it asks for", func() {
		testCtx := context.Background()
		log := zap.New(zap.UseDevMode(true))
		persona := &dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: "metrics-hc-app", Namespace: "default"},
			Spec:       dorguv1.ApplicationPersonaSpec{Name: "metrics-hc-app", Type: "api"},
		}
		Expect(k8sClient.Create(testCtx, persona)).To(Succeed())
		ref := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: persona.Name, Namespace: "default"}

		collector := &stubCollector{name: "metrics", signals: []detection.Signal{{
			Type: detection.SignalOOMKilled, Severity: detection.SeverityCritical,
			Category: detection.CategoryResource, Source: "metrics", Message: "OOM",
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "metrics-hc-app-abc", Namespace: "default"},
			PersonaRef: ref, DetectedAt: time.Now(),
		}}}
		reconciler := &HealthCheckReconciler{
			Client:            k8sClient,
			Detection:         detection.NewEngine(log, collector),
			Diagnosis:         diagnosis.NewEngine(log, diagnosis.NewRuleBasedProvider(log)),
			EventStore:        &noopEventStore{},
			EventEmitter:      &noopEmitter{},
			Proposer:          &countingProposer{},
			Logger:            log,
			ReconcileInterval: time.Second,
		}

		incidents := map[string]string{"class": "resource", "scope": AttributionPersona}
		skipped := map[string]string{
			"source": metrics.SourceDorgu, "action_type": "resource-adjustment", "result": metrics.ProposalSkipped,
		}
		incidentsBefore := counterValue("dorgu_incidents_total", incidents)
		skippedBefore := counterValue("dorgu_proposals_total", skipped)

		reconciler.reconcile(testCtx)

		Expect(counterValue("dorgu_incidents_total", incidents) - incidentsBefore).To(Equal(1.0))
		Expect(counterValue("dorgu_proposals_total", skipped) - skippedBefore).To(Equal(1.0))

		// The next cycle updates the same incident: no new incident is counted.
		incidentsBefore = counterValue("dorgu_incidents_total", incidents)
		reconciler.reconcile(testCtx)
		Expect(counterValue("dorgu_incidents_total", incidents) - incidentsBefore).To(Equal(0.0))
	})
})

var _ = Describe("proposalOutcome", func() {
	It("classifies every Propose result", func() {
		Expect(proposalOutcome(nil, errors.New("boom"))).To(Equal(metrics.ProposalError))
		Expect(proposalOutcome(&remediation.ProposalResult{Proposed: true}, nil)).To(Equal(metrics.ProposalProposed))
		Expect(proposalOutcome(&remediation.ProposalResult{SkipReason: "guardrail"}, nil)).To(Equal(metrics.ProposalSkipped))
	})
})
