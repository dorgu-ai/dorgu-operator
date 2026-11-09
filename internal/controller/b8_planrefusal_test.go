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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// B8 EKS QA F-5, KPI I3: when the AI plan was refused at Validate/ActionClass and
// its advisory steps were proposed instead, the refusal is still recorded on the
// incident (a Warning Event and the condition), and the proposal answers it.
func TestProposeRemediation_AFallbackProposalStillRecordsTheRefusalOfTheFirstPlan(t *testing.T) {
	action := &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "ra-1", Namespace: "shop"}}
	proposer := &stubProposer{result: &remediation.ProposalResult{
		Proposed: true, Action: action,
		PlanRefusal: &remediation.ProposalResult{
			Stage: gate.StageValidate, Reason: gate.GuardrailActionClass,
			SkipReason: "the plan changes the persona, but the rules did not recommend a resource adjustment for this diagnosis",
		},
	}}
	r := newHandoffReconciler(t, proposer)
	r.Client = fake.NewClientBuilder().WithScheme(r.Client.Scheme()).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).Build()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	diag, incident := handoffFixtures()
	require.NoError(t, r.Client.Create(context.Background(), incident))

	r.proposeRemediation(context.Background(), diag, incident)

	assert.True(t, hasEvent(drainEvents(rec), corev1.EventTypeWarning, "DorguValidateRefused"),
		"the refusal of the first plan is an Event, as every refusal is")
	var got dorguv1.IncidentMemory
	require.NoError(t, r.Client.Get(context.Background(), client.ObjectKeyFromObject(incident), &got))
	cond := findCondition(got.Status.Conditions, dorguv1.ConditionProposalRefused)
	require.NotNil(t, cond, "the refusal is on the incident")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "and the proposal that followed answers it")
	assert.Equal(t, dorguv1.ReasonProposalMade, cond.Reason)
}
