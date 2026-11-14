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
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// countingProposer records how many times the reconciler asked for a proposal,
// for the tests that do not care about the proposer's own decisions.
type countingProposer struct {
	calls int
}

func (p *countingProposer) Propose(
	_ context.Context,
	_ diagnosis.Diagnosis,
	_ *dorguv1.IncidentMemory,
) (*remediation.ProposalResult, error) {
	p.calls++
	return &remediation.ProposalResult{SkipReason: "stub proposer"}, nil
}

// spyProposer wraps the REAL proposer, and with it the real gate, and records
// what it answered. The rejection cooldown is the gate's: it is decided inside
// Propose, before any planning call, so these tests ask the real thing whether it
// would have proposed rather than stubbing the decision out.
type spyProposer struct {
	inner remediation.RemediationProposer
	calls int
	last  *remediation.ProposalResult
	err   error
}

func (p *spyProposer) Propose(
	ctx context.Context,
	diag diagnosis.Diagnosis,
	incident *dorguv1.IncidentMemory,
) (*remediation.ProposalResult, error) {
	p.calls++
	p.last, p.err = p.inner.Propose(ctx, diag, incident)
	return p.last, p.err
}

// suppressed reports whether the proposer withheld the proposal for a prior
// rejection.
func (p *spyProposer) suppressed() bool { return p.last != nil && p.last.Suppressed }

func newSpyProposer(c client.Client) *spyProposer {
	return &spyProposer{inner: remediation.NewProposer(c, gate.New(c, logr.Discard()), logr.Discard())}
}

// rejectedAction builds a RemediationAction the user declined, with the
// rejection stamped at the given time. A zero rejectedAt leaves the condition
// off entirely, which is what the CLI alone produces.
func rejectedAction(name string, incident *dorguv1.IncidentMemory, rejectedAt time.Time) *dorguv1.RemediationAction {
	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         incident.Namespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-90 * time.Minute)),
			Labels: map[string]string{
				"dorgu.io/persona-kind":      incident.Spec.PersonaRef.Kind,
				"dorgu.io/persona-name":      incident.Spec.PersonaRef.Name,
				"dorgu.io/persona-namespace": incident.Namespace,
			},
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{Name: incident.Name, Namespace: incident.Namespace},
			PersonaRef:  incident.Spec.PersonaRef,
			TrustLevel:  2,
			Explanation: "increase the memory limit",
			Confidence:  "0.90",
			Action:      dorguv1.RemediationActionDetail{Type: "persona-update"},
		},
		Status: dorguv1.RemediationActionStatus{Phase: RemediationPhaseRejected},
	}
	if !rejectedAt.IsZero() {
		ra.Status.Conditions = []metav1.Condition{{
			Type:               ConditionRejected,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonUserRejected,
			Message:            "declined by a human",
			LastTransitionTime: metav1.NewTime(rejectedAt),
		}}
	}
	return ra
}

// proposalRun wires a reconciler over the given objects and runs one
// processDiagnosis cycle, returning what the real proposer decided.
func proposalRun(t *testing.T, diag *diagnosis.Diagnosis, objs ...client.Object) (*spyProposer, error) {
	t.Helper()

	c := fake.NewClientBuilder().
		WithScheme(cf4Scheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}).
		Build()

	proposer := newSpyProposer(c)
	logger, _ := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		Proposer:     proposer,
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
	}

	err := r.processDiagnosis(context.Background(), personaSubject(*diag.PersonaRef), diag, map[string]bool{})
	return proposer, err
}

// TestRejectedRemediation_SuppressesReproposal reproduces F-07: declining a
// remediation used to buy about 30 seconds of quiet before dorgu proposed the
// identical fix again, so saying no cost money.
func TestRejectedRemediation_SuppressesReproposal(t *testing.T) {
	im := activeIncident()
	ra := rejectedAction("ra-api-memory-rejected", im, time.Now().Add(-2*time.Minute))

	proposer, err := proposalRun(t, aiDiagnosis(), im, ra)
	require.NoError(t, err)
	assert.True(t, proposer.suppressed(), "a rejected remediation must suppress re-proposal for the same incident and target")
}

// TestRejectedRemediation_UnstampedRejectionSuppresses covers the CLI-only path.
// `dorgu remediation reject` patches status.phase and nothing else, so before the
// operator stamps a timestamp there is no time to hold a cooldown against.
// Suppressing is the safe reading: an un-timestamped no is still a no.
func TestRejectedRemediation_UnstampedRejectionSuppresses(t *testing.T) {
	im := activeIncident()
	ra := rejectedAction("ra-api-memory-unstamped", im, time.Time{})

	proposer, err := proposalRun(t, aiDiagnosis(), im, ra)
	require.NoError(t, err)
	assert.True(t, proposer.suppressed(), "a rejection with no recorded timestamp must not be treated as expired")
}

// TestRejectedRemediation_CooldownExpiryAllowsReproposal proves the suppression
// is a cooldown and not a permanent mute: a problem that is still there an hour
// later is worth raising again.
func TestRejectedRemediation_CooldownExpiryAllowsReproposal(t *testing.T) {
	im := activeIncident()
	ra := rejectedAction("ra-api-memory-old", im, time.Now().Add(-RejectionCooldown-time.Minute))

	proposer, err := proposalRun(t, aiDiagnosis(), im, ra)
	require.NoError(t, err)
	assert.False(t, proposer.suppressed(), "an expired rejection cooldown must allow a fresh proposal")
}

