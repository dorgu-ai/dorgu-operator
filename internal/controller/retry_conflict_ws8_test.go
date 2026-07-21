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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// TestIncidentController_Conditions_RetriesOnConflict verifies the retry-on-
// conflict wrapper on IncidentController's conditions status write (WS8 F3): a
// first-attempt Conflict is recovered by re-fetching and re-deriving conditions.
func TestIncidentController_Conditions_RetriesOnConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))

	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: "im-cond", Namespace: "default"},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "api", Namespace: "default"},
			Category:   "health",
			Severity:   "critical",
			Detection:  dorguv1.DetectionInfo{Signal: "OOMKilled"},
		},
		Status: dorguv1.IncidentMemoryStatus{Phase: PhaseDetected, OccurrenceCount: 1},
	}

	base := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(im).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	counter := &conflictCounter{}
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: counter.interceptor(im.Name),
	})

	r := &IncidentController{Client: c, Logger: zap.New(zap.UseDevMode(true))}

	// Persona intentionally absent: syncPersonaStatus returns before any status
	// write, isolating the interceptor to the conditions update under test.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(im)})
	require.NoError(t, err, "retry-on-conflict loop should recover")
	assert.GreaterOrEqual(t, counter.calls, 2, "conditions update should be retried after the first conflict")

	var got dorguv1.IncidentMemory
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
	var detected *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == ConditionDetected {
			detected = &got.Status.Conditions[i]
		}
	}
	require.NotNil(t, detected, "Detected condition should be persisted")
	assert.Equal(t, metav1.ConditionTrue, detected.Status)
}

// TestIncidentController_SyncPersonaStatus_RetriesOnConflict verifies the retry-
// on-conflict wrapper on the ApplicationPersona status sync (WS8 F3).
func TestIncidentController_SyncPersonaStatus_RetriesOnConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))

	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       dorguv1.ApplicationPersonaSpec{Name: "api"},
	}
	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "im-sync",
			Namespace: "default",
			Labels: map[string]string{
				LabelPersonaKind: "ApplicationPersona",
				LabelPersonaName: "api",
			},
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "api", Namespace: "default"},
			Category:   "health",
			Detection:  dorguv1.DetectionInfo{Signal: "OOMKilled"},
		},
		Status: dorguv1.IncidentMemoryStatus{Phase: PhaseDetected},
	}

	base := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(persona, im).
		WithStatusSubresource(&dorguv1.ApplicationPersona{}, &dorguv1.IncidentMemory{}).
		Build()

	counter := &conflictCounter{}
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: counter.interceptor(persona.Name),
	})

	r := &IncidentController{Client: c, Logger: zap.New(zap.UseDevMode(true))}

	require.NoError(t, r.syncPersonaStatus(context.Background(), im), "retry-on-conflict loop should recover")
	assert.GreaterOrEqual(t, counter.calls, 2, "persona status update should be retried after the first conflict")

	var got dorguv1.ApplicationPersona
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(persona), &got))
	assert.Equal(t, int32(1), got.Status.ActiveIncidents, "one active incident counted despite the conflict")
}
