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

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// skipReasonObserveMode is the SkipReason (and log line) used when
// spec.policies.selfHealing.mode is observe. Observe is the cautious first-run
// posture: detection, diagnosis, and IncidentMemory still happen — only the
// RemediationAction is withheld.
const skipReasonObserveMode = "selfHealing.mode=observe — proposal suppressed"

// selfHealingMode resolves the cluster-wide selfHealing mode from the
// ClusterPersona, mirroring the safety checker's lookup: the first persona that
// declares a mode wins.
//
// It falls back to SelfHealingModePropose — the CRD default — when no
// ClusterPersona exists, none declares a mode, or the List fails, so a lookup
// error degrades to the documented default rather than silently disabling
// healing.
func (p *Proposer) selfHealingMode(ctx context.Context) string {
	var clusterPersonas dorguv1.ClusterPersonaList
	if err := p.client.List(ctx, &clusterPersonas); err != nil {
		p.logger.V(1).Info("failed to list ClusterPersonas for selfHealing mode, assuming propose",
			"error", err)
		return dorguv1.SelfHealingModePropose
	}

	for i := range clusterPersonas.Items {
		policies := clusterPersonas.Items[i].Spec.Policies
		if policies == nil || policies.SelfHealing == nil {
			continue
		}
		if mode := policies.SelfHealing.Mode; mode != "" {
			return mode
		}
	}

	return dorguv1.SelfHealingModePropose
}

// proposalSuppressedByMode reports whether selfHealing.mode forbids creating a
// RemediationAction, returning the skip reason when it does. It is the single
// place the mode is honored, and it runs before the AI planner so observe costs
// nothing.
//
// auto-approve is NOT implemented: rather than silently behaving like propose, it
// logs a prominent warning and then proposes with approval still required. An
// unrecognized mode (only reachable if CRD enum validation is bypassed) is also
// treated as propose, and logged.
func (p *Proposer) proposalSuppressedByMode(ctx context.Context, personaName, incidentName string) string {
	switch mode := p.selfHealingMode(ctx); mode {
	case dorguv1.SelfHealingModeObserve:
		p.logger.Info(skipReasonObserveMode,
			"persona", personaName, "incident", incidentName,
			"hint", "set spec.policies.selfHealing.mode=propose on the ClusterPersona to receive remediation proposals")
		return skipReasonObserveMode

	case dorguv1.SelfHealingModeAutoApprove:
		p.logger.Info("selfHealing.mode=auto-approve is not implemented — treating as propose; human approval is still required",
			"persona", personaName, "incident", incidentName)
		return ""

	case dorguv1.SelfHealingModePropose:
		return ""

	default:
		p.logger.Info("unrecognized selfHealing.mode — treating as propose",
			"mode", mode, "persona", personaName, "incident", incidentName)
		return ""
	}
}

// incidentName returns the incident's name, or a placeholder when the proposal
// has no incident attached, so log lines stay readable.
func incidentName(incident *dorguv1.IncidentMemory) string {
	if incident == nil {
		return "<none>"
	}
	return incident.Name
}
