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

package planner

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func TestBuildPlanUserMessage_IncludesAllContext(t *testing.T) {
	rc := RemediationContext{
		Diagnosis:        oomDiagnosis("my-app"),
		Signals:          signalsFromDiagnosis(oomDiagnosis("my-app")),
		AppPersona:       appPersona(),
		ClusterPersona:   clusterPersona(),
		PastIncidents:    []dorguv1.IncidentMemory{*incident("im-1", "my-app", "OOMKilled", time.Unix(0, 0), "ra-1")},
		PastRemediations: []dorguv1.RemediationAction{*remediation("default", "ra-1", "my-app", "Completed", "Healthy")},
	}

	msg := buildPlanUserMessage(rc)

	// Diagnosis.
	assert.Contains(t, msg, "## Diagnosis")
	assert.Contains(t, msg, "OOMKilled detected")
	assert.Contains(t, msg, "Suggested action: resource-adjustment")

	// Signals.
	assert.Contains(t, msg, "## Signals")
	assert.Contains(t, msg, "OOMKilled")

	// App persona resources + learned baseline.
	assert.Contains(t, msg, "## Application persona")
	assert.Contains(t, msg, "memory=256Mi")
	assert.Contains(t, msg, "Learned baseline")

	// Cluster self-healing policy.
	assert.Contains(t, msg, "## Cluster policy")
	assert.Contains(t, msg, "Self-healing:")
	assert.Contains(t, msg, "mode=propose")
	assert.Contains(t, msg, "trustLevel=2")

	// Past incidents.
	assert.Contains(t, msg, "## Past incidents")

	// Past remediation OUTCOMES.
	assert.Contains(t, msg, "## Past remediation outcomes")
	assert.Contains(t, msg, "phase=Completed")
	assert.Contains(t, msg, "verification=Healthy")
}

func TestBuildPlanUserMessage_HandlesEmptyContext(t *testing.T) {
	msg := buildPlanUserMessage(RemediationContext{Diagnosis: oomDiagnosis("my-app")})
	assert.Contains(t, msg, "(unavailable)") // no app persona
	assert.Contains(t, msg, "(no ClusterPersona configured)")
	assert.Contains(t, msg, "## Past remediation outcomes")
}

func TestPlanToolSchema_IsValidJSON(t *testing.T) {
	schema := planToolSchema()
	raw, err := json.Marshal(schema)
	require.NoError(t, err)

	var roundtrip map[string]any
	require.NoError(t, json.Unmarshal(raw, &roundtrip))
	assert.Equal(t, "object", roundtrip["type"])
	assert.Equal(t, false, roundtrip["additionalProperties"])
	props, ok := roundtrip["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "rootCause")
	assert.Contains(t, props, "confidence")
	assert.Contains(t, props, "steps")
}

// TestPlanToolSchema_OffersAnAdvisoryCommand covers F-10 at the wire contract:
// the model cannot return a copy-paste command unless the tool schema has a
// field for it, and cannot stay within the sanitizer unless the schema says so.
func TestPlanToolSchema_OffersAnAdvisoryCommand(t *testing.T) {
	props := planToolSchema()["properties"].(map[string]any)
	item := props["steps"].(map[string]any)["items"].(map[string]any)
	stepProps := item["properties"].(map[string]any)

	command, ok := stepProps["command"].(map[string]any)
	require.True(t, ok, "steps[].command missing from the tool schema")
	assert.Equal(t, "string", command["type"])
	assert.Contains(t, command["description"], "kubectl ")

	required, _ := item["required"].([]string)
	assert.NotContains(t, required, "command", "command must stay optional")
}

// TestPlanSystemPrompt_AsksForAResolvedCommand guards the instruction itself:
// an unresolved placeholder command is worse than none, so the prompt has to
// demand real names and forbid guessing.
func TestPlanSystemPrompt_AsksForAResolvedCommand(t *testing.T) {
	assert.Contains(t, planSystemPrompt, `"command"`)
	assert.Contains(t, planSystemPrompt, "kubectl set image")
	assert.Contains(t, planSystemPrompt, "Never guess.")
}

// TestExtractPlan_CarriesTheCommandThrough asserts the decode path keeps the
// command; without this the schema field would be silently dropped.
func TestExtractPlan_CarriesTheCommandThrough(t *testing.T) {
	input := `{
		"rootCause": "image tag typo",
		"confidence": 0.91,
		"steps": [
			{"order": 1, "type": "config-change", "description": "fix the tag",
			 "rationale": "tag does not exist", "risk": "low",
			 "command": "kubectl set image deployment/web web=nginx:1.27-alpine -n demo"},
			{"order": 2, "type": "manual", "description": "watch the rollout",
			 "rationale": "confirm recovery", "risk": "low"}
		]
	}`

	plan, err := extractPlan(claudeResponse{Content: []claudeContent{
		{Type: "tool_use", Name: planToolName, Input: json.RawMessage(input)},
	}})
	require.NoError(t, err)
	require.Len(t, plan.Steps, 2)

	assert.Equal(t, "kubectl set image deployment/web web=nginx:1.27-alpine -n demo", plan.Steps[0].Command)
	assert.Empty(t, plan.Steps[1].Command, "an omitted command stays empty")
}
