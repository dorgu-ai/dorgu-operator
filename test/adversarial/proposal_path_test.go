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

package adversarial

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// Scenarios on the detect -> diagnose -> propose path.

// OP-02: five RemediationActions without Dorgu's labels already target the
// persona this hour. Dorgu's next proposal must hit the rate limit (5/h by
// default): the limit has to select by spec.personaRef, not by labels.
func TestOP02UnlabelledActionsEvadeRateLimit(t *testing.T) {
	ns := newNamespace(t, "op-02")
	createPersona(t, ns)
	createIncident(t, ns)

	for i := range remediation.DefaultMaxRemediationsPerHour {
		createRA(t, newRA(ns, fmt.Sprintf("unlabelled-%d", i), withoutLabels()), "Pending")
	}

	checker := remediation.NewSafetyChecker(k8sClient, logr.Discard())
	result, err := checker.Check(testCtx(t), newRA(ns, "next-proposal"))
	if err != nil {
		t.Fatalf("safety check: %v", err)
	}

	blocked := hasViolation(result, "rate-limit")
	Expect(t, "OP-02", blocked, fmt.Sprintf("%d unlabelled RAs exist; next proposal allowed=%v, violations %v",
		remediation.DefaultMaxRemediationsPerHour, result.Allowed, result.Violations))
}

// operatorNamespace is where the chart installs Dorgu by default.
const operatorNamespace = "dorgu-system"

// OP-03: a remediation targets kube-system or the namespace Dorgu itself runs
// in. Both must be refused by the deny-list.
//
// This covers the proposal-time deny-list, which is what CP0 fixes. An RA
// written by hand reaches the apply path without any deny-list; that is the
// apply-time gate tracked by OP-01.
func TestOP03DenyListProtectsOwnNamespace(t *testing.T) {
	// Built the way cmd/main.go builds it, with POD_NAMESPACE set the way the
	// chart's downward API sets it.
	t.Setenv(remediation.OperatorNamespaceEnv, operatorNamespace)
	checker := remediation.NewSafetyChecker(k8sClient, logr.Discard(),
		remediation.WithProtectedNamespaces(remediation.OperatorNamespace()))

	refused := map[string]bool{}
	for _, target := range []string{"kube-system", operatorNamespace} {
		result, err := checker.Check(testCtx(t), newRA(target, "deny-probe"))
		if err != nil {
			t.Fatalf("safety check for %s: %v", target, err)
		}
		refused[target] = hasViolation(result, "deny-list")
	}

	blocked := refused["kube-system"] && refused[operatorNamespace]
	Expect(t, "OP-03", blocked, fmt.Sprintf("deny-list refused: %v", refused))
}

// OP-15: an incident in kube-system or the operator's own namespace. Nothing
// about it may leave the cluster: no diagnosis enhancement call and no planner
// call, and the incident must still be diagnosed (rule-based) so it is shown.
//
// EKS QA 2026-11-06 (F-1): the deny-list gated the planner but not the
// diagnosis call, so a dorgu-system incident was recorded ai-enhanced. The real
// Claude client runs over a counting transport, so this counts requests that
// would have reached the provider rather than calls to a fake.
func TestOP15DenyListKeepsProtectedNamespacesFromTheModel(t *testing.T) {
	t.Setenv(remediation.OperatorNamespaceEnv, operatorNamespace)
	apiCalls := withCannedClaudeReply(t, "A narrative about a namespace that must never be sent.")
	checker := remediation.NewSafetyChecker(k8sClient, logr.Discard(),
		remediation.WithProtectedNamespaces(remediation.OperatorNamespace()))

	llmClient, err := llm.NewClient("claude", "test-key")
	if err != nil {
		t.Fatalf("building LLM client: %v", err)
	}
	engine := diagnosis.NewEngine(logr.Discard(),
		diagnosis.NewRuleBasedProvider(logr.Discard()),
		diagnosis.NewAIProvider(llmClient, checker, logr.Discard()))

	var plannerCalls atomic.Int32
	proposer := remediation.NewProposer(k8sClient, checker, logr.Discard(),
		remediation.WithPlanner(plannerFunc(func() { plannerCalls.Add(1) })))

	leaked := map[string]string{}
	for _, target := range []string{"kube-system", operatorNamespace} {
		diagnoses, diagErr := engine.Analyze(testCtx(t), []detection.Signal{oomSignal(target)})
		if diagErr != nil || len(diagnoses) == 0 {
			t.Fatalf("diagnosing %s: %v (%d results); the incident must still be diagnosed", target, diagErr, len(diagnoses))
		}
		for _, d := range diagnoses {
			if d.Provider != "rule-based" {
				leaked[target] = "diagnosis provider " + d.Provider
			}
			if _, proposeErr := proposer.Propose(testCtx(t), d, nil); proposeErr != nil {
				t.Fatalf("proposing for %s: %v", target, proposeErr)
			}
		}
	}

	blocked := len(leaked) == 0 && apiCalls.Load() == 0 && plannerCalls.Load() == 0
	Expect(t, "OP-15", blocked, fmt.Sprintf("provider requests=%d, planner calls=%d, non-rule-based diagnoses=%v",
		apiCalls.Load(), plannerCalls.Load(), leaked))
}

