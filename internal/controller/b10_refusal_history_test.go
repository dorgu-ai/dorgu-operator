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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// B10 (f), D1.1 finding 4: `dorgu report` missed 3 Validate/step-command refusals because
// ProposalRefused is the live state and is rewritten to False when the incident resolves
// or a proposal answers it. The refusal also stays on the incident as
// ProposalRefusalRecorded, which nothing clears, so a report can count a refusal that
// happened.

func recordedRefusal(t *testing.T, r *HealthCheckReconciler, incident *dorguv1.IncidentMemory) *metav1.Condition {
	t.Helper()
	var got dorguv1.IncidentMemory
	require.NoError(t, r.Client.Get(context.Background(), client.ObjectKeyFromObject(incident), &got))
	return findCondition(got.Status.Conditions, dorguv1.ConditionProposalRefusalRecorded)
}

func TestRefusalHistory_ARefusalIsRecordedBesideTheLiveCondition(t *testing.T) {
	r, _, incident := handoffWithIncident(t, refusedProposal(gate.StageValidate, gate.GuardrailStepCommand))
	diag, _ := handoffFixtures()

	r.proposeRemediation(context.Background(), diag, incident)

	rec := recordedRefusal(t, r, incident)
	require.NotNil(t, rec)
	assert.Equal(t, metav1.ConditionTrue, rec.Status)
	assert.Equal(t, "Validate_StepCommand", rec.Reason, "the same enum as ProposalRefused: stage and guardrail, never message text")
	assert.NotContains(t, rec.Message, "kubectl", "no model text, no command")
	live := incidentCondition(t, r, incident)
	require.NotNil(t, live)
	assert.Equal(t, rec.Reason, live.Reason)
}

func TestRefusalHistory_SurvivesTheIncidentResolving(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	im.Status.Conditions = []metav1.Condition{
		{Type: dorguv1.ConditionProposalRefused, Status: metav1.ConditionTrue, Reason: "Validate_StepCommand", LastTransitionTime: metav1.Now()},
		{Type: dorguv1.ConditionProposalRefusalRecorded, Status: metav1.ConditionTrue, Reason: "Validate_StepCommand", LastTransitionTime: metav1.Now()},
	}
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 0))

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	var got dorguv1.IncidentMemory
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
	live := findCondition(got.Status.Conditions, dorguv1.ConditionProposalRefused)
	require.NotNil(t, live)
	assert.Equal(t, metav1.ConditionFalse, live.Status, "the live condition is cleared, as before")
	rec := findCondition(got.Status.Conditions, dorguv1.ConditionProposalRefusalRecorded)
	require.NotNil(t, rec, "the record stays")
	assert.Equal(t, metav1.ConditionTrue, rec.Status)
	assert.Equal(t, "Validate_StepCommand", rec.Reason)
}

func TestRefusalHistory_SurvivesAProposalAnsweringTheRefusal(t *testing.T) {
	proposer := refusedProposal(gate.StageValidate, gate.GuardrailStepCommand)
	r, _, incident := handoffWithIncident(t, proposer)
	diag, _ := handoffFixtures()
	r.proposeRemediation(context.Background(), diag, incident)

	proposer.result = &remediation.ProposalResult{Proposed: true, Action: &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: "ra-1", Namespace: "shop"}}}
	r.proposeRemediation(context.Background(), diag, incident)

	assert.Equal(t, metav1.ConditionFalse, incidentCondition(t, r, incident).Status)
	rec := recordedRefusal(t, r, incident)
	require.NotNil(t, rec)
	assert.Equal(t, metav1.ConditionTrue, rec.Status)
}

// The same refusal on every cycle is one record, and its time is when it was first made.
func TestRefusalHistory_ARepeatedRefusalIsRecordedOnce(t *testing.T) {
	r, _, incident := handoffWithIncident(t, refusedProposal(gate.StageModePolicy, gate.GuardrailSelfHealingMode))
	diag, _ := handoffFixtures()
	r.proposeRemediation(context.Background(), diag, incident)
	first := recordedRefusal(t, r, incident)
	require.NotNil(t, first)

	for range 3 {
		r.proposeRemediation(context.Background(), diag, incident)
	}

	again := recordedRefusal(t, r, incident)
	require.NotNil(t, again)
	assert.True(t, first.LastTransitionTime.Equal(&again.LastTransitionTime))
}

// A deferral is not a refusal and leaves no record.
func TestRefusalHistory_ADeferralLeavesNoRecord(t *testing.T) {
	r, _, incident := handoffWithIncident(t, &stubProposer{result: &remediation.ProposalResult{
		SkipReason: "active remediation already exists", Stage: remediation.StageDeferred, Reason: gate.GuardrailActiveAction,
	}})
	diag, _ := handoffFixtures()

	r.proposeRemediation(context.Background(), diag, incident)

	assert.Nil(t, recordedRefusal(t, r, incident))
}

// L-2: a refusal that comes back is dated by its latest occurrence, so a report window that
// began after the first one still counts it.
func TestRefusalHistory_ARecurringRefusalIsDatedByTheLatestOne(t *testing.T) {
	proposer := refusedProposal(gate.StageValidate, gate.GuardrailStepCommand)
	r, _, incident := handoffWithIncident(t, proposer)
	diag, _ := handoffFixtures()
	r.proposeRemediation(context.Background(), diag, incident)

	// The fault resolved and came back: the live condition was cleared by an answer, the record
	// stands at its old date.
	var stored dorguv1.IncidentMemory
	require.NoError(t, r.Client.Get(context.Background(), client.ObjectKeyFromObject(incident), &stored))
	old := metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))
	findCondition(stored.Status.Conditions, dorguv1.ConditionProposalRefusalRecorded).LastTransitionTime = old
	findCondition(stored.Status.Conditions, dorguv1.ConditionProposalRefused).Status = metav1.ConditionFalse
	findCondition(stored.Status.Conditions, dorguv1.ConditionProposalRefused).Reason = dorguv1.ReasonProposalMade
	require.NoError(t, r.Client.Status().Update(context.Background(), &stored))

	r.proposeRemediation(context.Background(), diag, incident) // refused again, same reason

	rec := recordedRefusal(t, r, incident)
	require.NotNil(t, rec)
	assert.True(t, rec.LastTransitionTime.After(old.Add(24*time.Hour)), "dated now, not ten days ago: %s", rec.LastTransitionTime)
}
