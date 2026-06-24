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
		Diagnosis:        oomDiagnosis("default", "my-app"),
		Signals:          signalsFromDiagnosis(oomDiagnosis("default", "my-app")),
		AppPersona:       appPersona("default", "my-app"),
		ClusterPersona:   clusterPersona(),
		PastIncidents:    []dorguv1.IncidentMemory{*incident("default", "im-1", "my-app", "OOMKilled", time.Unix(0, 0), "ra-1")},
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
	msg := buildPlanUserMessage(RemediationContext{Diagnosis: oomDiagnosis("default", "my-app")})
	assert.Contains(t, msg, "(unavailable)") // no app persona
	assert.Contains(t, msg, "(no ClusterPersona configured)")
	assert.Contains(t, msg, "## Past remediation outcomes")
}

func TestPlanToolSchema_IsValidJSON(t *testing.T) {
	schema := planToolSchema()
	raw, err := json.Marshal(schema)
	require.NoError(t, err)

	var roundtrip map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &roundtrip))
	assert.Equal(t, "object", roundtrip["type"])
	assert.Equal(t, false, roundtrip["additionalProperties"])
	props, ok := roundtrip["properties"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, props, "rootCause")
	assert.Contains(t, props, "confidence")
	assert.Contains(t, props, "steps")
}
