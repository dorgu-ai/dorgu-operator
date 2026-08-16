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

package diagnosis

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// helper to create a float64 pointer.
func floatPtr(v float64) *float64 { return &v }

func newTestProvider() *RuleBasedProvider {
	return NewRuleBasedProvider(logr.Discard())
}

func TestRuleBasedProvider_Name(t *testing.T) {
	p := newTestProvider()
	if got := p.Name(); got != providerNameRuleBased {
		t.Errorf("Name() = %q, want %q", got, providerNameRuleBased)
	}
}

func TestRuleBasedProvider_EmptySignals(t *testing.T) {
	p := newTestProvider()
	diagnoses, err := p.Diagnose(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) != 0 {
		t.Errorf("expected 0 diagnoses, got %d", len(diagnoses))
	}
}

func TestRuleBasedProvider_OOMKilledStandalone(t *testing.T) {
	p := newTestProvider()
	signals := []detection.Signal{
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Category:   detection.CategoryResource,
			Source:     "pod-detector",
			Message:    "Container killed due to OOM",
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "api-server-abc", Namespace: "default"},
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) < 1 {
		t.Fatal("expected at least 1 diagnosis")
	}

	d := diagnoses[0]
	if !strings.Contains(d.Summary, "OOM-killed") {
		t.Errorf("summary should mention OOM-killed, got: %s", d.Summary)
	}
	if d.Confidence < 0.60 || d.Confidence > 0.75 {
		t.Errorf("standalone OOM confidence = %v, expected ~0.70", d.Confidence)
	}
	if d.SuggestedAction != actionResourceAdjustment {
		t.Errorf("SuggestedAction = %q, want %q", d.SuggestedAction, actionResourceAdjustment)
	}
	if d.Provider != providerNameRuleBased {
		t.Errorf("Provider = %q, want %q", d.Provider, providerNameRuleBased)
	}
}

func TestRuleBasedProvider_OOMWithMemoryUsage(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Category:   detection.CategoryResource,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "api-pod", Namespace: "prod"},
			DetectedAt: now,
		},
		{
			Type:       detection.SignalMemoryUsageHigh,
			Severity:   detection.SeverityWarning,
			Category:   detection.CategoryResource,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "api-pod", Namespace: "prod"},
			Value:      floatPtr(97),
			DetectedAt: now.Add(-10 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diagnoses) < 1 {
		t.Fatal("expected at least 1 diagnosis")
	}

	d := diagnoses[0]
	if !strings.Contains(d.Summary, "insufficient") {
		t.Errorf("summary should mention insufficient, got: %s", d.Summary)
	}
	if !strings.Contains(d.Summary, "97%") {
		t.Errorf("summary should mention 97%%, got: %s", d.Summary)
	}
	// base=0.85 * countBoost=1.05 * avgClarity=(1.0+0.7)/2=0.85 * timeBoost=1.0 = ~0.758
	if d.Confidence < 0.70 {
		t.Errorf("OOM+memory confidence = %v, expected >= 0.70", d.Confidence)
	}
}

func TestRuleBasedProvider_CrashLoopWithOOM(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalCrashLoopBackOff,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "worker-pod", Namespace: "default"},
			DetectedAt: now,
		},
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "worker-pod", Namespace: "default"},
			DetectedAt: now.Add(-5 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have both OOM and CrashLoop diagnoses (different categories/actions).
	var crashDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "CrashLoopBackOff") {
			crashDiag = &diagnoses[i]
			break
		}
	}
	if crashDiag == nil {
		t.Fatal("expected a CrashLoopBackOff diagnosis")
	}
	if !strings.Contains(crashDiag.Summary, "OOM") {
		t.Errorf("CrashLoop+OOM diagnosis should mention OOM, got: %s", crashDiag.Summary)
	}
	if crashDiag.SuggestedAction != actionResourceAdjustment {
		t.Errorf("SuggestedAction = %q, want %q", crashDiag.SuggestedAction, actionResourceAdjustment)
	}
	if crashDiag.Confidence < 0.80 {
		t.Errorf("CrashLoop+OOM confidence = %v, expected >= 0.80", crashDiag.Confidence)
	}
}

