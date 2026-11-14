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
