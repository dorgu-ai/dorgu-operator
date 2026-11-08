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
	"strings"
	"testing"
	"time"

	"github.com/dorgu-ai/dorgu-operator/internal/controller"
)

// B9 fix round: the recurrence cap and the retention are flags, and a liveness
// window the cycle cannot sample is refused at startup (review F-3, F-6).

func TestRecurrenceAndRetentionFlags_Defaults(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.recurrenceCap != 3 || cfg.recurrenceWindow != 24*time.Hour {
		t.Errorf("recurrence cap = %d in %s, want 3 in 24h", cfg.recurrenceCap, cfg.recurrenceWindow)
	}
	if cfg.resolvedRetention != 30*24*time.Hour {
		t.Errorf("resolved retention = %s, want 720h", cfg.resolvedRetention)
	}
	if cfg.recurrenceCap != controller.DefaultRecurrenceCap || cfg.resolvedRetention != controller.DefaultResolvedRetention {
		t.Error("the flag defaults must be the controller's defaults")
	}
}

func TestRecurrenceAndRetentionFlags_Override(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), []string{
		"--incident-recurrence-cap=5", "--incident-recurrence-window=6h", "--resolved-incident-retention=48h",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.recurrenceCap != 5 || cfg.recurrenceWindow != 6*time.Hour || cfg.resolvedRetention != 48*time.Hour {
		t.Errorf("flags not applied: %+v", cfg)
	}
}

func TestValidate_RejectsNegativeRecurrenceAndRetention(t *testing.T) {
	for _, cfg := range []operatorConfig{
		{recurrenceCap: -1}, {recurrenceWindow: -time.Hour}, {resolvedRetention: -time.Hour},
	} {
		if err := cfg.validate(); err == nil {
			t.Errorf("%+v must be rejected", cfg)
		}
	}
	// Zero disables the cap and the pruning, and is valid.
	if err := (operatorConfig{}).validate(); err != nil {
		t.Errorf("zero values must be accepted: %v", err)
	}
}

// F-6: a window shorter than two health-check cycles leaves at most one sample in
// it, so a delta can never be computed and the liveness signal could never fire.
func TestValidate_RejectsLivenessWindowShorterThanTwoCycles(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		window   time.Duration
		wantErr  bool
	}{
		{"defaults", time.Minute, 10 * time.Minute, false},
		{"exactly two cycles", time.Minute, 2 * time.Minute, false},
		{"under two cycles", time.Minute, 90 * time.Second, true},
		{"shorter than one cycle", time.Minute, 30 * time.Second, true},
		{"the default window against a slow cycle", 6 * time.Minute, 0, true},
		{"a long window is fine for a slow cycle", 6 * time.Minute, 15 * time.Minute, false},
		{"the default interval and the default window", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := operatorConfig{enableHealthCheck: true, healthCheckInterval: tt.interval, livenessRestartWindow: tt.window}
			err := cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--liveness-restart-window") {
				t.Errorf("the error must name the flag, got %q", err)
			}
		})
	}
}

func TestValidate_LivenessWindowIsNotCheckedWhenDetectionIsOff(t *testing.T) {
	cfg := operatorConfig{enableHealthCheck: false, healthCheckInterval: time.Minute, livenessRestartWindow: time.Second}
	if err := cfg.validate(); err != nil {
		t.Errorf("no detection runs, so the window is moot: %v", err)
	}
}

func TestDefaultFlagsPassValidation(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("the defaults must validate: %v", err)
	}
}