func TestRuleBasedProvider_CrashLoopStandalone(t *testing.T) {
	p := newTestProvider()
	signals := []detection.Signal{
		{
			Type:       detection.SignalCrashLoopBackOff,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "worker-pod", Namespace: "default"},
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var crashDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "CrashLoopBackOff") {
			crashDiag = &diagnoses[i]
			break
		}
	}
	if crashDiag == nil {
		t.Fatal("expected CrashLoopBackOff diagnosis")
	}
	if crashDiag.Confidence > 0.55 {
		t.Errorf("standalone CrashLoop confidence = %v, expected <= 0.55", crashDiag.Confidence)
	}
	if crashDiag.SuggestedAction != actionInvestigate {
		t.Errorf("SuggestedAction = %q, want %q", crashDiag.SuggestedAction, actionInvestigate)
	}
}

func TestRuleBasedProvider_CrashLoopWithProbeFailure(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalCrashLoopBackOff,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "app-pod", Namespace: "default"},
			DetectedAt: now,
		},
		{
			Type:       detection.SignalProbeFailure,
			Severity:   detection.SeverityWarning,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "app-pod", Namespace: "default"},
			DetectedAt: now.Add(-3 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var crashDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "CrashLoopBackOff") {
			crashDiag = &diagnoses[i]
			break
		}
	}
	if crashDiag == nil {
		t.Fatal("expected CrashLoopBackOff diagnosis")
	}
	if !strings.Contains(crashDiag.Summary, "health probes") {
		t.Errorf("summary should mention health probes, got: %s", crashDiag.Summary)
	}
	if crashDiag.SuggestedAction != actionDeploymentFix {
		t.Errorf("SuggestedAction = %q, want %q", crashDiag.SuggestedAction, actionDeploymentFix)
	}
}

func TestRuleBasedProvider_NodePressureWithEviction(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalNodeMemoryPressure,
			Severity:   detection.SeverityWarning,
			Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "node-1"},
			DetectedAt: now,
		},
		{
			Type:       detection.SignalPodEvicted,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "evicted-pod", Namespace: "default"},
			Metadata:   map[string]string{"node": "node-1"},
			DetectedAt: now.Add(-20 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var pressureDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "pressure") {
			pressureDiag = &diagnoses[i]
			break
		}
	}
	if pressureDiag == nil {
		t.Fatal("expected node pressure diagnosis")
	}
	if !strings.Contains(pressureDiag.Summary, "evictions") {
		t.Errorf("summary should mention evictions, got: %s", pressureDiag.Summary)
	}
	if pressureDiag.Confidence < 0.80 {
		t.Errorf("pressure+eviction confidence = %v, expected >= 0.80", pressureDiag.Confidence)
	}
}

func TestRuleBasedProvider_NodePressureStandalone(t *testing.T) {
	p := newTestProvider()
	signals := []detection.Signal{
		{
			Type:       detection.SignalNodeDiskPressure,
			Severity:   detection.SeverityWarning,
			Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "node-2"},
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var pressureDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "pressure") {
			pressureDiag = &diagnoses[i]
			break
		}
	}
	if pressureDiag == nil {
		t.Fatal("expected node pressure diagnosis")
	}
	if pressureDiag.SuggestedAction != actionResourceAdjustment {
		t.Errorf("SuggestedAction = %q, want %q", pressureDiag.SuggestedAction, actionResourceAdjustment)
	}
}

func TestRuleBasedProvider_NodeDown(t *testing.T) {
	p := newTestProvider()
	signals := []detection.Signal{
		{
			Type:       detection.SignalNodeNotReady,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "node-3"},
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var nodeDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "not ready") {
			nodeDiag = &diagnoses[i]
			break
		}
	}
	if nodeDiag == nil {
		t.Fatal("expected node down diagnosis")
	}
	if !strings.Contains(nodeDiag.Summary, "node-3") {
		t.Errorf("summary should mention node name, got: %s", nodeDiag.Summary)
	}
	if nodeDiag.SuggestedAction != "node-restart" {
		t.Errorf("SuggestedAction = %q, want %q", nodeDiag.SuggestedAction, "node-restart")
	}
}

func TestRuleBasedProvider_NodeDownWithNetworkUnavailable(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalNodeNotReady,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "node-3"},
			DetectedAt: now,
		},
		{
			Type:       detection.SignalNodeNetworkDown,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "node-3"},
			DetectedAt: now.Add(-5 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var nodeDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "unreachable") {
			nodeDiag = &diagnoses[i]
			break
		}
	}
	if nodeDiag == nil {
		t.Fatal("expected network-related node down diagnosis")
	}
	if nodeDiag.SuggestedAction != actionInvestigate {
		t.Errorf("SuggestedAction = %q, want %q", nodeDiag.SuggestedAction, actionInvestigate)
	}
}

