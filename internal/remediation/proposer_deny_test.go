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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// A protected namespace is refused before the planner runs. EKS QA 2026-10-24
// (H-2) found the planner called first, so every incident in the operator's
// own namespace cost a model call per cycle and sent that namespace's context
// to the provider, only for the deny-list to throw the plan away afterwards.
func TestProposer_DenyListRefusesBeforeThePlannerRuns(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		options   []SafetyOption
	}{
		{"kube-system is always protected", "kube-system", nil},
		{"the operator's own namespace", "dorgu-system", []SafetyOption{WithProtectedNamespaces("dorgu-system")}},
		{"an installer-protected namespace", "payments", []SafetyOption{WithProtectedNamespaces("payments")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			persona := newTestPersona(tt.namespace, "my-app")
			incident := newTestIncident(tt.namespace, "oom", "my-app", "OOMKilled")
			c := fake.NewClientBuilder().WithScheme(newTestScheme()).
				WithRuntimeObjects(persona).
				WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

			model := &countingPlanner{}
			p := NewProposer(c, NewSafetyChecker(c, testLogger(), tt.options...), testLogger(), WithPlanner(model))

			diag := newOOMDiagnosis(tt.namespace, "my-app", detection.SeverityCritical)
			result, err := p.Propose(context.Background(), diag, incident)
			require.NoError(t, err)

			assert.False(t, result.Proposed)
			assert.Contains(t, result.SkipReason, "protected deny list")
			assert.Zero(t, model.calls, "the planner must not be called for a protected namespace")

			var actions dorguv1.RemediationActionList
			require.NoError(t, c.List(context.Background(), &actions))
			assert.Empty(t, actions.Items)
		})
	}
}
