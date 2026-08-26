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

You are given a diagnosed incident plus rich context: the LIVE workload as read
from the cluster, the affected application's persona (an imported snapshot of
desired spec, which drifts and is often stale), the cluster's self-healing
policy and environment, the recent incident history for this application, and the
OUTCOMES of past remediations (which prior fixes succeeded, failed, or were rolled back).

Produce a correct, ORDERED remediation plan by calling the submit_remediation_plan tool.

GROUNDING (the highest-priority rules; a wrong fact is worse than a missing one):
G1. The "Live workload" section is the only source of truth for what is running
   NOW. Every number, image tag, limit, request and replica count you state as
   current MUST be copied from it.
G2. The persona is a point-in-time import. Its values may be months out of date
   and may describe fields the workload does not have. NEVER present a persona
   value as the current state, and never compute from one when a live value
   exists.
G3. Size every change against the LIVE value. "No more than ~2x" means no more
   than twice what the live workload has, not twice what the persona records.
G4. Only change resource keys the live workload ALREADY sets. If the live
   container has no CPU limit, do not add one as part of an unrelated fix.
   If adding a key is genuinely the fix, it must be its own step whose
   description says plainly that a new field is being introduced and why.
G5. Never assert a version, tag or release you have not read in the context
   above. The live image and the "Prior images on record" list are the only
   versions you may name. If the right version is not among them, say you
   cannot verify it and prefer a rollback (kubectl rollout undo) or an explicit
   instruction to the operator to pick a known-good tag. Do not describe a tag
   as "latest" or "stable" unless the context says so.
G6. Do not claim to have queried anything you were not given. You did not query
   an image registry, a metrics backend, or an external API.
G7. Never say whether a change is within, under, or outside any cap, ceiling,
   guardrail or budget, and never cite a multiplier as evidence that it complies.
   Dorgu computes that ratio itself and prints its own verdict beside your text,
   so a claim like "well within a 2x ceiling" beside a 16x change is simply
   contradicted in the next line. State the values you propose and why they are
   right for the workload. Say nothing about limits on your own proposal.

OWNERSHIP (what you may tell the reader to run):
O1. The live workload section states managedBy. When it is anything other than
   "unmanaged", another system owns that Deployment's desired state.
O2. For an owned workload, NEVER suggest a command that writes to it (no
   kubectl patch, set, apply, edit, scale, replace, delete, annotate, label, or
   rollout undo/restart). A direct write takes field ownership and makes the
   owner's next apply fail or silently revert the fix.
O3. Instead, tell the reader exactly what to change in the owner's source of
   truth: the values file for a Helm release, the Git manifests for an ArgoCD
   application or a Flux source, the overlay for kustomize. Name the owner.
O4. managedBy "unknown" is treated as owned. Do not suggest workload writes.
O5. Read-only commands (kubectl get, describe, logs, top, events, rollout
   status/history) are fine for any workload.
O6. persona-update steps are unaffected by all of this. The operator patches the
   ApplicationPersona, never the workload, so those steps stay safe and
   auto-applied whoever owns the Deployment.

Rules you MUST follow:
1. Order steps by execution sequence, starting at 1.
2. Each step has a type: one of persona-update, workload-apply, restart, scale,
   config-change, manual.
3. ONLY persona-update steps are auto-applied by the operator. They MUST carry a
   "patch": a JSON merge patch against the ApplicationPersona spec, e.g.
   {"spec":{"resources":{"limits":{"memory":"512Mi"}}}}. Never put a workload
   (Deployment/Pod) patch here — the operator never writes workloads directly.
   A persona-update step with no patch applies nothing. It is not a plan, it is a
   sentence, and Dorgu will overwrite it with its own computed patch.
3a. A change to the container's CPU or memory requests or limits is ALWAYS a
   persona-update step carrying a patch. Never express a resource change as
   workload-apply: Dorgu applies resource changes by patching the
   ApplicationPersona, and the CLI takes it from there with the user's own
   credentials. workload-apply is for changes the persona cannot carry, such as
   an image tag.
3b. Emit at most ONE persona-update step, and put every persona-spec field the
   fix changes in its single patch. Do not restate the same resource change as a
   second advisory step: the reader would be told to make by hand a change that
   is already being applied for them.
4. All other step types are ADVISORY: they describe an action for a human, CLI,
   or platform to apply. Do not include a patch on them.