func TestRuleBasedProvider_ResourceSaturation(t *testing.T) {
	p := newTestProvider()
	signals := []detection.Signal{
		{
			Type:       detection.SignalCPUSaturationCritical,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "node-1"},
			Value:      floatPtr(96),
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var satDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "saturation") {
			satDiag = &diagnoses[i]
			break
		}
	}
	if satDiag == nil {
		t.Fatal("expected resource saturation diagnosis")
	}
	if !strings.Contains(satDiag.Summary, "96%") {
		t.Errorf("summary should mention 96%%, got: %s", satDiag.Summary)
	}
	if satDiag.SuggestedAction != "scale-up" {
		t.Errorf("SuggestedAction = %q, want %q", satDiag.SuggestedAction, "scale-up")
	}
}

func TestRuleBasedProvider_ResourceSaturationWithPending(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalMemorySaturationCrit,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "node-1"},
			Value:      floatPtr(98),
			DetectedAt: now,
		},
		{
			Type:       detection.SignalPodPendingLong,
			Severity:   detection.SeverityWarning,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "pending-pod", Namespace: "default"},
			DetectedAt: now.Add(-30 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var satDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "saturation") {
			satDiag = &diagnoses[i]
			break
		}
	}
	if satDiag == nil {
		t.Fatal("expected saturation diagnosis")
	}
	if !strings.Contains(satDiag.Summary, "failing to schedule") {
		t.Errorf("summary should mention scheduling failure, got: %s", satDiag.Summary)
	}
	// base=0.90 * countBoost=1.05 * avgClarity=(0.85+0.6)/2=0.725 * timeBoost=1.0 = ~0.685
	if satDiag.Confidence < 0.65 {
		t.Errorf("saturation+pending confidence = %v, expected >= 0.65", satDiag.Confidence)
	}

	// PendingPod rule should NOT produce a separate diagnosis since saturation covers it.
	for _, d := range diagnoses {
		if strings.Contains(d.Summary, "Pod pending for extended period") {
			t.Error("PendingPod rule should be suppressed when saturation signals present")
		}
	}
}

func TestRuleBasedProvider_ControlPlane(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalAPIServerUnhealthy,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Component", Name: "kube-apiserver"},
			DetectedAt: now,
		},
		{
			Type:       detection.SignalETCDUnhealthy,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Component", Name: "etcd"},
			DetectedAt: now.Add(-10 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cpDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "Control plane") {
			cpDiag = &diagnoses[i]
			break
		}
	}
	if cpDiag == nil {
		t.Fatal("expected control plane diagnosis")
	}
	if cpDiag.Confidence < 0.85 {
		t.Errorf("control plane confidence = %v, expected >= 0.85", cpDiag.Confidence)
	}
	if cpDiag.SuggestedAction != actionInvestigate {
		t.Errorf("SuggestedAction = %q, want %q", cpDiag.SuggestedAction, actionInvestigate)
	}
}

func TestRuleBasedProvider_ImagePullStandalone(t *testing.T) {
	p := newTestProvider()
	signals := []detection.Signal{
		{
			Type:       detection.SignalImagePullBackOff,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "img-pod", Namespace: "default"},
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var imgDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "Image pull") {
			imgDiag = &diagnoses[i]
			break
		}
	}
	if imgDiag == nil {
		t.Fatal("expected image pull diagnosis")
	}
	if imgDiag.SuggestedAction != actionDeploymentFix {
		t.Errorf("SuggestedAction = %q, want %q", imgDiag.SuggestedAction, actionDeploymentFix)
	}
}

func TestRuleBasedProvider_ImagePullSuppressedByCrashLoop(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalCrashLoopBackOff,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "crash-pod", Namespace: "default"},
			DetectedAt: now,
		},
		{
			Type:       detection.SignalImagePullBackOff,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "crash-pod", Namespace: "default"},
			DetectedAt: now.Add(-5 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, d := range diagnoses {
		if strings.Contains(d.Summary, "Image pull failing for container") {
			t.Error("standalone ImagePull diagnosis should be suppressed when CrashLoop+ImagePull for same pod")
		}
	}
}

