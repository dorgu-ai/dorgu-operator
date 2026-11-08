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
	"testing"

	"github.com/stretchr/testify/assert"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// --- R9: a Step.Command must be one plain kubectl invocation ----------------------

func commandCandidate(command string) Candidate {
	cand := planCandidate(personaUpdateStep(memPatch("384Mi")), dorguv1.RemediationStep{
		Order: 2, ID: "step-2", Type: dorguv1.StepTypeManual, Description: "look", Command: command,
	})
	cand.Final = true
	return cand
}

func TestR9_NonPlainCommandsAreRefused(t *testing.T) {
	for _, command := range []string{
		"sudo kubectl delete deployment web -n shop",
		"/usr/local/bin/kubectl delete deployment web -n shop",
		"./kubectl delete deployment web",
		"kubectl get pods && kubectl delete deployment web",
		"kubectl get pods; kubectl delete deployment web",
		"kubectl get pods | xargs kubectl delete",
		"kubectl get pods > /etc/passwd",
		"kubectl get $(rm -rf /)",
		"kubectl get `id`",
		"kubectl get pods\nkubectl delete deployment web",
		"helm uninstall web",
		"kubectl get pods --kubeconfig=/tmp/attacker",
		"kubectl get pods --server=https://evil.example",
		"kubectl get pods --token=abc",
		"kubectl get pods --as=system:admin",
		"kubectl get pods --context=prod",
		"KUBECONFIG=/tmp/x kubectl get pods",
		"kubectl get 'pods'",
		"kubectl get pods\x00delete",
		"kubectl get pods é",
	} {
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))

		d := run(t, g, commandCandidate(command), PhaseProposal)

		stage, guardrail := refusedBy(t, d)
		assert.Equal(t, StageValidate, stage, "command %q", command)
		assert.Equal(t, GuardrailStepCommand, guardrail, "command %q", command)
	}
}

func TestR9_PlainKubectlCommandsPassTheShapeCheck(t *testing.T) {
	for _, command := range []string{
		"",
		"kubectl get pods -n shop",
		"kubectl describe deployment/web -n shop",
		"kubectl logs deployment/web -n shop --tail=100",
		"kubectl rollout status deployment/web -n shop",
		"kubectl set image deployment/web web=nginx:1.27-alpine -n shop",
	} {
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))

		d := run(t, g, commandCandidate(command), PhaseProposal)

		assert.NotEqual(t, GuardrailStepCommand, func() string { _, gr := d.RefusedBy(); return gr }(), "command %q", command)
	}
}

func TestIsPlainKubectlCommand(t *testing.T) {
	assert.True(t, IsPlainKubectlCommand("kubectl get pods -n shop"))
	assert.False(t, IsPlainKubectlCommand(""))
	assert.False(t, IsPlainKubectlCommand("kubectl"))
	assert.False(t, IsPlainKubectlCommand("kubectl  get pods"), "doubled spaces are not a plain command")
}

// --- R10: dotted keys and non-object patches are refused --------------------------

func TestR10_DottedKeysAreRefused(t *testing.T) {
	for _, patch := range []string{
		`{"spec":{"resources":{"limits.memory":"999Gi"}}}`,
		`{"spec.resources.limits.memory":"999Gi"}`,
		`{"spec":{"resources":{"limits":{"memory":"384Mi"}}},"spec.resources.limits.memory":"999Gi"}`,
	} {
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))

		stage, guardrail := refusedBy(t, run(t, g, planCandidate(personaUpdateStep(jsonOf(patch))), PhaseProposal))

		assert.Equal(t, StageValidate, stage, patch)
		assert.Equal(t, GuardrailPatchShape, guardrail, patch)
	}
}

func TestR10_NonObjectPatchesAreRefused(t *testing.T) {
	for _, patch := range []string{`"string"`, `[1,2]`, `42`, `true`, `null`, `{not json`} {
		g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))

		stage, guardrail := refusedBy(t, run(t, g, planCandidate(personaUpdateStep(jsonOf(patch))), PhaseProposal))

		assert.Equal(t, StageValidate, stage, patch)
		assert.Equal(t, GuardrailPatchShape, guardrail, patch)
	}
}

func TestR10_DottedKeyInActionPatchIsRefusedToo(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))

	_, guardrail := refusedBy(t, run(t, g, legacyCandidate(jsonOf(`{"spec":{"resources":{"limits.memory":"999Gi"}}}`)), PhaseProposal))

	assert.Equal(t, GuardrailPatchShape, guardrail)
}
