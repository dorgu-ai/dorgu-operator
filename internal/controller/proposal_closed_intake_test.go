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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

type stubReadiness []intake.Failure

func (s stubReadiness) Failures() []intake.Failure { return s }

// F-8 and F-9: a closed intake is reported on the ClusterPersona, with the reason of
// the first failure and the text of every one.
func TestClusterPersona_ReportsAClosedAndAnOpenIntake(t *testing.T) {
	p := personaWithExcluded()

	(&ClusterPersonaReconciler{IntakeReadiness: stubReadiness{
		{Check: "operator-identity", Reason: intake.ReasonOperatorIdentityUnknown, Message: "OPERATOR_SERVICE_ACCOUNT is not set."},
		{Check: "action-provenance", Reason: intake.ReasonActionProvenanceMissing, Message: "apply the RemediationAction CRD."},
	}}).reportSafetyConfiguration(p)
	cond := conditionOf(p, ConditionIntakeReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, intake.ReasonOperatorIdentityUnknown, cond.Reason)
	assert.Contains(t, cond.Message, "CLOSED")
	assert.Contains(t, cond.Message, "OPERATOR_SERVICE_ACCOUNT is not set.")
	assert.Contains(t, cond.Message, "apply the RemediationAction CRD.")
	assert.Contains(t, cond.Message, "detection, diagnosis and existing actions continue")

	(&ClusterPersonaReconciler{IntakeReadiness: stubReadiness{}}).reportSafetyConfiguration(p)
	cond = conditionOf(p, ConditionIntakeReady)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "IntakeOpen", cond.Reason)
}

func TestClusterPersona_WithNoIntakeWritesNoIntakeCondition(t *testing.T) {
	p := personaWithExcluded()
	(&ClusterPersonaReconciler{}).reportSafetyConfiguration(p)
	assert.Nil(t, conditionOf(p, ConditionIntakeReady), "the health pipeline is off: there is no intake to report")
}

// A closed intake is not an error: the reconciler waits and looks again, writing
// nothing, rather than backing off exponentially and logging an error per proposal.
func TestProposalReconciler_AClosedIntakeWaitsWithoutAnError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))
	require.NoError(t, dorguv1alpha1.AddToScheme(scheme))
	prop := &dorguv1alpha1.RemediationProposal{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p", Generation: 1}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(prop).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}).Build()

	pf := intake.NewPreflight(logr.Discard(), intake.StaticCheck("proposal-crd", intake.ReasonProposalCRDMissing, errors.New("missing")))
	pf.Run(context.Background())
	r := &ProposalReconciler{Client: c, Processor: intake.New(intake.Config{
		Client: c, OperatorUsername: intake.TestOperatorUsername, Preflight: pf, Logger: logr.Discard()})}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "p"}})

	require.NoError(t, err)
	assert.Equal(t, closedIntakeRecheck, res.RequeueAfter)
	var got dorguv1alpha1.RemediationProposal
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "p"}, &got))
	assert.Empty(t, got.Status.Phase, "nothing was written")
}
