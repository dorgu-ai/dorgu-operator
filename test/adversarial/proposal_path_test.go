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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
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

	for i := range gate.DefaultMaxRemediationsPerHour {
		createRA(t, newRA(ns, fmt.Sprintf("unlabelled-%d", i), withoutLabels()), "Pending")
	}

	g := gate.New(k8sClient, logr.Discard())
	dec := gateRun(t, g, newRA(ns, "next-proposal"))

	blocked := !dec.Allowed() && refusedBy(dec, gate.GuardrailRateLimit)
	Expect(t, "OP-02", blocked, fmt.Sprintf("%d unlabelled RAs exist; next proposal result=%s, verdicts %v",
		gate.DefaultMaxRemediationsPerHour, dec.Result, verdictSummary(dec)))
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
	t.Setenv(gate.OperatorNamespaceEnv, operatorNamespace)
	checker := gate.New(k8sClient, logr.Discard(),
		gate.WithProtectedNamespaces(gate.OperatorNamespace()))

	refused := map[string]bool{}
	for _, target := range []string{"kube-system", operatorNamespace} {
		dec := gateRun(t, checker, newRA(target, "deny-probe"))
		refused[target] = !dec.Allowed() && refusedBy(dec, gate.GuardrailDenyList)
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
	t.Setenv(gate.OperatorNamespaceEnv, operatorNamespace)
	apiCalls := withCannedClaudeReply(t, "A narrative about a namespace that must never be sent.")
	checker := gate.New(k8sClient, logr.Discard(),
		gate.WithProtectedNamespaces(gate.OperatorNamespace()))

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

	denied := apiCalls.Load()

	// Positive control: the same engine and transport must reach the model for an
	// allowed namespace, or a zero above only means the transport stopped matching.
	if _, err := engine.Analyze(testCtx(t), []detection.Signal{oomSignal("op-15-allowed")}); err != nil {
		t.Fatalf("diagnosing the allowed control: %v", err)
	}
	if got := apiCalls.Load() - denied; got != 1 {
		t.Fatalf("positive control: an allowed namespace made %d provider requests, want exactly 1; "+
			"the counting transport is not observing the client, so OP-15 proves nothing", got)
	}

	blocked := len(leaked) == 0 && denied == 0 && plannerCalls.Load() == 0
	Expect(t, "OP-15", blocked, fmt.Sprintf("provider requests=%d, planner calls=%d, non-rule-based diagnoses=%v",
		denied, plannerCalls.Load(), leaked))
}

