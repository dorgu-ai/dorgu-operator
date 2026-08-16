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
	"fmt"
	"strings"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// planSystemPrompt instructs the model to act as an SRE remediation planner and
// emit an ordered plan through the structured-output tool. It encodes the
// non-negotiable safety contract: only persona-spec changes are auto-applied;
// everything else is advisory.
const planSystemPrompt = `You are a senior Site Reliability Engineer acting as a Kubernetes remediation planner.

You are given a diagnosed incident plus rich context: the affected application's
persona (desired spec and learned resource baselines), the cluster's self-healing
policy and environment, the recent incident history for this application, and the
OUTCOMES of past remediations (which prior fixes succeeded, failed, or were rolled back).

Produce a correct, ORDERED remediation plan by calling the submit_remediation_plan tool.

Rules you MUST follow:
1. Order steps by execution sequence, starting at 1.
2. Each step has a type: one of persona-update, workload-apply, restart, scale,
   config-change, manual.
3. ONLY persona-update steps are auto-applied by the operator. They MUST carry a
   "patch": a JSON merge patch against the ApplicationPersona spec, e.g.
   {"spec":{"resources":{"limits":{"memory":"512Mi"}}}}. Never put a workload
   (Deployment/Pod) patch here — the operator never writes workloads directly.
4. All other step types are ADVISORY: they describe an action for a human, CLI,
   or platform to apply. Do not include a patch on them.
4a. When an advisory step can be carried out by ONE kubectl command, include it
   as "command", fully resolved with the real namespace, workload, container and
   value from the context above (e.g. "kubectl set image deployment/web
   web=nginx:1.27-alpine -n demo"). A reader should be able to paste it and be
   done. Constraints: a single line, starting with "kubectl ", and containing
   none of ; & | < > $ or backticks. If no single command does the job, or you
   would have to guess at a name, omit "command" and say what to do in the
   description instead. Never guess.
5. Respect the cluster's self-healing policy and trust level. Keep resource
   changes conservative (no more than ~2x an existing limit).
6. PREFER approaches that succeeded in past remediations for this app; AVOID
   approaches that previously failed or were rolled back.
7. Give a concise rootCause and a confidence between 0 and 1.
8. Each step needs a short description, a rationale, and a risk (low|medium|high).

Be specific and actionable. Do not include caveats about being an AI.`

// planToolName is the name of the structured-output tool the model must call.
const planToolName = "submit_remediation_plan"

// planToolDescription describes the tool to the model.
const planToolDescription = "Submit the ordered remediation plan as structured data. Call this exactly once."

// planToolSchema is the JSON Schema for the remediation plan tool input. It is
// declared with strict validation so the model's tool input is guaranteed to
// match (see claude.go). The schema mirrors RemediationPlan / PlannedStep.
func planToolSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"rootCause": map[string]any{
				"type":        "string",
				"description": "Concise root-cause explanation for the incident.",
			},
			"confidence": map[string]any{
				"type":        "number",
				"description": "Confidence in the plan, between 0 and 1.",
			},
			"steps": map[string]any{
				"type":        "array",
				"description": "Ordered remediation steps.",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"order": map[string]any{
							"type":        "integer",
							"description": "1-based execution order.",
						},
						"type": map[string]any{
							"type":        "string",
							"enum":        []string{"persona-update", "workload-apply", "restart", "scale", "config-change", "manual"},
							"description": "Step type. Only persona-update is auto-applied.",
						},
						"description": map[string]any{
							"type":        "string",
							"description": "Human-readable summary of the action.",
						},
						"rationale": map[string]any{
							"type":        "string",
							"description": "Why this step is proposed.",
						},
						"risk": map[string]any{
							"type":        "string",
							"enum":        []string{"low", "medium", "high"},
							"description": "Assessed risk of applying this step.",
						},
						"patch": map[string]any{
							"type":        "string",
							"description": "For persona-update steps only: a JSON merge patch (as a JSON-encoded string) against the ApplicationPersona spec, e.g. {\"spec\":{\"resources\":{\"limits\":{\"memory\":\"512Mi\"}}}}. Omit for advisory steps.",
						},
						"command": map[string]any{
							"type":        "string",
							"description": "For advisory steps only: one ready-to-run kubectl command that carries out this step, fully resolved with real names, e.g. \"kubectl set image deployment/web web=nginx:1.27-alpine -n demo\". Single line, must start with \"kubectl \", must not contain ; & | < > $ or backticks. Omit when no single command does the job or a name would have to be guessed.",
						},
					},
					"required": []string{"order", "type", "description", "rationale", "risk"},
				},
			},
		},
		"required": []string{"rootCause", "confidence", "steps"},
	}
}

