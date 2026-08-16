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

package remediation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = dorguv1.AddToScheme(s)
	return s
}

func testLogger() logr.Logger {
	return zap.New(zap.UseDevMode(true))
}

func newTestAction(namespace string) *dorguv1.RemediationAction {
	const personaName = "my-app"
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ra-test-action",
			Namespace: namespace,
			Labels: map[string]string{
				"dorgu.io/persona-kind":      kindApplicationPersona,
				"dorgu.io/persona-name":      personaName,
				"dorgu.io/persona-namespace": namespace,
			},
		},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind:      kindApplicationPersona,
				Name:      personaName,
				Namespace: namespace,
			},
			Action: dorguv1.RemediationActionDetail{
				Type: "persona-update",
			},
			Confidence: "0.85",
		},
	}
}

func TestSafetyChecker_RateLimit_ZeroExisting_Allowed(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction(defaultNamespace)
	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Empty(t, result.Violations)
}

func TestSafetyChecker_RateLimit_FiveExisting_Blocked(t *testing.T) {
	scheme := newTestScheme()

	// Create 5 existing RemediationActions for the same persona.
	existingActions := make([]runtime.Object, 0, 5)
	for i := range 5 {
		ra := &dorguv1.RemediationAction{
			ObjectMeta: metav1.ObjectMeta{
				Name:              fmt.Sprintf("ra-existing-%d", i),
				Namespace:         defaultNamespace,
				CreationTimestamp: metav1.Now(),
				Labels: map[string]string{
					"dorgu.io/persona-kind": kindApplicationPersona,
					"dorgu.io/persona-name": "my-app",
				},
			},
			Spec: dorguv1.RemediationActionSpec{
				PersonaRef: dorguv1.PersonaReference{
					Kind:      kindApplicationPersona,
					Name:      "my-app",
					Namespace: defaultNamespace,
				},
				Action:     dorguv1.RemediationActionDetail{Type: "persona-update"},
				Confidence: "0.85",
			},
			Status: dorguv1.RemediationActionStatus{
				Phase: "Completed",
			},
		}
		existingActions = append(existingActions, ra)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(existingActions...).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction(defaultNamespace)
	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	require.Len(t, result.Violations, 1)
	assert.Equal(t, "rate-limit", result.Violations[0].Rule)
}

func TestSafetyChecker_Concurrent_Blocked(t *testing.T) {
	scheme := newTestScheme()

	existing := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ra-concurrent",
			Namespace: defaultNamespace,
			Labels: map[string]string{
				"dorgu.io/persona-kind": kindApplicationPersona,
				"dorgu.io/persona-name": "my-app",
			},
		},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind:      kindApplicationPersona,
				Name:      "my-app",
				Namespace: defaultNamespace,
			},
			Action:     dorguv1.RemediationActionDetail{Type: "persona-update"},
			Confidence: "0.85",
		},
		Status: dorguv1.RemediationActionStatus{
			Phase: "Applying",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(existing).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction(defaultNamespace)
	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	hasViolation := false
	for _, v := range result.Violations {
		if v.Rule == "concurrent" {
			hasViolation = true
		}
	}
	assert.True(t, hasViolation, "expected concurrent violation")
}

func TestSafetyChecker_FailedCooldown_Blocked(t *testing.T) {
	scheme := newTestScheme()

	recentFailTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	existing := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ra-failed",
			Namespace:         defaultNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
			Labels: map[string]string{
				"dorgu.io/persona-kind": kindApplicationPersona,
				"dorgu.io/persona-name": "my-app",
			},
		},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind:      kindApplicationPersona,
				Name:      "my-app",
				Namespace: defaultNamespace,
			},
			Action:     dorguv1.RemediationActionDetail{Type: "persona-update"},
			Confidence: "0.85",
		},
		Status: dorguv1.RemediationActionStatus{
			Phase:     "Failed",
			AppliedAt: &recentFailTime,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(existing).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction(defaultNamespace)
	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	hasViolation := false
	for _, v := range result.Violations {
		if v.Rule == "rate-limit" {
			hasViolation = true
		}
	}
	assert.True(t, hasViolation, "expected rate-limit violation from failed cooldown")
}

