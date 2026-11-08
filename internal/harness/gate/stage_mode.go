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
	"fmt"

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
	mode, err := s.selfHealingMode(ctx)
	if err != nil {
		// Fail closed: a policy that cannot be read is not the default policy.
		s.g.logUnreadable("selfHealingMode", err, "cannot read the ClusterPersona selfHealing mode; refusing until it is readable")
		d.refuse(StageModePolicy, GuardrailPolicyUnreadable,
			"the self-healing mode could not be read, so nothing can be cleared", nil)
		return nil
	}
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
		// The CRD enumerates the modes, so this is an object that predates or
		// bypassed it. It is not a mode the harness knows how to honour.
		s.g.logger.Info("unrecognized selfHealing.mode; refusing", "mode", mode,
			"persona", cand.Target.Name, "incident", cand.IncidentRef.Name)
		d.refuse(StageModePolicy, GuardrailUnknownMode,
			"the self-healing mode is not one the harness recognizes, so nothing can be cleared", numbers)
	}
	return nil
}

// modeRank orders the modes by how little they allow: a higher rank is more
// restrictive. An unrecognized mode outranks every real one.
func modeRank(mode string) int {
	switch mode {
	case dorguv1.SelfHealingModeAutoApprove:
		return 1
	case dorguv1.SelfHealingModePropose:
		return 2
	case dorguv1.SelfHealingModeObserve:
		return 3
	default:
		return 4
	}
}

// selfHealingMode resolves the cluster-wide selfHealing mode across every
// ClusterPersona: the MOST restrictive declared mode wins (observe over propose
// over auto-approve), whatever order the list comes back in, because the order
// of a list is not a policy. With none declared it is propose, the CRD default.
// A list error is returned: an unreadable policy is unknown, and unknown fails
// closed.
func (s modePolicyStage) selfHealingMode(ctx context.Context) (string, error) {
	var clusterPersonas dorguv1.ClusterPersonaList
	if err := s.g.client.List(ctx, &clusterPersonas); err != nil {
		return "", fmt.Errorf("listing ClusterPersonas: %w", err)
	}
	resolved := ""
	for i := range clusterPersonas.Items {
		policies := clusterPersonas.Items[i].Spec.Policies
		if policies == nil || policies.SelfHealing == nil || policies.SelfHealing.Mode == "" {
			continue
		}
		if mode := policies.SelfHealing.Mode; resolved == "" || modeRank(mode) > modeRank(resolved) {
			resolved = mode
		}
	}
	if resolved == "" {
		return dorguv1.SelfHealingModePropose, nil
	}
	return resolved, nil
}
