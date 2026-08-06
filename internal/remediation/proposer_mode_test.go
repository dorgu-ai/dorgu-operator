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

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// newTestClusterPersona builds a ClusterPersona carrying a selfHealing mode.
func newTestClusterPersona(name, mode string) *dorguv1.ClusterPersona {
	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: dorguv1.ClusterPersonaSpec{
			Name:        name,
			Environment: "development",
			Policies: &dorguv1.ClusterPolicies{
				SelfHealing: &dorguv1.SelfHealingPolicy{
					Enabled:    true,
					Mode:       mode,
					TrustLevel: 2,
				},
			},
		},
	}
}

// capturingLogger returns a logger that appends every message to *sink, so tests
// can assert on the skip/warning lines the mode gate is required to emit.
func capturingLogger(sink *[]string) logr.Logger {
	return funcr.New(func(prefix, args string) {
		*sink = append(*sink, prefix+" "+args)
	}, funcr.Options{Verbosity: 1})
}

// countingPlanner records how many times the AI planner was asked for a plan.
type countingPlanner struct {
	calls int
}

func (p *countingPlanner) PlanRemediation(_ context.Context, _ planner.RemediationContext) (*planner.RemediationPlan, error) {
	p.calls++
	return nil, errors.New("countingPlanner should never be called")
}

// logsContain reports whether any captured line contains substr.
func logsContain(logs []string, substr string) bool {
	for _, line := range logs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// TestProposer_SelfHealingModeGate is the contract for spec.policies.selfHealing.mode:
// observe records the incident and proposes nothing, propose keeps the historic
// behavior, and auto-approve is not implemented so it degrades to propose loudly
// rather than silently auto-approving.
func TestProposer_SelfHealingModeGate(t *testing.T) {
	tests := []struct {
		name          string
		clusterMode   string // "" means: create no ClusterPersona at all
		wantProposed  bool
		wantSkip      string
		wantLogSubstr string
	}{
		{
			name:          "observe_creates_no_remediation_action",
			clusterMode:   dorguv1.SelfHealingModeObserve,
			wantProposed:  false,
			wantSkip:      skipReasonObserveMode,
			wantLogSubstr: skipReasonObserveMode,
		},
		{
			name:         "propose_creates_a_remediation_action",
			clusterMode:  dorguv1.SelfHealingModePropose,
			wantProposed: true,
		},
		{
			name:          "auto_approve_is_not_implemented_and_falls_back_to_propose",
			clusterMode:   dorguv1.SelfHealingModeAutoApprove,
			wantProposed:  true,
			wantLogSubstr: "not implemented",
		},
		{
			name:          "unrecognized_mode_falls_back_to_propose",
			clusterMode:   "yolo",
			wantProposed:  true,
			wantLogSubstr: "unrecognized selfHealing.mode",
		},
		{
			name:         "no_cluster_persona_falls_back_to_propose",
			clusterMode:  "",
			wantProposed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			persona := newTestPersona("default", "my-app", "256Mi", "500m")
			incident := newTestIncident("default", "mode-test", "my-app", "OOMKilled")

			objects := []runtime.Object{persona}
			if tt.clusterMode != "" {
				objects = append(objects, newTestClusterPersona("dorgu-cluster", tt.clusterMode))
			}

			c := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(objects...).
				WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

			var logs []string
			logger := capturingLogger(&logs)
			proposer := NewProposer(c, NewSafetyChecker(c, logger), logger)

			diag := newOOMDiagnosis("default", "my-app", detection.SeverityCritical)
			result, err := proposer.Propose(context.Background(), diag, incident)
			require.NoError(t, err)

			assert.Equal(t, tt.wantProposed, result.Proposed)
			if tt.wantSkip != "" {
				assert.Equal(t, tt.wantSkip, result.SkipReason)
			}

			// The cluster is the source of truth: assert on what actually exists,
			// not just on the returned result.
			var actions dorguv1.RemediationActionList
			require.NoError(t, c.List(context.Background(), &actions))
			if tt.wantProposed {
				assert.Len(t, actions.Items, 1, "propose must create exactly one RemediationAction")
				assert.True(t, actions.Items[0].Spec.Approval.Required,
					"approval stays required in every mode — auto-approve is not implemented")
			} else {
				assert.Empty(t, actions.Items, "observe must create zero RemediationActions")
				assert.Nil(t, result.Action)
			}

			if tt.wantLogSubstr != "" {
				assert.True(t, logsContain(logs, tt.wantLogSubstr),
					"expected a log line containing %q, got %v", tt.wantLogSubstr, logs)
			}
		})
	}
}

// TestProposer_ObserveModeSkipsBeforePlanner proves observe short-circuits ahead of
// the AI planner: no key is spent, and no plan is requested, for a mode whose whole
// point is to do nothing.
func TestProposer_ObserveModeSkipsBeforePlanner(t *testing.T) {
	scheme := newTestScheme()
	persona := newTestPersona("default", "my-app", "256Mi", "500m")
	incident := newTestIncident("default", "observe-planner", "my-app", "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(persona, newTestClusterPersona("dorgu-cluster", dorguv1.SelfHealingModeObserve)).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	aiPlanner := &countingPlanner{}
	proposer := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger(), WithPlanner(aiPlanner))

	diag := newOOMDiagnosis("default", "my-app", detection.SeverityCritical)
	result, err := proposer.Propose(context.Background(), diag, incident)

	require.NoError(t, err)
	assert.False(t, result.Proposed)
	assert.Equal(t, skipReasonObserveMode, result.SkipReason)
	assert.Zero(t, aiPlanner.calls, "observe must not invoke the AI planner")
}

// TestProposer_SelfHealingMode covers the ClusterPersona lookup in isolation,
// including personas that declare no mode at all.
func TestProposer_SelfHealingMode(t *testing.T) {
	tests := []struct {
		name     string
		personas []runtime.Object
		want     string
	}{
		{
			name: "no_cluster_persona",
			want: dorguv1.SelfHealingModePropose,
		},
		{
			name:     "persona_with_observe",
			personas: []runtime.Object{newTestClusterPersona("dorgu-cluster", dorguv1.SelfHealingModeObserve)},
			want:     dorguv1.SelfHealingModeObserve,
		},
		{
			name: "persona_without_policies_falls_back",
			personas: []runtime.Object{&dorguv1.ClusterPersona{
				ObjectMeta: metav1.ObjectMeta{Name: "bare"},
				Spec:       dorguv1.ClusterPersonaSpec{Name: "bare", Environment: "development"},
			}},
			want: dorguv1.SelfHealingModePropose,
		},
		{
			name:     "persona_with_empty_mode_falls_back",
			personas: []runtime.Object{newTestClusterPersona("dorgu-cluster", "")},
			want:     dorguv1.SelfHealingModePropose,
		},
		{
			name: "first_persona_declaring_a_mode_wins",
			personas: []runtime.Object{
				newTestClusterPersona("a-no-mode", ""),
				newTestClusterPersona("b-observe", dorguv1.SelfHealingModeObserve),
			},
			want: dorguv1.SelfHealingModeObserve,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newTestScheme()).
				WithRuntimeObjects(tt.personas...).Build()
			proposer := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger())

			assert.Equal(t, tt.want, proposer.selfHealingMode(context.Background()))
		})
	}
}