func TestSafetyChecker_BlastRadius_1_5x_Allowed(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction(defaultNamespace)
	action.Spec.Action.Patch = &apiextensionsv1.JSON{
		Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`),
	}
	action.Spec.Action.PrePatchState = &apiextensionsv1.JSON{
		Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`),
	}

	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestSafetyChecker_BlastRadius_3x_Blocked(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction(defaultNamespace)
	action.Spec.Action.Patch = &apiextensionsv1.JSON{
		Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"768Mi"}}}}`),
	}
	action.Spec.Action.PrePatchState = &apiextensionsv1.JSON{
		Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`),
	}

	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	require.Len(t, result.Violations, 1)
	assert.Equal(t, "blast-radius", result.Violations[0].Rule)
}

func TestSafetyChecker_BlastRadius_60Percent_Decrease_Blocked(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction(defaultNamespace)
	action.Spec.Action.Patch = &apiextensionsv1.JSON{
		Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"100Mi"}}}}`),
	}
	action.Spec.Action.PrePatchState = &apiextensionsv1.JSON{
		Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`),
	}

	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	require.Len(t, result.Violations, 1)
	assert.Equal(t, "blast-radius", result.Violations[0].Rule)
}

func TestSafetyChecker_DenyList_DefaultNamespace_Allowed(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction(defaultNamespace)
	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.True(t, result.Allowed)
}

func TestSafetyChecker_DenyList_KubeSystem_Blocked(t *testing.T) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction("kube-system")
	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	hasDenyViolation := false
	for _, v := range result.Violations {
		if v.Rule == "deny-list" {
			hasDenyViolation = true
		}
	}
	assert.True(t, hasDenyViolation, "expected deny-list violation")
}

func TestSafetyChecker_DenyList_ClusterPersonaExcluded_Blocked(t *testing.T) {
	scheme := newTestScheme()

	cp := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster",
		},
		Spec: dorguv1.ClusterPersonaSpec{
			Name: "test-cluster",
			Policies: &dorguv1.ClusterPolicies{
				SelfHealing: &dorguv1.SelfHealingPolicy{
					Enabled:           true,
					Mode:              "propose",
					TrustLevel:        2,
					ExcludeNamespaces: []string{"staging"},
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(cp).Build()
	checker := NewSafetyChecker(c, testLogger())

	action := newTestAction("staging")
	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	hasDenyViolation := false
	for _, v := range result.Violations {
		if v.Rule == "deny-list" {
			hasDenyViolation = true
		}
	}
	assert.True(t, hasDenyViolation, "expected deny-list violation for excluded namespace")
}

func TestSafetyChecker_MultipleViolations(t *testing.T) {
	scheme := newTestScheme()

	// Create 5 existing actions (rate limit) + 1 concurrent.
	objects := make([]runtime.Object, 0, 6)
	for i := range 5 {
		ra := &dorguv1.RemediationAction{
			ObjectMeta: metav1.ObjectMeta{
				Name:              fmt.Sprintf("ra-multi-%d", i),
				Namespace:         "kube-system",
				CreationTimestamp: metav1.Now(),
				Labels: map[string]string{
					"dorgu.io/persona-kind": kindApplicationPersona,
					"dorgu.io/persona-name": "my-app",
				},
			},
			Spec: dorguv1.RemediationActionSpec{
				PersonaRef: dorguv1.PersonaReference{
					Kind:      kindApplicationPersona,
					Name:      "my-app",
					Namespace: "kube-system",
				},
				Action:     dorguv1.RemediationActionDetail{Type: "persona-update"},
				Confidence: "0.85",
			},
			Status: dorguv1.RemediationActionStatus{
				Phase: "Completed",
			},
		}
		objects = append(objects, ra)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	checker := NewSafetyChecker(c, testLogger())

	// kube-system namespace (deny-list) + 5 existing (rate-limit).
	action := newTestAction("kube-system")
	result, err := checker.Check(context.Background(), action)

	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.GreaterOrEqual(t, len(result.Violations), 2, "expected at least 2 violations")
}
