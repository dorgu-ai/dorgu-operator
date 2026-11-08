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
	"context"
	"flag"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// B9: the probe-detection thresholds are flags, with the detector's own defaults.

func TestProbeDetectionFlags_Defaults(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.readinessThreshold != 2*time.Minute {
		t.Errorf("readiness threshold = %s, want 2m", cfg.readinessThreshold)
	}
	if cfg.livenessRestartCount != 2 || cfg.livenessRestartWindow != 10*time.Minute {
		t.Errorf("liveness = %d in %s, want 2 in 10m", cfg.livenessRestartCount, cfg.livenessRestartWindow)
	}
}

func TestProbeDetectionFlags_Override(t *testing.T) {
	cfg, err := parseFlagSet(flag.NewFlagSet("test", flag.ContinueOnError),
		[]string{"--readiness-threshold=5m", "--liveness-restart-count=3", "--liveness-restart-window=30m"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.readinessThreshold != 5*time.Minute || cfg.livenessRestartCount != 3 ||
		cfg.livenessRestartWindow != 30*time.Minute {
		t.Errorf("flags not applied: %+v", cfg)
	}
}

func TestValidate_RejectsNegativeProbeDetectionSettings(t *testing.T) {
	for _, cfg := range []operatorConfig{
		{readinessThreshold: -time.Second},
		{livenessRestartCount: -1},
		{livenessRestartWindow: -time.Minute},
	} {
		if err := cfg.validate(); err == nil {
			t.Errorf("%+v must be rejected", cfg)
		}
	}
	if err := (operatorConfig{}).validate(); err != nil {
		t.Errorf("unset values mean the defaults and must be accepted: %v", err)
	}
}

// The flags reach the collector: a container not Ready for 3 minutes is a signal
// under the default 2 minute threshold and none under a 5 minute one.
func TestPodCollectorOptions_ThresholdReachesTheCollector(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "shop"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.ContainersReady, Status: corev1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(now.Add(-3 * time.Minute)),
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(now.Add(-10 * time.Minute)),
				}},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	filterless := func(cfg operatorConfig) int {
		collector := detection.NewPodCollector(c, logr.Discard(), podCollectorOptions(cfg, c)...)
		signals, err := collector.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(signals)
	}
	if got := filterless(operatorConfig{}); got != 1 {
		t.Errorf("default threshold: %d signals, want 1", got)
	}
	if got := filterless(operatorConfig{readinessThreshold: 5 * time.Minute}); got != 0 {
		t.Errorf("5m threshold: %d signals, want 0", got)
	}
}

// The grace before a terminated state counts as history outlasts the health-check
// interval, so a kill is always seen by at least one cycle first.
func TestTerminatedStaleAfter(t *testing.T) {
	tests := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{0, detection.DefaultTerminatedStaleAfter},
		{30 * time.Second, detection.DefaultTerminatedStaleAfter},
		{time.Minute, 2 * time.Minute},
		{5 * time.Minute, 10 * time.Minute},
	}
	for _, tt := range tests {
		if got := terminatedStaleAfter(operatorConfig{healthCheckInterval: tt.interval}); got != tt.want {
			t.Errorf("interval %s: grace %s, want %s", tt.interval, got, tt.want)
		}
	}
}

// The options never replace the deny-list filter newPodCollector installs.
func TestNewPodCollector_OptionsKeepTheDenyListFilter(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "shop"},
	}).Build()
	// A nil gate cannot answer the deny list, and the collector fails rather than
	// guess: proof the filter is still installed after the options are applied.
	collector := newPodCollector(c, nil, logr.Discard(), podCollectorOptions(operatorConfig{}, c)...)
	_, err := collector.Collect(context.Background())
	if err == nil {
		t.Fatal("the collector must still consult the gate's deny list")
	}
}

func TestClampInt32(t *testing.T) {
	cases := map[int]int32{0: 0, 7: 7, -3: -3, math.MaxInt32 + 5: math.MaxInt32, math.MinInt32 - 5: math.MinInt32}
	for in, want := range cases {
		if got := clampInt32(in); got != want {
			t.Errorf("clampInt32(%d) = %d, want %d", in, got, want)
		}
	}
}
