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

package remediation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

// evictionDiagnosis is the node-pressure shape the rules produce: one diagnosis
// merging signals from pods in several namespaces, whose PersonaRef is simply the
// first persona found. The persona is in an allowed namespace; one evicted pod is
// in dorgu-system.
func evictionDiagnosis(personaNamespace, otherPodNamespace string) diagnosis.Diagnosis {
	pod := func(ns, name string) detection.Signal {
		return detection.Signal{
			Type:       detection.SignalPodEvicted,
			Severity:   detection.SeverityWarning,
			Category:   detection.CategoryNode,
			Message:    "Pod " + ns + "/" + name + " was evicted due to node memory pressure",
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: name, Namespace: ns},
			DetectedAt: time.Now(),
		}
	}
	node := detection.Signal{
		Type:       detection.SignalNodeMemoryPressure,
		Severity:   detection.SeverityCritical,
		Category:   detection.CategoryNode,
		Message:    "Node ip-10-0-0-1 has memory pressure",
		Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "ip-10-0-0-1"},
		DetectedAt: time.Now(),
	}
	signals := []detection.Signal{node, pod(personaNamespace, "my-app-abc"), pod(otherPodNamespace, "victim-xyz")}

	d := newOOMDiagnosis(personaNamespace, "my-app", detection.SeverityCritical)
	d.Summary = "Node under pressure (NodeMemoryPressure), causing pod evictions."
	d.Category = string(detection.CategoryNode)
	d.Contributing = nil
	d.AffectedResources = nil
	for _, s := range signals {
		d.Contributing = append(d.Contributing, diagnosis.ContributingSignal{Signal: s})
		d.AffectedResources = append(d.AffectedResources, s.Resource)
	}
	return d
}

// A diagnosis whose PersonaRef is allowed but which merges in a pod from a
// protected namespace must not reach the planner: the planner prompt carries the
// summary and every contributing signal's message, namespace and pod name
// included.
func TestProposer_PlannerNeverSeesAProtectedNamespaceInAMixedDiagnosis(t *testing.T) {
	tests := []struct {
		name      string
		other     string
		wantCalls int
	}{
		{"a pod from the operator's namespace", "dorgu-system", 0},
		{"a pod from kube-system", "kube-system", 0},
		{"every namespace allowed reaches the planner as before", "other-team", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			persona := newTestPersona("apps", "my-app")
			incident := newTestIncident("apps", "evict", "my-app", "PodEvicted")
			c := fake.NewClientBuilder().WithScheme(newTestScheme()).
				WithRuntimeObjects(persona).
				WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

			model := &countingPlanner{}
			p := NewProposer(c, NewSafetyChecker(c, testLogger(), WithProtectedNamespaces("dorgu-system")),
				testLogger(), WithPlanner(model))

			_, err := p.Propose(context.Background(), evictionDiagnosis("apps", tt.other), incident)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCalls, model.calls)
		})
	}
}

// The deny-list answers for the planner path the same way it answers for the
// diagnosis path: it is one predicate over the same namespaces.
func TestProposer_WithheldDiagnosisStillGetsTheRuleBasedProposal(t *testing.T) {
	persona := newTestPersona("apps", "my-app")
	incident := newTestIncident("apps", "oom", "my-app", "OOMKilled")
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(persona).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	model := &countingPlanner{}
	p := NewProposer(c, NewSafetyChecker(c, testLogger(), WithProtectedNamespaces("dorgu-system")),
		testLogger(), WithPlanner(model))

	diag := newOOMDiagnosis("apps", "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Resource = dorguv1.ResourceReference{Kind: "Pod", Name: "x", Namespace: "dorgu-system"}

	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	assert.Zero(t, model.calls)
	assert.True(t, result.Proposed, "withholding the diagnosis from the model must not stop the rule-based path: %s", result.SkipReason)
}

func TestSafetyChecker_NilReceiverFailsClosed(t *testing.T) {
	var checker *SafetyCheckerImpl
	assert.NotPanics(t, func() {
		assert.True(t, checker.NamespaceProtected(context.Background(), "apps"))
		assert.NotNil(t, checker.NamespaceDenied(context.Background(), "apps"))
	})
	// The shape a typed nil takes inside the interface the provider holds.
	var gate diagnosis.NamespaceGate = checker
	assert.NotPanics(t, func() { assert.True(t, gate.NamespaceProtected(context.Background(), "apps")) })
}

// A persistent failure to read the deny-list turns AI off for every namespace.
// It has to be visible at the default log level and say why in the skip reason.
func TestProposer_UnreadableDenyListIsLoudAndExplained(t *testing.T) {
	persona := newTestPersona("apps", "my-app")
	incident := newTestIncident("apps", "oom", "my-app", "OOMKilled")
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(persona).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
				if _, ok := l.(*dorguv1.ClusterPersonaList); ok {
					return errors.New("clusterpersonas is forbidden")
				}
				return cl.List(ctx, l, opts...)
			},
		}).Build()

	var logs []string
	p := NewProposer(c, NewSafetyChecker(c, capturingLogger(&logs)), testLogger())

	result, err := p.Propose(context.Background(), newOOMDiagnosis("apps", "my-app", detection.SeverityCritical), incident)
	require.NoError(t, err)
	assert.False(t, result.Proposed)
	// The raw API error stays in the log: the skip reason may be persisted or shown.
	assert.NotContains(t, result.SkipReason, "clusterpersonas is forbidden")
	assert.Contains(t, result.SkipReason, "deny-list could not be read, treating namespace as protected")

	var loud bool
	for _, line := range logs {
		if strings.Contains(line, "clusterpersonas is forbidden") && !strings.Contains(line, `"level"=1`) {
			loud = true
		}
	}
	assert.True(t, loud, "the read failure must be logged above V(1); logs: %v", logs)
}
