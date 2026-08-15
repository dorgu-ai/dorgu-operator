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

// This file covers NOTES.txt (F-20). The chart used to install in silence: it
// succeeded and told the user nothing about what to do next, or that detection
// was off. The notes have to state the state the install actually landed in,
// not a generic congratulations.
package chart

import (
	"os/exec"
	"strings"
	"testing"
)

// helmNotes renders the chart's NOTES.txt with the given --set overrides.
// `helm template` omits notes, so this goes through a client-side dry-run
// install and returns everything from the NOTES: marker onward.
func helmNotes(t *testing.T, sets ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed on PATH; skipping chart notes test")
	}
	args := []string{"install", "rel", ".", "--dry-run=client", "--namespace", "dorgu-system"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm install --dry-run %v failed: %v\n%s", sets, err, out)
	}
	rendered := string(out)
	idx := strings.Index(rendered, "NOTES:")
	if idx < 0 {
		t.Fatalf("chart rendered no NOTES.txt:\n%s", rendered)
	}
	return rendered[idx:]
}

// TestNotesTellTheUserWhatToDoNext asserts the notes name the next commands
// rather than leaving a fresh install to guess.
func TestNotesTellTheUserWhatToDoNext(t *testing.T) {
	notes := helmNotes(t)

	for _, want := range []string{
		"kubectl rollout status deployment/rel-dorgu-operator -n dorgu-system",
		"dorgu health",
		"dorgu persona import",
		"dorgu incidents list",
		"dorgu remediation diff",
		"dorgu remediation approve",
	} {
		mustContain(t, notes, want, "notes must name the next step")
	}
}

// TestNotesReportDetectionState is the F-20 heart: an operator installed with
// detection off is doing nothing, and the notes have to say so instead of
// implying the install is complete.
func TestNotesReportDetectionState(t *testing.T) {
	t.Run("off by default states it and gives the fix", func(t *testing.T) {
		notes := helmNotes(t, "healthCheck.enabled=false")
		mustContain(t, notes, "Failure detection is OFF", "detection state")
		mustContain(t, notes, "Nothing will be detected until you turn it on", "consequence")
		mustContain(t, notes, "--set healthCheck.enabled=true", "the fix")
	})

	t.Run("on states it with the interval", func(t *testing.T) {
		notes := helmNotes(t, "healthCheck.enabled=true", "healthCheck.interval=30s")
		mustContain(t, notes, "Failure detection is ON", "detection state")
		mustContain(t, notes, "every 30s", "the configured interval")
		mustNotContain(t, notes, "Nothing will be detected", "no off-state copy when on")
	})
}

// TestNotesReportAIState covers the second half: AI is opt-in, and the notes
// must not imply a key-less install is getting AI diagnosis.
func TestNotesReportAIState(t *testing.T) {
	t.Run("off by default and explains why", func(t *testing.T) {
		notes := helmNotes(t)
		mustContain(t, notes, "AI diagnosis and planning is OFF", "AI state")
		mustContain(t, notes, "Rule-based detection and diagnosis work with no API key", "the no-key floor")
		mustContain(t, notes, "--set aiRemediation.enabled=true", "the opt-in")
	})

	t.Run("on only when both provider and remediation are set", func(t *testing.T) {
		notes := helmNotes(t, "llm.provider=claude", "aiRemediation.enabled=true")
		mustContain(t, notes, "AI diagnosis and planning is ON", "AI state")
		mustContain(t, notes, "Provider: claude", "the configured provider")
	})

	t.Run("a provider alone is not AI on", func(t *testing.T) {
		notes := helmNotes(t, "llm.provider=claude")
		mustContain(t, notes, "AI diagnosis and planning is OFF", "aiRemediation is the gate")
	})
}

// TestNotesPreserveTheApprovalInvariant guards the product's core promise
// against a copy edit: the notes must not suggest the operator changes
// workloads on its own.
func TestNotesPreserveTheApprovalInvariant(t *testing.T) {
	notes := helmNotes(t)
	mustContain(t, notes, "Nothing is applied without your approval", "approval promise")
	mustContain(t, notes, "The operator only ever writes", "operator never writes workloads")
}
