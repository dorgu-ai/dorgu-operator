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
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// stubProposer returns a canned proposal result.
type stubProposer struct {
	result *remediation.ProposalResult
	err    error
	calls  int
}

func (p *stubProposer) Propose(
	_ context.Context, _ diagnosis.Diagnosis, _ *dorguv1.IncidentMemory,
) (*remediation.ProposalResult, error) {
	p.calls++
	return p.result, p.err
}

func newHandoffReconciler(t *testing.T, proposer remediation.RemediationProposer) *HealthCheckReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := dorguv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &HealthCheckReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).Build(),
		Proposer: proposer,
		Logger:   logr.Discard(),
	}
}

func handoffFixtures() (*diagnosis.Diagnosis, *dorguv1.IncidentMemory) {
	diag := &diagnosis.Diagnosis{
		SuggestedAction: "resource-adjustment",
		PersonaRef:      &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "api", Namespace: "shop"},
	}
	incident := &dorguv1.IncidentMemory{ObjectMeta: metav1.ObjectMeta{Name: "im-1", Namespace: "shop"}}
	incident.Status.Phase = PhaseDetected
	return diag, incident
}

func TestProposeRemediation_HandsTheIncidentToTheProposer(t *testing.T) {
	action := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "ra-1", Namespace: "shop"}}
	proposer := &stubProposer{result: &remediation.ProposalResult{Proposed: true, Action: action}}
	r := newHandoffReconciler(t, proposer)

	diag, incident := handoffFixtures()
	r.proposeRemediation(context.Background(), diag, incident)

	if proposer.calls != 1 {
		t.Fatalf("proposer calls = %d, want 1", proposer.calls)
	}
}

func TestProposeRemediation_QuietWhenSkippedOrFailed(t *testing.T) {
	for name, proposer := range map[string]*stubProposer{
		"skipped": {result: &remediation.ProposalResult{SkipReason: "cooldown"}},
		"error":   {err: errors.New("boom")},
	} {
		t.Run(name, func(t *testing.T) {
			r := newHandoffReconciler(t, proposer)
			diag, incident := handoffFixtures()
			r.proposeRemediation(context.Background(), diag, incident)
			if proposer.calls != 1 {
				t.Fatalf("proposer calls = %d, want 1", proposer.calls)
			}
		})
	}
}

func TestProposeRemediation_NoProposerOrResolvedIncidentProposesNothing(t *testing.T) {
	diag, incident := handoffFixtures()

	// No proposer configured: must not panic.
	newHandoffReconciler(t, nil).proposeRemediation(context.Background(), diag, incident)

	// Resolved incident: the proposer is never asked.
	proposer := &stubProposer{result: &remediation.ProposalResult{Proposed: true}}
	resolved := incident.DeepCopy()
	resolved.Status.Phase = PhaseResolved
	newHandoffReconciler(t, proposer).proposeRemediation(context.Background(), diag, resolved)
	if proposer.calls != 0 {
		t.Errorf("proposer called %d times for a resolved incident, want 0", proposer.calls)
	}

	// Nil incident: same.
	newHandoffReconciler(t, proposer).proposeRemediation(context.Background(), diag, nil)
	if proposer.calls != 0 {
		t.Errorf("proposer called %d times for a nil incident, want 0", proposer.calls)
	}
}
