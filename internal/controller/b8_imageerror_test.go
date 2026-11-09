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

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B8 EKS QA F-1: one bad image is one incident and one proposal. The pod stays
// Pending, so PodPendingLong also fired five minutes in and opened a second
// incident, with its own proposal. The collector no longer raises it for a pod
// with an image error; these tests pin what the reconciler does when the two
// meet anyway, in either order.

func (e *recurrenceEnv) setImageError() {
	e.setSignal(detection.SignalImagePullBackOff, detection.CategoryDeployment, detection.SeverityWarning)
}

func (e *recurrenceEnv) setPendingLong() {
	e.setSignal(detection.SignalPodPendingLong, detection.CategoryScaling, detection.SeverityWarning)
}

func (e *recurrenceEnv) liveActions() int {
	n := 0
	for _, a := range e.actions() {
		switch a.Status.Phase {
		case RemediationPhaseRejected, RemediationPhaseExpired, RemediationPhaseFailed, RemediationPhaseRolledBack, RemediationPhaseCompleted:
		default:
			n++
		}
	}
	return n
}

// Image error first, then PodPendingLong for the same pod: no second incident.
func TestImageError_ThenPendingLong_IsStillOneIncident(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setImageError()
	e.cycle()
	first := e.onlyOpen()
	require.Equal(t, "ImagePullBackOff", first.Spec.Detection.Signal)

	e.count.signals = append(e.count.signals,
		e.signalFor(detection.SignalPodPendingLong, detection.CategoryScaling, detection.SeverityWarning, e.pod, ""))
	e.cycle()
	e.cycle()

	require.Len(t, e.incidents(), 1, "one bad image, one incident: got %s", incidentSignals(e.incidents()))
	assert.Equal(t, first.Name, e.onlyOpen().Name)
	assert.Equal(t, "ImagePullBackOff", e.onlyOpen().Spec.Detection.Signal, "the cause stays the signal of the incident")
}

// PodPendingLong first (a pod Pending for a while), then the image error: the
// advice for the pending pod is superseded, not left beside the new proposal.
func TestPendingLong_ThenImageError_SupersedesTheEarlierProposal(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setPendingLong()
	e.cycle()
	pending := e.onlyOpen()
	require.Equal(t, "PodPendingLong", pending.Spec.Detection.Signal)
	ra := advisoryFor(t, e, pending.Name, RemediationPhasePending)
	ra.Annotations = map[string]string{gate.AnnotationAnswersSignal: "PodPendingLong"}
	require.NoError(t, e.c.Update(context.Background(), ra))

	e.setImageError()
	e.cycle()

	got := e.actionNamed(ra.Name)
	assert.Equal(t, RemediationPhaseExpired, got.Status.Phase, "the advice for the pending pod no longer fits")
	assert.True(t, isSuperseded(&got), "with the reason Superseded, not ApprovalTimedOut and not a rejection")
	assert.LessOrEqual(t, e.liveActions(), 1, "never two live proposals for one bad image")
}

// A Pending pod with no image error is untouched: its incident and proposal stay.
func TestPendingLong_WithoutAnImageErrorKeepsItsProposal(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setPendingLong()
	e.cycle()
	ra := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhasePending)
	e.cycle()

	assert.Equal(t, RemediationPhasePending, e.actionNamed(ra.Name).Status.Phase)
}

// F-4 / M-2: a crash loop that continues is one signal for as long as it lasts, so it
// is one incident and one proposal, and nothing is superseded or re-proposed.
func TestContinuingRestartLoopIsOneIncidentAndOneProposalOverFiveCycles(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setLoop()
	e.cycle()
	ra := advisoryFor(t, e, e.onlyOpen().Name, RemediationPhasePending)
	for range 4 {
		e.setLoop()
		e.cycle()
	}

	require.Len(t, e.incidents(), 1, "got %s", incidentSignals(e.incidents()))
	assert.EqualValues(t, 5, e.onlyOpen().Status.OccurrenceCount)
	assert.Len(t, e.actions(), 1, "one proposal")
	assert.Equal(t, RemediationPhasePending, e.actionNamed(ra.Name).Status.Phase, "and it was not superseded")
}
