/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClusterPersonaSpec_SelfHealingJSONPath documents that self-healing settings live under
// spec.policies.selfHealing (not spec.selfHealing). kubectl:
//
//	kubectl get clusterpersona <name> -o jsonpath='{.spec.policies.selfHealing.mode}'
func TestClusterPersonaSpec_SelfHealingJSONPath(t *testing.T) {
	raw := `{
		"name": "qa-helm",
		"environment": "development",
		"policies": {
			"selfHealing": {
				"enabled": true,
				"mode": "observe",
				"trustLevel": 2,
				"maxRemediationsPerHour": 5
			}
		}
	}`

	var spec ClusterPersonaSpec
	require.NoError(t, json.Unmarshal([]byte(raw), &spec))
	require.NotNil(t, spec.Policies)
	require.NotNil(t, spec.Policies.SelfHealing)
	require.Equal(t, "observe", spec.Policies.SelfHealing.Mode)
	require.Equal(t, int32(2), spec.Policies.SelfHealing.TrustLevel)
}