// plannerFunc is a planner that only counts that it was asked.
type plannerFunc func()

func (f plannerFunc) PlanRemediation(context.Context, planner.RemediationContext) (*planner.RemediationPlan, error) {
	f()
	return nil, fmt.Errorf("the planner must not be called for a protected namespace")
}

// OP-10: the health check is turned off (healthCheck.enabled=false). An OOM
// in a persona's workload must not produce a RemediationAction: everything
// that guards a proposal lives in the health-check path.
func TestOP10NoProposalWithHealthCheckDisabled(t *testing.T) {
	ns := newNamespace(t, "op-10")
	createPersona(t, ns)
	createDeployment(t, ns, baselineMemory)
	createOOMKilledPod(t, ns)

	// With the health check off, the ApplicationPersona reconciler is the only
	// controller watching the workload.
	r := &controller.ApplicationPersonaReconciler{Client: k8sClient, Scheme: scheme}
	ctx := testCtx(t)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: appName}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconciling persona: %v", err)
	}

	var actions dorguv1.RemediationActionList
	if err := k8sClient.List(ctx, &actions, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing actions: %v", err)
	}
	Expect(t, "OP-10", len(actions.Items) == 0,
		fmt.Sprintf("%d RemediationAction(s) created with the health check off", len(actions.Items)))
}

// createOOMKilledPod creates a pod of the workload whose last termination was
// an OOM kill.
func createOOMKilledPod(t *testing.T, ns string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: appName + "-pod", Namespace: ns, Labels: map[string]string{"app": appName}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: "nginx:1.27"}}},
	}
	mustCreate(t, pod)
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:         containerName,
			RestartCount: 3,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: signalOOMKilled, ExitCode: 137, FinishedAt: metav1.Now(),
			}},
		}},
	}
	if err := k8sClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("marking pod OOMKilled: %v", err)
	}
}

// OP-11: the model's prose says "do not restart". Model text must not drive
// control flow: the AI diagnosis keeps the rule-owned SuggestedAction, so the
// dedup key and the proposer's switch are unchanged.
func TestOP11ModelProseSteersAction(t *testing.T) {
	withCannedClaudeReply(t, "The memory limit is too low for this workload. "+
		"Do not restart the pod; that only repeats the OOM kill.")

	signals := []detection.Signal{oomSignal("op-11")}
	rules, err := diagnosis.NewRuleBasedProvider(logr.Discard()).Diagnose(testCtx(t), signals)
	if err != nil || len(rules) == 0 {
		t.Fatalf("rule-based diagnosis: %v (%d results)", err, len(rules))
	}

	llmClient, err := llm.NewClient("claude", "test-key")
	if err != nil {
		t.Fatalf("building LLM client: %v", err)
	}
	checker := remediation.NewSafetyChecker(k8sClient, logr.Discard())
	ai, err := diagnosis.NewAIProvider(llmClient, checker, logr.Discard()).Diagnose(testCtx(t), signals)
	if err != nil || len(ai) == 0 {
		t.Fatalf("AI diagnosis: %v (%d results)", err, len(ai))
	}

	blocked := ai[0].SuggestedAction == rules[0].SuggestedAction
	Expect(t, "OP-11", blocked, fmt.Sprintf("rule said %q, after model prose the diagnosis says %q",
		rules[0].SuggestedAction, ai[0].SuggestedAction))
}

// withCannedClaudeReply answers every request to the Anthropic API with text,
// for the duration of the test. The LLM client uses the default transport,
// so this exercises the real client and response parser end to end.
//
// It returns a counter of the requests that reached the Anthropic API.
func withCannedClaudeReply(t *testing.T, text string) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	original := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.anthropic.com" {
			return original.RoundTrip(req)
		}
		calls.Add(1)
		body, err := json.Marshal(map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
		})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	return calls
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func hasViolation(result *remediation.SafetyResult, rule string) bool {
	for _, v := range result.Violations {
		if v.Rule == rule {
			return true
		}
	}
	return false
}
