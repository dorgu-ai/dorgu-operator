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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// F-9: a model reply with more steps than spec.steps allows is a gate refusal with a
// reason (Validate/too-many-steps), reported and counted like any refusal, and the
// model is asked once. It used to pass the gate and then fail client.Create on every
// cycle with nothing recorded.
func TestF9_AnAIPlanOverTheStepCapIsARefusalNotADroppedCreate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	plan := &planner.RemediationPlan{RootCause: "many things", Confidence: 0.7}
	for i := 1; i <= dorguv1.MaxPlanSteps+1; i++ {
		plan.Steps = append(plan.Steps, planner.PlannedStep{
			Order: int32(i), Type: dorguv1.StepTypeManual, Description: fmt.Sprintf("Look at thing %d.", i), Rationale: "because", Risk: "low",
		})
	}
	cp := &planCounter{plan: plan}
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(cp))
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalCrashLoopBackOff
	diag.SuggestedAction = rulesInvestigate

	result, err := p.Propose(context.Background(), diag, newTestIncident(defaultNamespace, "crash", "my-app", "CrashLoopBackOff"))
	require.NoError(t, err)

	assert.False(t, result.Proposed)
	assert.Equal(t, gate.StageValidate, result.Stage)
	assert.Equal(t, gate.GuardrailTooManySteps, result.Reason)
	assert.NotEmpty(t, result.SkipReason)

	var list dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Empty(t, list.Items, "nothing is created for a plan the CRD cannot hold")
}