// buildPlanUserMessage renders the RemediationContext into the user-message text
// the planner reasons over. It deliberately summarizes the CRDs down to the
// fields that matter for planning rather than dumping full objects, to stay
// within a sensible token budget.
func buildPlanUserMessage(rc RemediationContext) string {
	var sb strings.Builder

	d := rc.Diagnosis
	fmt.Fprintf(&sb, "## Diagnosis\n")
	fmt.Fprintf(&sb, "Summary: %s\n", d.Summary)
	fmt.Fprintf(&sb, "Category: %s | Severity: %s | Confidence: %.2f | Suggested action: %s\n\n",
		d.Category, d.Severity, d.Confidence, d.SuggestedAction)

	sb.WriteString("## Signals\n")
	if len(rc.Signals) == 0 {
		sb.WriteString("(none)\n")
	}
	for _, s := range rc.Signals {
		line := fmt.Sprintf("- [%s] %s", s.Type, s.Message)
		if s.Resource.Kind != "" {
			line += fmt.Sprintf(" (resource: %s/%s)", s.Resource.Kind, s.Resource.Name)
		}
		if s.Value != nil {
			line += fmt.Sprintf(" (value: %.1f)", *s.Value)
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("\n")

	writeAppPersona(&sb, rc.AppPersona)
	writeClusterPersona(&sb, rc.ClusterPersona)
	writePastIncidents(&sb, rc.PastIncidents)
	writePastRemediations(&sb, rc.PastRemediations)

	return sb.String()
}

func writeAppPersona(sb *strings.Builder, p *dorguv1.ApplicationPersona) {
	sb.WriteString("## Application persona\n")
	if p == nil {
		sb.WriteString("(unavailable)\n\n")
		return
	}
	fmt.Fprintf(sb, "Name: %s | Type: %s | Tier: %s\n", p.Spec.Name, p.Spec.Type, p.Spec.Tier)
	if r := p.Spec.Resources; r != nil && r.Limits != nil {
		fmt.Fprintf(sb, "Current limits: cpu=%s memory=%s\n", r.Limits.CPU, r.Limits.Memory)
	}
	if r := p.Spec.Resources; r != nil && r.Requests != nil {
		fmt.Fprintf(sb, "Current requests: cpu=%s memory=%s\n", r.Requests.CPU, r.Requests.Memory)
	}
	if learned := p.Status.Learned; learned != nil && learned.ResourceBaseline != nil {
		b := learned.ResourceBaseline
		fmt.Fprintf(sb, "Learned baseline: avgCPU=%s avgMemory=%s peakCPU=%s peakMemory=%s\n",
			b.AvgCPU, b.AvgMemory, b.PeakCPU, b.PeakMemory)
	}
	sb.WriteString("\n")
}

func writeClusterPersona(sb *strings.Builder, cp *dorguv1.ClusterPersona) {
	sb.WriteString("## Cluster policy\n")
	if cp == nil {
		sb.WriteString("(no ClusterPersona configured)\n\n")
		return
	}
	fmt.Fprintf(sb, "Environment: %s\n", cp.Spec.Environment)
	if cp.Spec.Policies != nil && cp.Spec.Policies.SelfHealing != nil {
		sh := cp.Spec.Policies.SelfHealing
		fmt.Fprintf(sb, "Self-healing: enabled=%t mode=%s trustLevel=%d maxRemediationsPerHour=%d\n",
			sh.Enabled, sh.Mode, sh.TrustLevel, sh.MaxRemediationsPerHour)
		if len(sh.ExcludeNamespaces) > 0 {
			fmt.Fprintf(sb, "Excluded namespaces: %s\n", strings.Join(sh.ExcludeNamespaces, ", "))
		}
	}
	sb.WriteString("\n")
}

func writePastIncidents(sb *strings.Builder, incidents []dorguv1.IncidentMemory) {
	sb.WriteString("## Past incidents for this application\n")
	if len(incidents) == 0 {
		sb.WriteString("(none)\n\n")
		return
	}
	for i := range incidents {
		im := &incidents[i]
		occurrences := im.Status.OccurrenceCount
		fmt.Fprintf(sb, "- signal=%s category=%s severity=%s phase=%s occurrences=%d\n",
			im.Spec.Detection.Signal, im.Spec.Category, im.Spec.Severity, im.Status.Phase, occurrences)
	}
	sb.WriteString("\n")
}

func writePastRemediations(sb *strings.Builder, remediations []dorguv1.RemediationAction) {
	sb.WriteString("## Past remediation outcomes\n")
	if len(remediations) == 0 {
		sb.WriteString("(none)\n\n")
		return
	}
	for i := range remediations {
		ra := &remediations[i]
		phase := ra.Status.Phase
		if phase == "" {
			phase = "Unknown"
		}
		verification := ra.Status.VerificationResult
		if verification == "" {
			verification = "n/a"
		}
		fmt.Fprintf(sb, "- type=%s phase=%s verification=%s explanation=%q\n",
			ra.Spec.Action.Type, phase, verification, ra.Spec.Explanation)
	}
	sb.WriteString("\n")
}
