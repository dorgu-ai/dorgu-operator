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
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// systemUserPrefix marks a ServiceAccount or a Kubernetes component: never a person.
const systemUserPrefix = "system:"

// approverProblem says why status.approvedBy cannot be the decision of a person other than
// the one who proposed the action, or "" when it can. An empty approver is the gate's to
// refuse (NoApprover), not this check's. It is the operator's second look at
// what the admission policy dorgu-approval-gate already enforces (task 2.6, defence in
// depth): an approval that got in while the policy was missing, edited or not yet noticed
// must not become an apply ticket.
//
// The value is compared as stored and as the ticket will carry it (the ticket trims
// whitespace), so padding cannot make a name differ from the proposer in one place and match
// it in the other. An action with no recorded proposer was proposed by the operator, whose
// username (operator) is a "system:" name and so refused as an approver in any case.
func approverProblem(action *dorguv1.RemediationAction, operator string) string {
	raw := action.Status.ApprovedBy
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		// Nobody is named. The gate refuses that, terminally and before a ticket, with its
		// own NoApprover reason (the documented behaviour), so it is left to it.
		return ""
	}
	if strings.HasPrefix(raw, systemUserPrefix) || strings.HasPrefix(trimmed, systemUserPrefix) {
		return "status.approvedBy is a service account or system user, and an approval is a person's decision"
	}
	proposer := operator
	if p := action.Spec.Provenance; p != nil {
		proposer = p.Proposer.Username
	}
	if proposer == "" {
		return ""
	}
	if raw == proposer || trimmed == proposer {
		return "the person who proposed this remediation cannot be the one who approves it"
	}
	return ""
}

// refuseApprover ends an approval whose approver is not allowed to give it: Failed with the
// GateRefusedAtApproval reason and a GateRefused condition naming why. Nothing was applied,
// so no cooldown and no outcome is booked, as for every other refusal at approval. It is
// terminal because the same approval is refused the same way on every retry.
func (r *RemediationController) refuseApprover(
	ctx context.Context, logger logr.Logger, action *dorguv1.RemediationAction, problem string,
) error {
	message := fmt.Sprintf("refused at approval (%s): %s; nothing was applied. "+
		"Someone else has to approve a new proposal for it", dorguv1.ReasonApproverNotPermitted, problem)
	logger.Info("the approval names an approver that may not give it; ending it without a ticket", "problem", problem)

	setCondition(&action.Status.Conditions, dorguv1.ConditionGateRefused, metav1.ConditionTrue,
		dorguv1.ReasonApproverNotPermitted, message)
	if err := r.transitionToFailedWithReason(ctx, action, dorguv1.ReasonGateRefusedAtApproval, message); err != nil {
		return err
	}
	r.reportApprovalRefusal(action, gate.StageGuardrails, message)
	return nil
}
