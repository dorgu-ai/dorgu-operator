/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// QA checklist defaults for ClusterPersona self-healing (BUG-3-6). The mode
// default tracks the CRD's kubebuilder default, which the proposer now enforces.
const (
	wantSelfHealingMode       = dorguv1.SelfHealingModePropose
	wantSelfHealingTrustLevel = int32(2)
)

func testSchemeClusterPersona(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, dorguv1.AddToScheme(s))
	return s
}

func TestClusterPersonaReconcile_SetsSelfHealingDefaultsWhenMissing(t *testing.T) {
	tests := []struct {
		name string
		spec dorguv1.ClusterPersonaSpec
	}{
		{
			name: "policies_present_without_selfHealing",
			spec: dorguv1.ClusterPersonaSpec{
				Name:        "qa-helm",
				Environment: "development",
				Policies: &dorguv1.ClusterPolicies{
					Security: &dorguv1.ClusterSecurityPolicy{
						PodSecurityStandard: "baseline",
					},
				},
			},
		},
		{
			name: "policies_nil",
			spec: dorguv1.ClusterPersonaSpec{
				Name:        "qa-helm",
				Environment: "development",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sch := testSchemeClusterPersona(t)
			cp := &dorguv1.ClusterPersona{
				ObjectMeta: metav1.ObjectMeta{Name: "qa-helm"},
				Spec:       tt.spec,
			}

			cl := fake.NewClientBuilder().
				WithScheme(sch).
				WithObjects(cp).
				WithStatusSubresource(&dorguv1.ClusterPersona{}).
				Build()

			r := &ClusterPersonaReconciler{
				Client: cl,
				Scheme: sch,
			}

			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "qa-helm"},
			})
			require.NoError(t, err)

			updated := &dorguv1.ClusterPersona{}
			require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: "qa-helm"}, updated))

			require.NotNil(t, updated.Spec.Policies, "expected spec.policies after reconcile defaulting")
			require.NotNil(t, updated.Spec.Policies.SelfHealing,
				"expected spec.policies.selfHealing defaults when missing (BUG-3-6)")
			assert.Equal(t, wantSelfHealingMode, updated.Spec.Policies.SelfHealing.Mode)
			assert.Equal(t, wantSelfHealingTrustLevel, updated.Spec.Policies.SelfHealing.TrustLevel)
		})
	}
}

func TestClusterPersonaReconcile_PreservesCustomSelfHealing(t *testing.T) {
	sch := testSchemeClusterPersona(t)
	custom := &dorguv1.SelfHealingPolicy{
		Enabled:                true,
		Mode:                   "auto-approve",
		TrustLevel:             4,
		MaxRemediationsPerHour: 10,
	}
	cp := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-sh"},
		Spec: dorguv1.ClusterPersonaSpec{
			Name: "custom-sh",
			Policies: &dorguv1.ClusterPolicies{
				SelfHealing: custom,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(cp).
		WithStatusSubresource(&dorguv1.ClusterPersona{}).
		Build()

	r := &ClusterPersonaReconciler{
		Client: cl,
		Scheme: sch,
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "custom-sh"},
	})
	require.NoError(t, err)

	updated := &dorguv1.ClusterPersona{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: "custom-sh"}, updated))
	require.NotNil(t, updated.Spec.Policies)
	require.NotNil(t, updated.Spec.Policies.SelfHealing)

	assert.Equal(t, "auto-approve", updated.Spec.Policies.SelfHealing.Mode, "user-provided mode must be preserved when defaulting selfHealing")
	assert.Equal(t, int32(4), updated.Spec.Policies.SelfHealing.TrustLevel, "user-provided trustLevel must be preserved when defaulting selfHealing")
}
