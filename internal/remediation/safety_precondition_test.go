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

// F-03: approving the remediation Dorgu itself suggested produced
// "apply failed: precondition failed: unsupported action type", phase Failed,
// and then a 30-minute blackout on the app: "[rate-limit] failed remediation ...
// within 30m0s cooldown period". The cooldown was counting a remediation that
// never touched the cluster.
package remediation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// failedAction builds a Failed RemediationAction for the same persona, with the
// given Applied-condition reason and applied timestamp.
func failedAction(name, conditionReason string, appliedAt *metav1.Time) *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
			Labels: map[string]string{
				"dorgu.io/persona-kind": "ApplicationPersona",
				"dorgu.io/persona-name": "my-app",
			},
		},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      "my-app",
				Namespace: "default",
			},
			Action:     dorguv1.RemediationActionDetail{Type: "notification"},
			Confidence: "0.85",
		},
		Status: dorguv1.RemediationActionStatus{
			Phase:     phaseFailed,
			AppliedAt: appliedAt,
			Conditions: []metav1.Condition{{
				Type:               conditionApplied,
				Status:             metav1.ConditionFalse,
				Reason:             conditionReason,
				Message:            "unsupported action type; nothing was applied",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
			}},
		},
	}
}

func TestSafetyChecker_PreconditionRejection_DoesNotCooldown(t *testing.T) {
	existing := failedAction("ra-precondition-rejected", dorguv1.ReasonPreconditionRejected, nil)

	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(existing).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	checker := NewSafetyChecker(c, testLogger())

	result, err := checker.Check(context.Background(), newTestAction("default", "my-app"))
	require.NoError(t, err)

	for _, v := range result.Violations {
		assert.NotContains(t, v.Message, "cooldown period",
			"a remediation rejected before apply must not cool down the app")
	}
	assert.True(t, result.Allowed, "violations: %+v", result.Violations)
}

func TestSafetyChecker_GenuineApplyFailure_StillCoolsDown(t *testing.T) {
	// No AppliedAt (the patch attempt itself failed) but a real failure reason.
	existing := failedAction("ra-apply-failed", "Failed", nil)

	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(existing).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	checker := NewSafetyChecker(c, testLogger())

	result, err := checker.Check(context.Background(), newTestAction("default", "my-app"))
	require.NoError(t, err)
	assert.False(t, result.Allowed)

	cooldown := false
	for _, v := range result.Violations {
		if v.Rule == "rate-limit" && assert.Contains(t, v.Message, "cooldown period") {
			cooldown = true
		}
	}
	assert.True(t, cooldown, "a real failure must still cool down the app")
}

func TestSafetyChecker_AppliedThenFailed_StillCoolsDown(t *testing.T) {
	// A remediation that reached the cluster and then failed verification is
	// exactly what the cooldown is for, whatever its condition reason says.
	appliedAt := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	existing := failedAction("ra-applied-then-failed", dorguv1.ReasonPreconditionRejected, &appliedAt)

	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(existing).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	checker := NewSafetyChecker(c, testLogger())

	result, err := checker.Check(context.Background(), newTestAction("default", "my-app"))
	require.NoError(t, err)
	assert.False(t, result.Allowed)
}

func TestSafetyChecker_AcknowledgedAction_DoesNotCooldown(t *testing.T) {
	// The new terminal phase for an approved advisory plan.
	existing := failedAction("ra-acknowledged", dorguv1.ReasonAdvisoryOnly, nil)
	existing.Status.Phase = "Acknowledged"

	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(existing).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	checker := NewSafetyChecker(c, testLogger())

	result, err := checker.Check(context.Background(), newTestAction("default", "my-app"))
	require.NoError(t, err)
	assert.True(t, result.Allowed, "violations: %+v", result.Violations)
}
