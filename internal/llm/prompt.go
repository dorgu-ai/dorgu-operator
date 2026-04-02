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

package llm

import (
	"fmt"
	"strings"
)

const systemPrompt = `You are a Kubernetes incident diagnosis expert. You are given the output of a
deterministic rule-based diagnosis engine and the raw detection signals.

Your job is to:
1. Provide a clearer, more actionable explanation of the root cause
2. Add context about why this happened and what to watch for
3. Confirm or refine the suggested remediation action
4. Note any patterns from incident history

Be concise (2-4 sentences). Focus on actionable insight, not generic advice.
Do not include caveats about being an AI.`

// buildUserMessage creates the structured diagnosis prompt from a request.
func buildUserMessage(req DiagnosisRequest) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Rule-based diagnosis:\n%s\n\n", req.Summary)
	fmt.Fprintf(&sb, "Category: %s | Severity: %s | Confidence: %.2f\n", req.Category, req.Severity, req.Confidence)
	fmt.Fprintf(&sb, "Suggested action: %s\n\n", req.SuggestedAction)

	sb.WriteString("Detection signals:\n")
	for _, s := range req.Signals {
		line := fmt.Sprintf("- [%s] %s", s.Type, s.Message)
		if s.Resource != "" {
			line += fmt.Sprintf(" (resource: %s)", s.Resource)
		}
		if s.Value != "" {
			line += fmt.Sprintf(" (value: %s)", s.Value)
		}
		sb.WriteString(line + "\n")
	}

	sb.WriteString(fmt.Sprintf("\nIncident history: %d previous occurrences\n", req.PreviousOccurrences))
	if len(req.PreviousResolutions) > 0 {
		sb.WriteString("Previous resolutions: " + strings.Join(req.PreviousResolutions, ", ") + "\n")
	} else {
		sb.WriteString("Previous resolutions: none\n")
	}

	return sb.String()
}