4a. When an advisory step can be carried out by ONE kubectl command AND the
   command is permitted for this workload's owner (see O1-O5), include it as
   "command", fully resolved with the real namespace, workload, container and
   value from the Live workload section (e.g. "kubectl set image
   deployment/web web=nginx:1.27-alpine -n demo"). A reader should be able to
   paste it and be done. Constraints: a single line, starting with "kubectl ",
   and containing none of ; & | < > $ or backticks. If no single command does
   the job, the workload is owned, or you would have to guess at a name, omit
   "command" and say what to do in the description instead. Never guess.
5. Respect the cluster's self-healing policy and trust level. Size resource
   changes conservatively: no more than about twice the LIVE value (per G3).
   Size it and move on; per G7, do not comment on whether it complies.
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
							"description": "REQUIRED on every persona-update step: a JSON merge patch (as a JSON-encoded string) against the ApplicationPersona spec, e.g. {\"spec\":{\"resources\":{\"limits\":{\"memory\":\"512Mi\"}}}}. A persona-update step without this applies nothing and will be replaced by Dorgu's own computed patch. Any change to container requests or limits belongs here, never on a workload-apply step. Omit for advisory steps.",
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

	// The live workload goes first, ahead of the persona, because it is the only
	// section that describes the present.
	writeLiveWorkload(&sb, rc.Workload)
	writeAppPersona(&sb, rc.AppPersona)
	writeClusterPersona(&sb, rc.ClusterPersona)
	writePastIncidents(&sb, rc.PastIncidents)
	writePastRemediations(&sb, rc.PastRemediations)

	return sb.String()
}

// writeLiveWorkload renders the ground-truth section: what is actually running,
// which resource keys it actually sets, and who owns it.
func writeLiveWorkload(sb *strings.Builder, w *WorkloadContext) {
	sb.WriteString("## Live workload (ground truth, read from the cluster just now)\n")
	if w == nil || w.Ref == nil {
		sb.WriteString("(no Deployment could be resolved for this application)\n")
		sb.WriteString("Because the live workload is unreadable, you may not state any current\n")
		sb.WriteString("resource value, image tag or replica count as fact. Say what you cannot see.\n\n")
		return
	}

	ref := w.Ref
	fmt.Fprintf(sb, "%s: %s/%s | container: %s\n", ref.Kind, ref.Namespace, ref.Name, ref.Container)
	fmt.Fprintf(sb, "Image (running now): %s\n", valueOrNone(ref.ObservedImage))
	fmt.Fprintf(sb, "Replicas: desired=%d ready=%d\n", w.Replicas, w.ReadyReplicas)

	writeObservedResources(sb, ref.ObservedResources)

	if len(w.PriorImages) > 0 {
		fmt.Fprintf(sb, "Prior images on record (read by Dorgu, safe to name): %s\n",
			strings.Join(w.PriorImages, ", "))
	} else {
		sb.WriteString("Prior images on record: (none) - you have no verified previous tag to roll back to.\n")
	}

	owner := ref.ManagedBy
	if ref.ManagedByDetail != "" {
		owner = fmt.Sprintf("%s (%s)", ref.ManagedBy, ref.ManagedByDetail)
	}
	fmt.Fprintf(sb, "managedBy: %s\n", owner)
	if ref.IsOwned() {
		sb.WriteString("This workload is OWNED. Do not suggest any command that writes to it; " +
			"tell the reader what to change in the owner's source of truth instead.\n")
	} else {
		sb.WriteString("Nothing reconciles this workload, so a direct kubectl command is the right " +
			"instruction to give.\n")
	}
	sb.WriteString("\n")
}

// writeObservedResources spells out both the values and the absences, because
// "the container sets no CPU limit" is the fact that stops a memory fix from
// quietly introducing one.
func writeObservedResources(sb *strings.Builder, res *dorguv1.ObservedResources) {
	if res == nil {
		sb.WriteString("Live resources: the container sets NO requests and NO limits. " +
			"Do not add one as a side effect of another fix.\n")
		return
	}
	fmt.Fprintf(sb, "Live limits: %s\n", describeResourceValues(res.Limits))
	fmt.Fprintf(sb, "Live requests: %s\n", describeResourceValues(res.Requests))
}

// describeResourceValues renders a live resource pair, naming absent keys
// explicitly rather than printing an empty value that reads as zero.
func describeResourceValues(v *dorguv1.ResourceValues) string {
	if v == nil {
		return "none set (adding one would introduce a field the workload does not have)"
	}
	parts := make([]string, 0, 2)
	if v.CPU != "" {
		parts = append(parts, "cpu="+v.CPU)
	} else {
		parts = append(parts, "cpu=NOT SET (do not introduce)")
	}
	if v.Memory != "" {
		parts = append(parts, "memory="+v.Memory)
	} else {
		parts = append(parts, "memory=NOT SET (do not introduce)")
	}
	return strings.Join(parts, " ")
}

func valueOrNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func writeAppPersona(sb *strings.Builder, p *dorguv1.ApplicationPersona) {
	sb.WriteString("## Application persona (imported snapshot of INTENT, may be stale)\n")
	if p == nil {
		sb.WriteString("(unavailable)\n\n")
		return
	}
	fmt.Fprintf(sb, "Name: %s | Type: %s | Tier: %s\n", p.Spec.Name, p.Spec.Type, p.Spec.Tier)
	if r := p.Spec.Resources; r != nil && r.Limits != nil {
		fmt.Fprintf(sb, "Persona-recorded limits (NOT current): cpu=%s memory=%s\n", r.Limits.CPU, r.Limits.Memory)
	}
	if r := p.Spec.Resources; r != nil && r.Requests != nil {
		fmt.Fprintf(sb, "Persona-recorded requests (NOT current): cpu=%s memory=%s\n", r.Requests.CPU, r.Requests.Memory)
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
