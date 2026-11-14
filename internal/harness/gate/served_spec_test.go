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

package gate

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate/gatetest"
)

// The ticket hash is of the spec the API server serves now (P2-D14, C3 re-review
// N-1), read through the gate's uncached reader.

func TestServedSpecHash_IsTheHashOfWhatTheReaderServes(t *testing.T) {
	ra := proposedAction()
	g := standardGate(t)

	got := serve(t, g, ra)

	assert.Equal(t, gatetest.ServedSpecHash(ra), got)
}

func TestServedSpecHash_ReadsTheUncachedReaderNotTheCache(t *testing.T) {
	cached := proposedAction()
	truth := proposedAction()
	truth.Spec.Explanation = "edited, and the cache has not seen it"
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), cached), WithReader(fakeClient(t, truth)))

	got, err := g.ServedSpecHash(context.Background(), truth)

	require.NoError(t, err)
	assert.Equal(t, gatetest.ServedSpecHash(truth), got)
	assert.NotEqual(t, gatetest.ServedSpecHash(cached), got)
}

func TestServedSpecHash_AServedSpecNewerOrOtherThanTheOneInHandIsNeverHashed(t *testing.T) {
	cases := map[string]func(served *dorguv1.RemediationAction){
		"the spec was edited (generation moved)": func(ra *dorguv1.RemediationAction) { ra.Generation = 2 },
		"the action was recreated (UID changed)": func(ra *dorguv1.RemediationAction) { ra.UID = "recreated" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			inHand := proposedAction()
			g := standardGate(t)
			served := inHand.DeepCopy()
			change(served)
			serve(t, g, served)

			hash, err := g.ServedSpecHash(context.Background(), inHand)

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrSpecMoved)
			assert.Empty(t, hash)
		})
	}
}

func TestServedSpecHash_AnUnreadableActionIsAnErrorNotAMoveOrAHash(t *testing.T) {
	ra := proposedAction()
	for name, g := range map[string]*Gate{
		"not found": standardGate(t),
		"the API server fails": newTestGate(t, fakeClient(t), WithReader(interceptor.NewClient(fakeClient(t, ra), interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("the API server is unavailable")
			},
		}))),
	} {
		t.Run(name, func(t *testing.T) {
			hash, err := g.ServedSpecHash(context.Background(), ra)

			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrSpecMoved)
			assert.Empty(t, hash)
		})
	}
}

func TestCheckTicket_ASpecEditedSinceTheActionWasReadIsSpecChanged(t *testing.T) {
	g := standardGate(t)
	ra := ticketedAction()
	served := ra.DeepCopy()
	served.Generation = 2
	serve(t, g, served)

	problem, err := g.CheckTicket(context.Background(), ra)

	require.NoError(t, err)
	require.NotNil(t, problem)
	assert.Equal(t, GuardrailSpecChanged, problem.Guardrail)
}

func TestCheckTicket_AnUnreadableActionIsAnErrorTheCallerMustRefuseOn(t *testing.T) {
	g := standardGate(t)

	problem, err := g.CheckTicket(context.Background(), ticketedAction())

	require.Error(t, err, "the served spec could not be read, so nothing may be honoured")
	assert.Nil(t, problem)
}

func TestApprovalCandidate_WithNoSpecHashIssuesNoTicket(t *testing.T) {
	g := standardGate(t)
	ra := proposedAction()
	serve(t, g, ra)

	d, err := g.Run(context.Background(), ApprovalCandidate(ra, ""), PhaseApprovalRecheck)

	require.NoError(t, err)
	assert.False(t, d.Allowed(), "an approval that cannot bind a spec hash is refused")
	_, guardrail := d.RefusedBy()
	assert.Equal(t, GuardrailTicketIncomplete, guardrail)
}