func TestRuleBasedProvider_ImagePullPartialSuppression(t *testing.T) {
	// CrashLoop on pod A + ImagePull on pod A and pod B.
	// Pod A's ImagePull should be suppressed, pod B's should still produce a diagnosis.
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalCrashLoopBackOff,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "crash-pod", Namespace: "default"},
			DetectedAt: now,
		},
		{
			Type:       detection.SignalImagePullBackOff,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "crash-pod", Namespace: "default"},
			DetectedAt: now.Add(-5 * time.Second),
		},
		{
			Type:       detection.SignalImagePullBackOff,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "other-pod", Namespace: "default"},
			DetectedAt: now.Add(-3 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var imgDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "Image pull failing") {
			imgDiag = &diagnoses[i]
			break
		}
	}
	if imgDiag == nil {
		t.Fatal("expected ImagePull diagnosis for other-pod (not covered by CrashLoop)")
	}
	// Verify it only includes other-pod, not crash-pod.
	for _, r := range imgDiag.AffectedResources {
		if r.Name == "crash-pod" {
			t.Error("ImagePull diagnosis should not include crash-pod (covered by CrashLoop rule)")
		}
	}
}

func TestRuleBasedProvider_PendingPodStandalone(t *testing.T) {
	p := newTestProvider()
	signals := []detection.Signal{
		{
			Type:       detection.SignalPodPendingLong,
			Severity:   detection.SeverityWarning,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "stuck-pod", Namespace: "default"},
			DetectedAt: time.Now(),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var pendingDiag *Diagnosis
	for i := range diagnoses {
		if strings.Contains(diagnoses[i].Summary, "pending") {
			pendingDiag = &diagnoses[i]
			break
		}
	}
	if pendingDiag == nil {
		t.Fatal("expected pending pod diagnosis")
	}
	if pendingDiag.Confidence > 0.55 {
		t.Errorf("standalone pending confidence = %v, expected <= 0.55", pendingDiag.Confidence)
	}
	if pendingDiag.SuggestedAction != actionResourceAdjustment {
		t.Errorf("SuggestedAction = %q, want %q", pendingDiag.SuggestedAction, actionResourceAdjustment)
	}
}

func TestRuleBasedProvider_MultipleIndependentIssues(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		// Independent issue 1: OOM on pod A
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "pod-a", Namespace: "ns-a"},
			DetectedAt: now,
		},
		// Independent issue 2: Node not ready
		{
			Type:       detection.SignalNodeNotReady,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Node", Name: "node-b"},
			DetectedAt: now.Add(-30 * time.Second),
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(diagnoses) < 2 {
		t.Errorf("expected at least 2 diagnoses for independent issues, got %d", len(diagnoses))
	}
}

func TestRuleBasedProvider_SortedByConfidence(t *testing.T) {
	p := newTestProvider()
	now := time.Now()
	signals := []detection.Signal{
		// Low confidence: standalone pending
		{
			Type:       detection.SignalPodPendingLong,
			Severity:   detection.SeverityWarning,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "pending-pod", Namespace: "default"},
			DetectedAt: now,
		},
		// High confidence: control plane
		{
			Type:       detection.SignalAPIServerUnhealthy,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Component", Name: "kube-apiserver"},
			DetectedAt: now,
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(diagnoses) < 2 {
		t.Fatalf("expected at least 2 diagnoses, got %d", len(diagnoses))
	}

	for i := 1; i < len(diagnoses); i++ {
		if diagnoses[i].Confidence > diagnoses[i-1].Confidence {
			t.Errorf("diagnoses not sorted by confidence: [%d]=%v > [%d]=%v",
				i, diagnoses[i].Confidence, i-1, diagnoses[i-1].Confidence)
		}
	}
}

func TestRuleBasedProvider_DiagnosedAtSet(t *testing.T) {
	p := newTestProvider()
	before := time.Now()
	signals := []detection.Signal{
		{
			Type:       detection.SignalOOMKilled,
			Severity:   detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "pod-1", Namespace: "default"},
			DetectedAt: before,
		},
	}

	diagnoses, err := p.Diagnose(context.Background(), signals)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	after := time.Now()
	for _, d := range diagnoses {
		if d.DiagnosedAt.Before(before) || d.DiagnosedAt.After(after) {
			t.Errorf("DiagnosedAt %v is not between %v and %v", d.DiagnosedAt, before, after)
		}
	}
}
