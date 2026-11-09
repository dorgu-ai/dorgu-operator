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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// B8 EKS QA F-3: a proposal made by a 0.12.x operator has no status.gate record,
// so approving it after the upgrade is refused (NoProposalRecord). That refusal
// ended the action Failed, which is a failure of the remediation. Nothing failed:
// the proposal predates the gate. It now ends Expired with the reason
// PredatesGate (terminal, nothing applied, no failure or rejection cooldown), and
// the operator proposes again.

func approvedWithoutAGateRecord() *dorguv1.RemediationAction {
	return lcRA(RemediationPhaseApproved, func(ra *dorguv1.RemediationAction) {
		ra.Status.Gate = nil
		ra.Status.Conditions = nil
	})
}

func TestApprovalOfAProposalThatPredatesTheGateExpiresIt(t *testing.T) {
	ra := approvedWithoutAGateRecord()
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()}, lcOOMSignal())

	_, got := lcReconcile(t, rc, ra)

	assert.Equal(t, RemediationPhaseExpired, got.Status.Phase, "a proposal that predates the gate is expired, not failed")
	expired := lcCondition(got, ConditionExpired)
	require.NotNil(t, expired)
	assert.Equal(t, metav1.ConditionTrue, expired.Status)
	assert.Equal(t, ReasonPredatesGate, expired.Reason)
	assert.Contains(t, expired.Message, "propose")
	assert.Contains(t, expired.Message, "Nothing was applied")

	// The refusal is still persisted as before (KPI I3): the enum on GateRefused and the decision on status.gate.
	refused := lcCondition(got, dorguv1.ConditionGateRefused)
	require.NotNil(t, refused)
	assert.Equal(t, "NoProposalRecord", refused.Reason)
	require.NotNil(t, got.Status.Gate)
	assert.Equal(t, "Refuse", got.Status.Gate.Result)

	assert.Nil(t, got.Status.ApplyTicket, "no ticket was issued")
	assert.Nil(t, got.Status.AppliedAt)
	assert.Equal(t, lcBaseline, lcPersonaMemory(t, c), "the persona was left alone")
	assert.Equal(t, "", lcIncidentOutcome(t, c), "the incident is not recorded as a failed remediation")
}

// An approval the gate refuses for any other reason is still a refusal, Failed as before.
func TestOtherApprovalRefusalsStayFailed(t *testing.T) {
	ra := lcRA(RemediationPhaseApproved, func(ra *dorguv1.RemediationAction) {
		ra.Generation = 3 // the spec was edited after the operator cleared it
	})
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), lcIncidentObj(), ra.DeepCopy()}, lcOOMSignal())

	_, got := lcReconcile(t, rc, ra)

	assert.Equal(t, RemediationPhaseFailed, got.Status.Phase)
}
