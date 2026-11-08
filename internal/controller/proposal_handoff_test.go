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
	"github.com/dorgu-ai/dorgu-operator/internal/websocket"
)

// recordingNotifier is a Notifier that remembers what it was told.
type recordingNotifier struct {
	incidents    []websocket.IncidentEvent
	remediations []websocket.RemediationEvent
	health       []websocket.HealthUpdateEvent
}

func (n *recordingNotifier) BroadcastIncident(e websocket.IncidentEvent) {
	n.incidents = append(n.incidents, e)
}

func (n *recordingNotifier) BroadcastRemediation(e websocket.RemediationEvent) {
	n.remediations = append(n.remediations, e)
}

func (n *recordingNotifier) BroadcastHealthUpdate(e websocket.HealthUpdateEvent) {
	n.health = append(n.health, e)
}

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

func newHandoffReconciler(t *testing.T, proposer remediation.RemediationProposer, n Notifier) *HealthCheckReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := dorguv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &HealthCheckReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).Build(),
		Proposer: proposer,
		Notifier: n,
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

func TestProposeRemediation_NotifiesWhenProposed(t *testing.T) {
	action := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "ra-1", Namespace: "shop"}}
	action.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "api"}
	action.Status.Phase = RemediationPhasePending
	proposer := &stubProposer{result: &remediation.ProposalResult{Proposed: true, Action: action}}
	n := &recordingNotifier{}
	r := newHandoffReconciler(t, proposer, n)

	diag, incident := handoffFixtures()
	r.proposeRemediation(context.Background(), diag, incident)

	if proposer.calls != 1 {
		t.Fatalf("proposer calls = %d, want 1", proposer.calls)
	}
	if len(n.remediations) != 1 {
		t.Fatalf("remediation events = %d, want 1", len(n.remediations))
	}
	got := n.remediations[0]
	if got.EventType != "created" || got.Name != "ra-1" || got.Namespace != "shop" ||
		got.Phase != RemediationPhasePending || got.PersonaName != "api" {
		t.Errorf("unexpected remediation event: %+v", got)
	}
}

func TestProposeRemediation_QuietWhenSkippedOrFailed(t *testing.T) {
	for name, proposer := range map[string]*stubProposer{
		"skipped": {result: &remediation.ProposalResult{SkipReason: "cooldown"}},
		"error":   {err: errors.New("boom")},
	} {
		t.Run(name, func(t *testing.T) {
			n := &recordingNotifier{}
			r := newHandoffReconciler(t, proposer, n)
			diag, incident := handoffFixtures()
			r.proposeRemediation(context.Background(), diag, incident)
			if proposer.calls != 1 {
				t.Fatalf("proposer calls = %d, want 1", proposer.calls)
			}
			if len(n.remediations) != 0 {
				t.Errorf("expected no remediation events, got %d", len(n.remediations))
			}
		})
	}
}

func TestProposeRemediation_NoProposerOrResolvedIncidentProposesNothing(t *testing.T) {
	diag, incident := handoffFixtures()

	// No proposer configured: must not panic.
	newHandoffReconciler(t, nil, nil).proposeRemediation(context.Background(), diag, incident)

	// Resolved incident: the proposer is never asked.
	proposer := &stubProposer{result: &remediation.ProposalResult{Proposed: true}}
	resolved := incident.DeepCopy()
	resolved.Status.Phase = PhaseResolved
	newHandoffReconciler(t, proposer, nil).proposeRemediation(context.Background(), diag, resolved)
	if proposer.calls != 0 {
		t.Errorf("proposer called %d times for a resolved incident, want 0", proposer.calls)
	}

	// Nil incident: same.
	newHandoffReconciler(t, proposer, nil).proposeRemediation(context.Background(), diag, nil)
	if proposer.calls != 0 {
		t.Errorf("proposer called %d times for a nil incident, want 0", proposer.calls)
	}
}

func TestNotifier_ExplicitWinsAndNilServerIsNotListening(t *testing.T) {
	r := &HealthCheckReconciler{}
	if r.notifier() != nil {
		t.Fatal("no Notifier and no WebSocket must mean nobody is listening")
	}

	// A nil *websocket.Server must not become a non-nil interface.
	r.WebSocket = nil
	if r.notifier() != nil {
		t.Fatal("nil WebSocket server must not be converted to a non-nil Notifier")
	}

	explicit := &recordingNotifier{}
	r.Notifier = explicit
	if r.notifier() != Notifier(explicit) {
		t.Fatal("explicit Notifier must be returned")
	}
}

func TestBroadcastHealthSummary_CountsOpenIncidentsAndPendingActions(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dorguv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	detected := &dorguv1.IncidentMemory{ObjectMeta: metav1.ObjectMeta{
		Name: "a", Namespace: "shop", Labels: map[string]string{LabelPhase: PhaseDetected}}}
	investigating := &dorguv1.IncidentMemory{ObjectMeta: metav1.ObjectMeta{
		Name: "b", Namespace: "shop", Labels: map[string]string{LabelPhase: PhaseInvestigating}}}
	resolved := &dorguv1.IncidentMemory{ObjectMeta: metav1.ObjectMeta{
		Name: "c", Namespace: "shop", Labels: map[string]string{LabelPhase: PhaseResolved}}}
	pending := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "shop"}}
	pending.Status.Phase = RemediationPhasePending
	approved := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "shop"}}
	approved.Status.Phase = "Approved"

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(detected, investigating, resolved, pending, approved).Build()
	n := &recordingNotifier{}
	r := &HealthCheckReconciler{Client: c, Notifier: n, Logger: logr.Discard()}

	r.broadcastHealthSummary(context.Background(), n)

	if len(n.health) != 1 {
		t.Fatalf("health events = %d, want 1", len(n.health))
	}
	if n.health[0].ActiveIncidents != 2 || n.health[0].PendingRemedies != 1 {
		t.Errorf("health = %+v, want 2 active incidents and 1 pending remedy", n.health[0])
	}
}
