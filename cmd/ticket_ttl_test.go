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

package main

import (
	"flag"
	"testing"
	"time"

	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// Ticket v1 (P2-D9): the ticket lifetime is a flag, 15 minutes by default.

func TestTicketTTLFlag_DefaultIsTheGatesDefault(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ticketTTL != 15*time.Minute {
		t.Errorf("--ticket-ttl default = %s, want 15m", cfg.ticketTTL)
	}
	if cfg.ticketTTL != gate.DefaultTicketTTL {
		t.Errorf("--ticket-ttl default = %s, want the gate's default %s", cfg.ticketTTL, gate.DefaultTicketTTL)
	}
}

func TestTicketTTLFlag_Override(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), []string{"--ticket-ttl=5m"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ticketTTL != 5*time.Minute {
		t.Errorf("--ticket-ttl = %s, want 5m", cfg.ticketTTL)
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("5m must be accepted: %v", err)
	}
}

func TestTicketTTLFlag_ExplicitNonPositiveIsRefused(t *testing.T) {
	for _, arg := range []string{"--ticket-ttl=0", "--ticket-ttl=0s", "--ticket-ttl=-5m"} {
		if _, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), []string{arg}); err == nil {
			t.Errorf("%s must be refused: a ticket that is born expired turns every approval into a refusal", arg)
		}
	}
}

func TestValidate_TicketTTLBounds(t *testing.T) {
	for _, ttl := range []time.Duration{-time.Second, 24*time.Hour + time.Second, 100 * time.Hour} {
		if err := (operatorConfig{ticketTTL: ttl}).validate(); err == nil {
			t.Errorf("ticket ttl %s must be rejected", ttl)
		}
	}
	for _, ttl := range []time.Duration{0, time.Minute, gate.DefaultTicketTTL, 24 * time.Hour} {
		if err := (operatorConfig{ticketTTL: ttl}).validate(); err != nil {
			t.Errorf("ticket ttl %s must be accepted: %v", ttl, err)
		}
	}
}

func TestBuildHealthPipeline_GateUsesTheConfiguredTicketTTL(t *testing.T) {
	pipeline := buildForTest(t, operatorConfig{ticketTTL: 3 * time.Minute}, "dorgu-system")
	if got := pipeline.Gate.TicketTTL(); got != 3*time.Minute {
		t.Errorf("gate ticket ttl = %s, want 3m", got)
	}
}
