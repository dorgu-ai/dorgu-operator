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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

// newConflict returns an "object has been modified" apierror for the
// IncidentMemory resource, matching what the real API server emits.
func newConflict(name string) error {
	return apierrors.NewConflict(
		schema.GroupResource{Group: "dorgu.io", Resource: "incidentmemories"},
		name,
		assert.AnError,
	)
}

// conflictCounter wraps a Funcs.SubResourceUpdate interceptor and tracks how
// many times it has been called.
type conflictCounter struct {
	calls int
}

// interceptor returns a SubResourceUpdate interceptor that returns a Conflict
// error the first time it is called, and delegates to the underlying fake
// client afterwards. The caller can inspect `calls` to confirm both attempts
// happened.
func (cc *conflictCounter) interceptor(name string) func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
	return func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		cc.calls++
		if cc.calls == 1 {
			return newConflict(name)
		}
		return c.SubResource(subResourceName).Update(ctx, obj, opts...)
	}
}

// TestUpdateExistingIncident_RetriesOnConflict exercises the retry-on-conflict
// wrapper added to HealthCheckReconciler.updateExistingIncident. The interceptor
// returns a Conflict on the first status update; the retry loop must re-fetch,
// re-apply the mutation, and succeed on the second attempt.
func TestUpdateExistingIncident_RetriesOnConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))

	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "im-default-api-oomkilled-abc123",
			Namespace: "default",
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind: "ApplicationPersona",
				Name: "api",
			},
			Category: "health",
			Severity: "critical",
			Detection: dorguv1.DetectionInfo{
				Signal:   "OOMKilled",
				Source:   "test",
				LastSeen: metav1.Now(),
			},
		},
		Status: dorguv1.IncidentMemoryStatus{
			Phase:           PhaseDetected,
			OccurrenceCount: 1,
		},
	}

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(im).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	counter := &conflictCounter{}
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: counter.interceptor(im.Name),
	})

	r := &HealthCheckReconciler{
		Client: c,
		Logger: zap.New(zap.UseDevMode(true)),
	}

	diag := &diagnosis.Diagnosis{
		PersonaRef: &dorguv1.PersonaReference{
			Kind:      "ApplicationPersona",
			Name:      "api",
			Namespace: "default",
		},
		Category:   "health",
		Confidence: 0.9,
		Summary:    "OOM",
	}

	now := metav1.Now()
	err := r.updateExistingIncident(context.Background(), im, diag, now)
	require.NoError(t, err, "retry-on-conflict loop should recover")

	// Verify the occurrence count was incremented exactly once despite the
	// first conflict (the retry loop re-applies the mutation after re-fetch).
	var got dorguv1.IncidentMemory
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
	assert.Equal(t, int32(2), got.Status.OccurrenceCount)
	assert.NotNil(t, got.Status.LastOccurrence)
}

// TestUpdateIncidentResolution_RetriesOnConflict exercises the retry-on-conflict
// wrapper added to RemediationController.updateIncidentResolution.
func TestUpdateIncidentResolution_RetriesOnConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))

	firstSeen := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	incident := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "im-default-api-oomkilled-abc123",
			Namespace: "default",
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind: "ApplicationPersona",
				Name: "api",
			},
			Category: "health",
			Severity: "critical",
			Detection: dorguv1.DetectionInfo{
				Signal:    "OOMKilled",
				Source:    "test",
				FirstSeen: firstSeen,
				LastSeen:  firstSeen,
			},
		},
		Status: dorguv1.IncidentMemoryStatus{
			Phase:           PhaseDetected,
			OccurrenceCount: 1,
		},
	}

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(incident).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}).
		Build()

	counter := &conflictCounter{}
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: counter.interceptor(incident.Name),
	})

	appliedAt := metav1.Now()
	action := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ra-default-api-memory",
			Namespace: "default",
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{
				Name:      incident.Name,
				Namespace: incident.Namespace,
			},
			PersonaRef: dorguv1.PersonaReference{
				Kind: "ApplicationPersona",
				Name: "api",
			},
			Explanation: "increase memory",
			Confidence:  "0.90",
			Action: dorguv1.RemediationActionDetail{
				Type: "persona-update",
			},
		},
		Status: dorguv1.RemediationActionStatus{
			Phase:     RemediationPhaseCompleted,
			AppliedAt: &appliedAt,
		},
	}

	r := &RemediationController{
		Client: c,
		Logger: zap.New(zap.UseDevMode(true)),
	}

	err := r.updateIncidentResolution(context.Background(), action, "resolved")
	require.NoError(t, err, "retry-on-conflict loop should recover")

	// Both attempts (conflict + success) must have hit the interceptor.
	assert.GreaterOrEqual(t, counter.calls, 2, "retry loop should invoke status update at least twice")

	// Verify the incident was resolved despite the first conflict.
	var got dorguv1.IncidentMemory
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(incident), &got))
	assert.Equal(t, PhaseResolved, got.Status.Phase)
	assert.Equal(t, PhaseResolved, got.Labels[LabelPhase])
	require.NotNil(t, got.Spec.Resolution)
	assert.Equal(t, "resolved", got.Spec.Resolution.Outcome)
}
