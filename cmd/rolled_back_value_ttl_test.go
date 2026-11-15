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

// C6b (review F-3): the memory of a value a person rolled back ends. Its lifetime is a
// flag, 7 days by default.

func TestRolledBackValueTTLFlag_DefaultIsTheGatesDefault(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.rolledBackValueTTL != 7*24*time.Hour {
		t.Errorf("--rolled-back-value-ttl default = %s, want 7 days", cfg.rolledBackValueTTL)
	}
	if cfg.rolledBackValueTTL != gate.DefaultRolledBackValueTTL {
		t.Errorf("--rolled-back-value-ttl default = %s, want the gate's default %s",
			cfg.rolledBackValueTTL, gate.DefaultRolledBackValueTTL)
	}
}

func TestRolledBackValueTTLFlag_Override(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), []string{"--rolled-back-value-ttl=48h"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.rolledBackValueTTL != 48*time.Hour {
		t.Errorf("--rolled-back-value-ttl = %s, want 48h", cfg.rolledBackValueTTL)
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("48h must be accepted: %v", err)
	}
}

func TestRolledBackValueTTLFlag_ExplicitNonPositiveIsRefused(t *testing.T) {
	for _, arg := range []string{
		"--rolled-back-value-ttl=0", "--rolled-back-value-ttl=0s", "--rolled-back-value-ttl=-24h",
	} {
		if _, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), []string{arg}); err == nil {
			t.Errorf("%s must be refused: a memory that lapses before it begins forgets every rollback at once", arg)
		}
	}
}

func TestValidate_RolledBackValueTTLBounds(t *testing.T) {
	for _, ttl := range []time.Duration{-time.Second, maxRolledBackValueTTL + time.Second, 10000 * time.Hour} {
		if err := (operatorConfig{rolledBackValueTTL: ttl}).validate(); err == nil {
			t.Errorf("rolled-back value ttl %s must be rejected", ttl)
		}
	}
	for _, ttl := range []time.Duration{0, time.Hour, gate.DefaultRolledBackValueTTL, maxRolledBackValueTTL} {
		if err := (operatorConfig{rolledBackValueTTL: ttl}).validate(); err != nil {
			t.Errorf("rolled-back value ttl %s must be accepted: %v", ttl, err)
		}
	}
}

func TestBuildHealthPipeline_GateUsesTheConfiguredRolledBackValueTTL(t *testing.T) {
	pipeline := buildForTest(t, operatorConfig{rolledBackValueTTL: 36 * time.Hour}, "dorgu-system")
	if got := pipeline.Gate.RolledBackValueTTL(); got != 36*time.Hour {
		t.Errorf("gate rolled-back value ttl = %s, want 36h", got)
	}
}
