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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

func personaWithExcluded(namespaces ...string) *dorguv1.ClusterPersona {
	return &dorguv1.ClusterPersona{
		Spec: dorguv1.ClusterPersonaSpec{Policies: &dorguv1.ClusterPolicies{
			SelfHealing: &dorguv1.SelfHealingPolicy{ExcludeNamespaces: namespaces},
		}},
	}
}

func conditionOf(p *dorguv1.ClusterPersona, condType string) *metav1.Condition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == condType {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// #51 follow-up: an operator that does not know its own namespace cannot keep
// that namespace away from a model, so AI is off, and the ClusterPersona says so.
func TestClusterPersona_ReportsAnUnknownOperatorNamespace(t *testing.T) {
	p := personaWithExcluded()

	(&ClusterPersonaReconciler{OperatorNamespaceUnknown: true}).reportSafetyConfiguration(p)
	cond := conditionOf(p, ConditionOperatorNamespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "OperatorNamespaceUnknown", cond.Reason)
	assert.Contains(t, cond.Message, "AI diagnosis and AI remediation planning are disabled")

	// Once it is known, the same condition flips, with its message rewritten.
	(&ClusterPersonaReconciler{}).reportSafetyConfiguration(p)
	cond = conditionOf(p, ConditionOperatorNamespace)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "OperatorNamespaceKnown", cond.Reason)
	assert.Len(t, p.Status.Conditions, 2, "one condition per type, updated in place")
}

// #51 follow-up: excludeNamespaces entries are exact namespace names. A wildcard
// matches nothing while looking like protection.
func TestClusterPersona_ValidatesExcludeNamespaces(t *testing.T) {
	t.Run("valid names", func(t *testing.T) {
		p := personaWithExcluded("payments", "team-a", "ns1")
		(&ClusterPersonaReconciler{}).reportSafetyConfiguration(p)
		cond := conditionOf(p, ConditionExcludeNamespaces)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
	})

	t.Run("no policy at all", func(t *testing.T) {
		p := &dorguv1.ClusterPersona{}
		(&ClusterPersonaReconciler{}).reportSafetyConfiguration(p)
		assert.Equal(t, metav1.ConditionTrue, conditionOf(p, ConditionExcludeNamespaces).Status)
	})

	t.Run("names that can never match", func(t *testing.T) {
		p := personaWithExcluded("payments", "prod-*", "Billing", "has space", "-lead", strings.Repeat("a", 64), "")
		(&ClusterPersonaReconciler{}).reportSafetyConfiguration(p)
		cond := conditionOf(p, ConditionExcludeNamespaces)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, "InvalidNamespaceName", cond.Reason)
		assert.Contains(t, cond.Message, `"prod-*"`)
		assert.Contains(t, cond.Message, `"Billing"`)
		assert.NotContains(t, cond.Message, `"payments"`, "a valid entry is not reported")
		assert.Contains(t, cond.Message, "and 1 more", "a long list is summarised")
		assert.Contains(t, cond.Message, "wildcards are not supported")
	})

	t.Run("a single invalid entry reads naturally", func(t *testing.T) {
		p := personaWithExcluded("Prod")
		(&ClusterPersonaReconciler{}).reportSafetyConfiguration(p)
		assert.Contains(t, conditionOf(p, ConditionExcludeNamespaces).Message, "has 1 entry that cannot")
	})
}

func TestInvalidNamespaceNames(t *testing.T) {
	assert.Empty(t, invalidNamespaceNames(nil))
	assert.Equal(t, []string{"a_b", "UP"}, invalidNamespaceNames([]string{"ok", "a_b", "UP", "fine-1"}))
}

func TestProposalOutcome_SuppressedIsItsOwnOutcome(t *testing.T) {
	assert.Equal(t, metrics.ProposalSuppressed, proposalOutcome(&remediation.ProposalResult{Suppressed: true}, nil))
	assert.Equal(t, metrics.ProposalError, proposalOutcome(&remediation.ProposalResult{Suppressed: true}, assert.AnError))
}
