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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// TestProposer_PersonaMissingTheLimitTheWorkloadHas covers the drift running the
// other way: the workload sets a memory limit the imported persona never
// recorded. The fix is still sized off the live value, and the pre-patch
// snapshot records that live value so a rollback has something to restore.
func TestProposer_PersonaMissingTheLimitTheWorkloadHas(t *testing.T) {
	persona := personaWithLimits(groundedPersona, "", "")
	deploy := liveDeployment(groundedPersona,
		map[corev1.ResourceName]string{corev1.ResourceMemory: "64Mi"}, nil)
	incident := newTestIncident(defaultNamespace, "oom-no-persona-limit", groundedPersona, "OOMKilled")

	c := fake.NewClientBuilder().WithScheme(newWorkloadScheme()).
		WithRuntimeObjects(persona, deploy).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()

	p := NewProposer(c, NewSafetyChecker(c, testLogger()), testLogger())
	result, err := p.Propose(context.Background(),
		newOOMDiagnosis(defaultNamespace, groundedPersona, detection.SeverityCritical), incident)
	require.NoError(t, err)
	require.True(t, result.Proposed)

	action := result.Action
	assert.Equal(t, "128Mi", patchLimit(t, action.Spec.Action.Patch.Raw, "memory"))

	require.NotNil(t, action.Spec.Action.PrePatchState)
	assert.Equal(t, "64Mi", patchLimit(t, action.Spec.Action.PrePatchState.Raw, "memory"),
		"with no persona value to restore, the rollback target is the live value")

	var pre map[string]any
	require.NoError(t, json.Unmarshal(action.Spec.Action.PrePatchState.Raw, &pre))
	assert.NotEmpty(t, pre, "the executor refuses an action with an empty pre-patch state")
}
