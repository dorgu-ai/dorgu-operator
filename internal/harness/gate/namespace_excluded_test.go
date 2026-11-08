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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// NamespaceExcluded is detection's question to the same deny list the gate
// uses: it must agree with NamespaceDenied about every namespace the list names.
func TestNamespaceExcluded(t *testing.T) {
	persona := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.ExcludeNamespaces = []string{"cert-manager", " ArgoCD "} })
	g := New(fakeClient(t, persona), logr.Discard(), WithProtectedNamespaces("Dorgu-System", "gateway"))

	tests := []struct {
		namespace string
		want      bool
	}{
		{"kube-system", true},
		{"dorgu-system", true},
		{"DORGU-SYSTEM", true},
		{"gateway", true},
		{"cert-manager", true},
		{"argocd", true},
		{"  Cert-Manager ", true},
		{"shop", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			got, err := g.NamespaceExcluded(context.Background(), tt.namespace)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			// One source of truth: for every non-empty namespace, detection skips it
			// exactly when the gate would deny it by the list.
			if tt.namespace != "" {
				assert.Equal(t, tt.want, g.NamespaceProtected(context.Background(), tt.namespace),
					"detection and the gate must agree on %q", tt.namespace)
			}
		})
	}
}

// An operator that cannot name its own namespace refuses every action but does
// not blind detection.
func TestNamespaceExcluded_UnknownOperatorNamespaceDoesNotBlindDetection(t *testing.T) {
	g := New(fakeClient(t), logr.Discard(), WithOperatorNamespaceUnknown())

	excluded, err := g.NamespaceExcluded(context.Background(), "shop")
	require.NoError(t, err)
	assert.False(t, excluded, "detection still sees shop")
	assert.True(t, g.NamespaceProtected(context.Background(), "shop"), "the gate still refuses it")

	excluded, err = g.NamespaceExcluded(context.Background(), "kube-system")
	require.NoError(t, err)
	assert.True(t, excluded)
}

// An unreadable policy is an error, never a guess.
func TestNamespaceExcluded_UnreadablePolicyIsAnError(t *testing.T) {
	c := failingListClient(t, isKind[*dorguv1.ClusterPersonaList])
	g := New(c, logr.Discard())

	excluded, err := g.NamespaceExcluded(context.Background(), "shop")
	require.Error(t, err)
	assert.False(t, excluded)

	// The protected set needs no read, so it still answers.
	excluded, err = g.NamespaceExcluded(context.Background(), "kube-system")
	require.NoError(t, err)
	assert.True(t, excluded)
}

func TestNamespaceExcluded_NilGateIsAnError(t *testing.T) {
	var g *Gate
	_, err := g.NamespaceExcluded(context.Background(), "shop")
	require.Error(t, err)
}