// OP-16: node pressure evicts pods from several namespaces and the rules merge
// them into ONE diagnosis. Its persona (the first one found) is in an allowed
// namespace, but a contributing signal names a pod in the operator's namespace.
// The planner prompt carries every signal's message, so the whole diagnosis must
// be withheld from the model, not just checked on its persona.
//
// The attacker only needs a memory-hungry pod in a namespace that has a persona:
// the node's eviction takes the denied pod down with it.
func TestOP16MixedNamespaceEvictionNeverReachesThePlanner(t *testing.T) {
	t.Setenv(gate.OperatorNamespaceEnv, operatorNamespace)
	apiCalls := withCannedClaudeReply(t, "A plan for a namespace that must never be sent.")
	checker := gate.New(k8sClient, logr.Discard(),
		gate.WithProtectedNamespaces(gate.OperatorNamespace()))

	ns := newNamespace(t, "op-16")
	createPersona(t, ns)
	createIncident(t, ns)

	node := detection.Signal{
		Type: detection.SignalNodeMemoryPressure, Severity: detection.SeverityCritical,
		Category: detection.CategoryNode, Source: "node-collector",
		Message:  "Node ip-10-0-0-9 has memory pressure",
		Resource: dorguv1.ResourceReference{Kind: "Node", Name: "ip-10-0-0-9"}, DetectedAt: time.Now(),
	}
	evicted := func(podNamespace, name string) detection.Signal {
		return detection.Signal{
			Type: detection.SignalPodEvicted, Severity: detection.SeverityWarning,
			Category: detection.CategoryNode, Source: "pod-collector",
			Message:    fmt.Sprintf("Pod %s/%s was evicted due to node memory pressure", podNamespace, name),
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: name, Namespace: podNamespace},
			PersonaRef: &dorguv1.PersonaReference{Kind: kindPersona, Name: appName, Namespace: ns},
			Metadata:   map[string]string{"node": "ip-10-0-0-9"},
			DetectedAt: time.Now(),
		}
	}
	victim := evicted(operatorNamespace, "operator-victim")
	victim.PersonaRef = nil
	signals := []detection.Signal{node, evicted(ns, appName+"-abc"), victim}

	diagnoses, err := diagnosis.NewRuleBasedProvider(logr.Discard()).Diagnose(testCtx(t), signals)
	if err != nil || len(diagnoses) == 0 {
		t.Fatalf("rule-based diagnosis: %v (%d results)", err, len(diagnoses))
	}

	claudePlanner, err := planner.NewClaudePlanner("test-key")
	if err != nil {
		t.Fatalf("building planner: %v", err)
	}
	proposer := remediation.NewProposer(k8sClient, checker, logr.Discard(), remediation.WithPlanner(claudePlanner))

	var incident dorguv1.IncidentMemory
	if err := k8sClient.Get(testCtx(t), client.ObjectKey{Namespace: ns, Name: incidentName}, &incident); err != nil {
		t.Fatalf("reading incident: %v", err)
	}

	merged := false
	for _, d := range diagnoses {
		if d.PersonaRef == nil || d.PersonaRef.Namespace != ns {
			continue
		}
		merged = merged || len(d.Contributing) == len(signals)
		if _, err := proposer.Propose(testCtx(t), d, &incident); err != nil {
			t.Fatalf("proposing: %v", err)
		}
	}
	if !merged {
		t.Fatalf("the attack needs one merged allowed-persona diagnosis; got %d diagnoses", len(diagnoses))
	}

	denied := apiCalls.Load()

	// Positive control for the planner: the same proposer and transport must
	// reach the model for an all-allowed diagnosis, or a zero above only means
	// the transport stopped matching.
	allowed := diagnosis.Diagnosis{
		Summary:    "Node under pressure (NodeMemoryPressure), causing pod evictions.",
		Category:   string(detection.CategoryNode),
		Severity:   detection.SeverityCritical,
		PersonaRef: &dorguv1.PersonaReference{Kind: kindPersona, Name: appName, Namespace: ns},
		Contributing: []diagnosis.ContributingSignal{
			{Signal: node},
			{Signal: evicted(ns, appName+"-abc")},
		},
		SuggestedAction: "resource-adjustment",
	}
	if _, err := proposer.Propose(testCtx(t), allowed, &incident); err != nil {
		t.Fatalf("proposing the allowed control: %v", err)
	}
	if apiCalls.Load()-denied < 1 {
		t.Fatalf("positive control: an all-allowed diagnosis made no planner request; " +
			"the counting transport is not observing the planner, so OP-16 proves nothing")
	}

	Expect(t, "OP-16", denied == 0,
		fmt.Sprintf("%d request(s) reached the model API carrying a %s pod", denied, operatorNamespace))
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
	checker := gate.New(k8sClient, logr.Discard())
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

// gateRun puts a RemediationAction through the gate as a proposal.
func gateRun(t *testing.T, g *gate.Gate, ra *dorguv1.RemediationAction) *gate.Decision {
	t.Helper()
	dec, err := g.Run(testCtx(t), gate.Candidate{
		Target:      ra.Spec.PersonaRef,
		Namespace:   ra.Namespace,
		Name:        ra.Name,
		Source:      ra.Spec.PlanSource,
		IncidentRef: ra.Spec.IncidentRef,
		Steps:       ra.Spec.Steps,
		Action:      ra.Spec.Action,
	}, gate.PhaseProposal)
	if err != nil {
		t.Fatalf("running the gate for %s/%s: %v", ra.Namespace, ra.Name, err)
	}
	return dec
}

// refusedBy reports whether a refusing verdict came from the guardrail.
func refusedBy(dec *gate.Decision, guardrail string) bool {
	for _, v := range dec.Verdicts {
		if v.Guardrail == guardrail && v.Verdict == dorguv1.GateVerdictRefuse {
			return true
		}
	}
	return false
}

// verdictSummary renders a decision's verdicts as "guardrail:verdict" pairs.
func verdictSummary(dec *gate.Decision) []string {
	out := make([]string, 0, len(dec.Verdicts))
	for _, v := range dec.Verdicts {
		out = append(out, v.Guardrail+":"+v.Verdict)
	}
	return out
}
