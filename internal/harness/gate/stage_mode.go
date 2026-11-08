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

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// SkipReasonObserveMode is the reason (and log line) used when
// spec.policies.selfHealing.mode is observe. Observe is the cautious first-run
// posture: detection, diagnosis, and IncidentMemory still happen; only the
// RemediationAction is withheld.
const SkipReasonObserveMode = "selfHealing.mode=observe — proposal suppressed"

// modePolicyStage honours spec.policies.selfHealing.mode. It is also the reserved
// hook for approval policies: auto-approve is NOT implemented, so it is logged and
// treated as propose, with human approval still required.
type modePolicyStage struct{ g *Gate }

func (modePolicyStage) Name() string { return StageModePolicy }

func (s modePolicyStage) Run(ctx context.Context, cand *Candidate, d *Decision) error {
	mode := s.selfHealingMode(ctx)
	numbers := map[string]string{"mode": mode}

	switch mode {
	case dorguv1.SelfHealingModeObserve:
		s.g.logger.Info(SkipReasonObserveMode,
			"persona", cand.Target.Name, "incident", cand.IncidentRef.Name,
			"hint", "set spec.policies.selfHealing.mode=propose on the ClusterPersona to receive remediation proposals")
		d.refuse(StageModePolicy, GuardrailSelfHealingMode, SkipReasonObserveMode, numbers)

	case dorguv1.SelfHealingModeAutoApprove:
		s.g.logger.Info("selfHealing.mode=auto-approve is not implemented — treating as propose; human approval is still required",
			"persona", cand.Target.Name, "incident", cand.IncidentRef.Name)
		d.pass(StageModePolicy, GuardrailSelfHealingMode,
			"auto-approve is not implemented; treated as propose, human approval still required", numbers)

	case dorguv1.SelfHealingModePropose:
		d.pass(StageModePolicy, GuardrailSelfHealingMode, "mode is propose", numbers)

	default:
		s.g.logger.Info("unrecognized selfHealing.mode — treating as propose",
			"mode", mode, "persona", cand.Target.Name, "incident", cand.IncidentRef.Name)
		d.pass(StageModePolicy, GuardrailSelfHealingMode, "unrecognized mode treated as propose", numbers)
	}
	return nil
}

// selfHealingMode resolves the cluster-wide selfHealing mode: the first
// ClusterPersona that declares one wins. It falls back to propose, the CRD
// default, when none does or the list fails, so a lookup error degrades to the
// documented default rather than silently disabling healing. (The Guardrails
// stage fails closed on the same list, so a persistent failure still stops
// proposals.)
func (s modePolicyStage) selfHealingMode(ctx context.Context) string {
	var clusterPersonas dorguv1.ClusterPersonaList
	if err := s.g.client.List(ctx, &clusterPersonas); err != nil {
		s.g.logger.V(1).Info("failed to list ClusterPersonas for selfHealing mode, assuming propose", "error", err)
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