// TestRejectedRemediation_SeverityEscalationAllowsReproposal covers the "until
// the signal materially changes" half of the rule. A warning the user waved off
// that has since gone critical is a different question, not the same one.
func TestRejectedRemediation_SeverityEscalationAllowsReproposal(t *testing.T) {
	im := activeIncident()
	im.Spec.Severity = string(detection.SeverityWarning)
	im.Labels[LabelSeverity] = string(detection.SeverityWarning)
	ra := rejectedAction("ra-api-memory-warning", im, time.Now().Add(-2*time.Minute))

	diag := aiDiagnosis()
	diag.Severity = detection.SeverityCritical

	proposer, err := proposalRun(t, diag, im, ra)
	require.NoError(t, err)
	assert.False(t, proposer.suppressed(), "an escalated signal is a materially different question")
}

// TestRejectedRemediation_OtherIncidentDoesNotSuppress keeps the suppression
// scoped. A rejection is a no to one fix for one incident, not a blanket mute
// for the persona.
func TestRejectedRemediation_OtherIncidentDoesNotSuppress(t *testing.T) {
	im := activeIncident()

	other := activeIncident()
	other.Name = "im-default-api-crashloop-cf4"
	other.Labels[LabelSignal] = string(detection.SignalCrashLoopBackOff)
	other.Spec.Detection.Signal = string(detection.SignalCrashLoopBackOff)
	ra := rejectedAction("ra-api-crashloop-rejected", other, time.Now().Add(-2*time.Minute))

	proposer, err := proposalRun(t, aiDiagnosis(), im, other, ra)
	require.NoError(t, err)
	assert.False(t, proposer.suppressed(), "a rejection on another incident must not suppress this one")
}

// TestPendingRemediation_DoesNotSuppress guards the boundary: only Rejected
// actions engage the cooldown. Dedup of in-flight proposals stays the proposer's
// job.
func TestPendingRemediation_DoesNotSuppress(t *testing.T) {
	im := activeIncident()
	ra := rejectedAction("ra-api-memory-pending", im, time.Now().Add(-2*time.Minute))
	ra.Status.Phase = RemediationPhasePending
	ra.Status.Conditions = nil

	proposer, err := proposalRun(t, aiDiagnosis(), im, ra)
	require.NoError(t, err)
	assert.False(t, proposer.suppressed(), "only a Rejected action engages the rejection cooldown")
}

// TestRejectionCheck_FailsClosed asserts the reconciler declines to propose when
// it cannot tell whether a rejection exists. Guessing wrong in the other
// direction re-bills the user for saying no.
func TestRejectionCheck_FailsClosed(t *testing.T) {
	im := activeIncident()

	base := fake.NewClientBuilder().
		WithScheme(cf4Scheme(t)).
		WithObjects(im).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.IncidentMemory{}).
		Build()

	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, isActions := list.(*dorguv1.RemediationActionList); isActions {
				return errors.New("the API server is unavailable")
			}
			return inner.List(ctx, list, opts...)
		},
	})

	proposer := newSpyProposer(c)
	logger, sink := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		Proposer:     proposer,
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
	}

	require.NoError(t, r.processDiagnosis(context.Background(), personaSubject(*aiDiagnosis().PersonaRef), aiDiagnosis(), map[string]bool{}))
	assert.False(t, proposer.last != nil && proposer.last.Proposed,
		"an unreadable rejection history must not produce a proposal")
	require.Error(t, proposer.err, "the gate cannot decide without the history, and says so")
	assert.True(t, sink.hasError("propose"),
		"the failed proposal must be reported at ERROR, got: %v", sink.messages())
}

// TestRecordRejection_StampsTimestamp proves the operator supplies the missing
// half of the contract. The CLI records the decision; the operator records when,
// which is what the cooldown is measured from.
func TestRecordRejection_StampsTimestamp(t *testing.T) {
	im := activeIncident()
	ra := rejectedAction("ra-api-memory-stamp", im, time.Time{})

	c := fake.NewClientBuilder().
		WithScheme(cf4Scheme(t)).
		WithObjects(im, ra).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}).
		Build()

	logger, _ := newRecordingLogger()
	r := &RemediationController{Client: c, Logger: logger}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
	require.NoError(t, err)

	var got dorguv1.RemediationAction
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(ra), &got))

	var cond *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == ConditionRejected {
			cond = &got.Status.Conditions[i]
		}
	}
	require.NotNil(t, cond, "the operator must stamp a Rejected condition")
	assert.Equal(t, ReasonUserRejected, cond.Reason)
	assert.WithinDuration(t, time.Now(), cond.LastTransitionTime.Time, time.Minute)

	// Reconciling again must not move the timestamp: the cooldown would never
	// expire if every pass reset it.
	first := cond.LastTransitionTime
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(ra), &got))
	assert.True(t, first.Equal(&got.Status.Conditions[0].LastTransitionTime),
		"re-reconciling a rejected action must not restart its cooldown")
}
