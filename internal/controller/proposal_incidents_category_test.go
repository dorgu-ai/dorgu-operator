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
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

func TestProposalIncidentCategory(t *testing.T) {
	plain := &dorguv1alpha1.RemediationProposal{}
	assert.Equal(t, "health", proposalIncidentCategory(plain), "no persona-update step is a health incident")

	resize := &dorguv1alpha1.RemediationProposal{Spec: dorguv1alpha1.RemediationProposalSpec{
		Steps: []dorguv1.RemediationStep{{Type: dorguv1.StepTypePersonaUpdate}},
	}}
	assert.Equal(t, "resource", proposalIncidentCategory(resize))

	// A rollback carries no steps (the operator derives them) and restores resources.
	rollback := &dorguv1alpha1.RemediationProposal{Spec: dorguv1alpha1.RemediationProposalSpec{
		RollbackOf: &dorguv1alpha1.ActionRef{Name: "ra-1"},
	}}
	assert.Equal(t, "resource", proposalIncidentCategory(rollback))
}
