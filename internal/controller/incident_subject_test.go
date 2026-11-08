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
	"testing"

	"github.com/stretchr/testify/assert"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

func TestIncidentSubjectFor(t *testing.T) {
	ref := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "web", Namespace: "shop"}

	t.Run("a persona group is filed against its persona", func(t *testing.T) {
		subject, ok := incidentSubjectFor(&detection.SignalGroup{Scope: detection.ScopePersona, PersonaRef: ref, Namespace: "shop"})
		assert.True(t, ok)
		assert.Equal(t, "shop", subject.namespace)
		assert.False(t, subject.unattributed)
		assert.Equal(t, AttributionPersona, subject.attribution())
		assert.Equal(t, "web", subject.incidentNameKey())
	})

	t.Run("a persona ref with no namespace falls back to the group's", func(t *testing.T) {
		noNS := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "web"}
		subject, ok := incidentSubjectFor(&detection.SignalGroup{Scope: detection.ScopePersona, PersonaRef: noNS, Namespace: "shop"})
		assert.True(t, ok)
		assert.Equal(t, "shop", subject.namespace)
	})

	t.Run("a persona group with no persona belongs to nothing", func(t *testing.T) {
		_, ok := incidentSubjectFor(&detection.SignalGroup{Scope: detection.ScopePersona})
		assert.False(t, ok)
	})

	t.Run("an unattributed group is recorded against the workload and says so", func(t *testing.T) {
		subject, ok := incidentSubjectFor(&detection.SignalGroup{
			Scope: detection.ScopeUnattributed, Workload: "legacy", Namespace: "shop",
		})
		assert.True(t, ok)
		assert.True(t, subject.unattributed)
		assert.Equal(t, AttributionUnattributed, subject.attribution())
		assert.Equal(t, "legacy-unattributed", subject.incidentNameKey())
		assert.Equal(t, "legacy", subject.personaRef.Name)
	})

	t.Run("cluster-scoped groups belong to no application", func(t *testing.T) {
		_, ok := incidentSubjectFor(&detection.SignalGroup{Scope: detection.ScopeCluster})
		assert.False(t, ok)
	})
}

func TestProviderSuffix(t *testing.T) {
	assert.Empty(t, providerSuffix(""))
	assert.Equal(t, " rule-based", providerSuffix("rule-based"))
}
