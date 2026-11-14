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

package chart

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Ticket v1 (P2-D9): the ticket lifetime is a chart value, remediation.ticketTTL,
// rendered as --ticket-ttl and 15m by default.

func TestTicketTTL_RendersTheDefault(t *testing.T) {
	mustContain(t, helmTemplate(t), "--ticket-ttl=15m", "fresh install")
}

func TestTicketTTL_ValueIsRendered(t *testing.T) {
	mustContain(t, helmTemplate(t, "remediation.ticketTTL=5m"), "--ticket-ttl=5m", "remediation.ticketTTL=5m")
}

func TestTicketTTL_IsDocumentedInValuesYaml(t *testing.T) {
	raw, err := os.ReadFile("values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "ticketTTL: 15m") {
		t.Error("values.yaml must declare remediation.ticketTTL: 15m")
	}
}

// helm upgrade --reuse-values keeps the old release's values, which have no
// ticketTTL: the flag still renders with the default, never empty.
func TestTicketTTL_RendersAgainstTheOldValues(t *testing.T) {
	out, err := renderAgainstValues(t)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	mustContain(t, out, "--ticket-ttl=15m", "0.12.1 values, new chart")
}

// F-11: a bad ticketTTL fails the chart, so `helm install` and `helm upgrade` stop
// with a message instead of rendering a flag the operator refuses at startup and a
// pod that crash-loops. The operator accepts a Go duration that is positive and at
// most 24h, and the chart must refuse exactly what the operator refuses.
func helmTemplateArgs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", append([]string{"template", "."}, args...)...).CombinedOutput()
	return string(out), err
}

func TestTicketTTL_RefusesWhatTheOperatorRefuses(t *testing.T) {
	cases := map[string][]string{
		"zero (a bare number)":    {"--set", "remediation.ticketTTL=0"},
		"empty":                   {"--set-string", "remediation.ticketTTL="},
		"a bare number":           {"--set", "remediation.ticketTTL=900"},
		"a bare number as string": {"--set-string", "remediation.ticketTTL=900"},
		"zero seconds":            {"--set-string", "remediation.ticketTTL=0s"},
		"zero compound":           {"--set-string", "remediation.ticketTTL=0h0m"},
		"negative":                {"--set-string", "remediation.ticketTTL=-5m"},
		"over 24h":                {"--set-string", "remediation.ticketTTL=25h"},
		"just over 24h":           {"--set-string", "remediation.ticketTTL=24h1s"},
		"over 24h in minutes":     {"--set-string", "remediation.ticketTTL=1441m"},
		"not a duration":          {"--set-string", "remediation.ticketTTL=banana"},
		"unknown unit":            {"--set-string", "remediation.ticketTTL=5d"},
		"a duration with a space": {"--set-string", "remediation.ticketTTL=5 m"},
		"a duration plus junk":    {"--set-string", "remediation.ticketTTL=5mx"},
		"a value that is a list":  {"--set", "remediation.ticketTTL={5m}"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := helmTemplateArgs(t, args...)
			if err == nil {
				t.Fatalf("helm template accepted it and rendered:\n%s", grepLine(out, "--ticket-ttl"))
			}
			if !strings.Contains(out, "remediation.ticketTTL") {
				t.Errorf("the failure must name the value:\n%s", out)
			}
		})
	}
}

func TestTicketTTL_AcceptsWhatTheOperatorAccepts(t *testing.T) {
	for _, ttl := range []string{"15m", "5m", "90s", "1h30m", "24h", "23h59m59s", "1.5h", "500ms", "1440m", "86400s"} {
		t.Run(ttl, func(t *testing.T) {
			out, err := helmTemplateArgs(t, "--set-string", "remediation.ticketTTL="+ttl)
			if err != nil {
				t.Fatalf("helm template refused %q: %v\n%s", ttl, err, out)
			}
			mustContain(t, out, "--ticket-ttl="+ttl, ttl)
		})
	}
}

func TestTicketTTL_AnUpgradeFromValuesWithoutItStillRendersTheDefault(t *testing.T) {
	out, err := renderAgainstValues(t)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	mustContain(t, out, "--ticket-ttl=15m", "0.12.1 values, new chart")
}

func grepLine(s, sub string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}
