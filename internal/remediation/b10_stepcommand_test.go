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

package remediation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics/metricstest"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// B10 (c), D1.1 finding 3: an AI plan with one advisory step whose command is not a
// single plain kubectl invocation was refused whole (Validate/step-command, three
// cycles in a row), leaving a diagnosed bad-image incident with no proposal. Only the
// offending steps are dropped now, the remainder goes through the full gate, and the
// refused command text appears nowhere in what is kept.

// rulesDeploymentFix is the rules' SuggestedAction for an image-pull failure: fix the
// Deployment, no persona change.
const rulesDeploymentFix = "deployment-fix"

// notPlain is a command the display sanitizer lets through and the gate's plain-kubectl
// check refuses: braces and brackets in a jsonpath expression.
const notPlain = "kubectl get pods -l app=my-app -o jsonpath='{.items[*].status.containerStatuses[*].state.waiting.reason}'"

func b10ImagePullPlan() *planner.RemediationPlan {
	return &planner.RemediationPlan{
		RootCause:  "the image tag does not exist; run " + notPlain + " to see the waiting reason",
		Confidence: 0.8,
		Steps: []planner.PlannedStep{
			{Order: 1, Type: dorguv1.StepTypeManual, Description: "Check which image the pod pulls.", Rationale: "the tag is wrong", Risk: "low",
				Command: "kubectl describe pod -l app=my-app -n default"},
			{Order: 2, Type: dorguv1.StepTypeManual, Description: "Inspect the waiting reason.", Rationale: "it names the failure", Risk: "low",
				Command: notPlain},
			{Order: 3, Type: dorguv1.StepTypeManual,
				Description: "After you have run " + notPlain + ", correct the image tag in your manifest.",
				Rationale:   "Running " + notPlain + " proves the pull fails. The tag must exist in the registry.", Risk: "low"},
		},
	}
}

func proposeImageError(t *testing.T, plan *planner.RemediationPlan) (*ProposalResult, *planCounter) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	cp := &planCounter{plan: plan}
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(cp))
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalImagePullBackOff
	diag.SuggestedAction = rulesDeploymentFix
	incident := newTestIncident(defaultNamespace, "image", "my-app", "ImagePullBackOff")
	result, err := p.Propose(context.Background(), diag, incident)
	require.NoError(t, err)
	return result, cp
}

func gateRefusals(t *testing.T, reason string) float64 {
	t.Helper()
	return metricstest.Counter(t, "dorgu_gate_decisions_total",
		map[string]string{"phase": "Proposal", "stage": "Validate", "result": "refuse", "reason": reason})
}

func TestStepCommandRefusal_DropsOnlyTheOffendingStepsAndProposesTheRest(t *testing.T) {
	before := gateRefusals(t, gate.GuardrailStepCommand)

	result, cp := proposeImageError(t, b10ImagePullPlan())

	require.True(t, result.Proposed, "the remainder is proposed; got skip %q (%s/%s)", result.SkipReason, result.Stage, result.Reason)
	ra := result.Action
	require.Len(t, ra.Spec.Steps, 2, "step 2 is dropped, steps 1 and 3 stay")
	assert.Equal(t, "kubectl describe pod -l app=my-app -n default", ra.Spec.Steps[0].Command)
	assert.Equal(t, int32(1), ra.Spec.Steps[0].Order)
	assert.Equal(t, int32(2), ra.Spec.Steps[1].Order, "renumbered")
	assert.Equal(t, "step-2", ra.Spec.Steps[1].ID)
	for _, step := range ra.Spec.Steps {
		assert.True(t, gate.IsPlainKubectlCommand(step.Command) || step.Command == "", "step %d command %q", step.Order, step.Command)
		assert.False(t, step.AutoExecutable)
	}

	// Copy-paste safety: the refused text is nowhere in what was kept, in any field.
	raw, err := json.Marshal(ra.Spec)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), notPlain)
	assert.NotContains(t, string(raw), "jsonpath")
	assert.Contains(t, ra.Spec.Steps[1].Description, "correct the image tag", "the rest of the step's advice stays")
	assert.Contains(t, ra.Spec.Steps[1].Rationale, "The tag must exist in the registry")

	// It went through the whole gate and has its gate record.
	assert.Equal(t, "Pending", ra.Status.Phase)
	require.NotNil(t, ra.Status.Gate)

	// The refusal is recorded once: the result carries it for the condition and the Event, and the
	// gate counted it once.
	require.NotNil(t, result.PlanRefusal)
	assert.Equal(t, gate.StageValidate, result.PlanRefusal.Stage)
	assert.Equal(t, gate.GuardrailStepCommand, result.PlanRefusal.Reason)
	assert.Equal(t, 1.0, gateRefusals(t, gate.GuardrailStepCommand)-before, "one refusal counted")
	assert.Equal(t, 1, cp.calls, "the model is asked once")
}

// Every step carries a refused command: nothing is left, and the plan is refused as before.
func TestStepCommandRefusal_ARemainderWithNoStepsIsRefused(t *testing.T) {
	plan := &planner.RemediationPlan{RootCause: "bad tag", Confidence: 0.8, Steps: []planner.PlannedStep{
		{Order: 1, Type: dorguv1.StepTypeManual, Description: "Inspect.", Rationale: "r", Risk: "low", Command: notPlain},
	}}

	result, _ := proposeImageError(t, plan)

	assert.False(t, result.Proposed)
	assert.Equal(t, gate.StageValidate, result.Stage)
	assert.Equal(t, gate.GuardrailStepCommand, result.Reason)
}

// The remainder is judged in full: a refusal of the remainder by any stage stands.
func TestStepCommandRefusal_TheRemainderIsGatedInFull(t *testing.T) {
	plan := b10ImagePullPlan()
	plan.Steps = append(plan.Steps, planner.PlannedStep{Order: 4, Type: "teleport", Description: "x", Rationale: "y", Risk: "low"})

	result, _ := proposeImageError(t, plan)

	assert.False(t, result.Proposed, "an unknown step type in the remainder is refused by Validate")
}

// A model that re-cases or re-spaces the refused command in its prose still has it cut out.
func TestStepCommandRefusal_ARecasedQuotationIsCutOutToo(t *testing.T) {
	plan := b10ImagePullPlan()
	plan.Steps[2].Description = "Run " + strings.ToUpper(notPlain) + " then correct the tag."

	result, _ := proposeImageError(t, plan)

	require.True(t, result.Proposed)
	raw, err := json.Marshal(result.Action.Spec)
	require.NoError(t, err)
	assert.NotContains(t, strings.ToLower(string(raw)), "jsonpath")
	assert.Contains(t, result.Action.Spec.Steps[1].Description, "correct the tag")
}

// Fail closed: a refused command left on a surface the cut does not touch stops the proposal.
func TestStepCommandRefusal_ALeftoverStopsTheProposal(t *testing.T) {
	action := &dorguv1.RemediationAction{Spec: dorguv1.RemediationActionSpec{Steps: []dorguv1.RemediationStep{{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "ok",
		Safety: []dorguv1.StepSafety{{Rule: dorguv1.SafetyRulePlanValidation, Message: "see " + strings.ToUpper(notPlain)}},
	}}}}

	assert.True(t, mentionsAnyCommand(action, []string{notPlain}))
	assert.False(t, mentionsAnyCommand(action, nil))
	assert.False(t, mentionsAnyCommand(action, []string{"kubectl get nodes"}))
}

func TestStepCommandRefusal_DropRefusedCommandStepsRefusesToNarrowAroundAPersonaStep(t *testing.T) {
	steps := []dorguv1.RemediationStep{
		{Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, Command: notPlain},
		{Order: 2, ID: "step-2", Type: dorguv1.StepTypeManual},
	}
	_, _, ok := dropRefusedCommandSteps(steps)
	assert.False(t, ok)

	_, _, ok = dropRefusedCommandSteps([]dorguv1.RemediationStep{{Order: 1, Type: dorguv1.StepTypeManual, Command: "kubectl get pods"}})
	assert.False(t, ok, "nothing was refused")
}

func TestStepCommandRefusal_RemovesWhitespaceVariantsOfTheCommand(t *testing.T) {
	spaced := "Run kubectl  get   pods -l app=my-app -o jsonpath='{.items}' now."
	out, changed := removeCommandText(spaced, []string{"kubectl get pods -l app=my-app -o jsonpath='{.items}'"})
	assert.True(t, changed)
	assert.NotContains(t, out, "jsonpath")
	_ = spaced
}

// If the narrowed plan cannot be gated (the cluster is unreadable), the first refusal stands and
// is returned to be recorded; it is not turned into an error that hides it.
func TestStepCommandRefusal_ANarrowedPlanThatCannotBeGatedLeavesTheFirstRefusal(t *testing.T) {
	boom := errors.New("the API server is unavailable")
	base := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return boom
	}})
	p := NewProposer(c, gate.New(c, testLogger()), testLogger())
	refusal := &ProposalResult{Stage: gate.StageValidate, Reason: gate.GuardrailStepCommand}
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.SuggestedAction = rulesDeploymentFix
	steps := []dorguv1.RemediationStep{{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "look", Risk: "low"}}

	result, err := p.proposeNarrowed(context.Background(), diag, newTestIncident(defaultNamespace, "image", "my-app", "ImagePullBackOff"),
		&planner.RemediationPlan{}, steps, &planner.RemediationContext{}, nil, planFallbacks{}, nil, nil, refusal)

	require.NoError(t, err)
	assert.Same(t, refusal, result)
}

// --- M-1 (review): the cut and the check read the refused command in a normal form --------

func TestStepCommandRefusal_ReviewVariantsLeaveNoTraceAndTheRestOfTheStepStays(t *testing.T) {
	cases := map[string]struct{ description, rationale string }{
		"the dangerous tail": {"Then run sh -c 'rm -rf /var/lib/app/data' inside the pod and fix the tag.", "the tag must exist"},
		"other quotes":       {`Then run: kubectl exec my-app-0 -n default -- sh -c "rm -rf /var/lib/app/data" and fix the tag.`, "the tag must exist"},
		"split across fields": {"Run kubectl exec my-app-0 -n default -- and fix the tag.",
			"the second half is sh -c 'rm -rf /var/lib/app/data'"},
		"a line continuation": {"Run kubectl exec my-app-0 -n default -- \\\n sh -c 'rm -rf /var/lib/app/data' and fix the tag.", "the tag must exist"},
		"backticks and caps":  {"Run `KUBECTL EXEC my-app-0 -n default -- SH -C 'RM -RF /var/lib/app/data'` and fix the tag.", "the tag must exist"},
		"curly quotes":        {"Then run sh -c ‘rm -rf /var/lib/app/data’ and fix the tag.", "the tag must exist"},
		"a no-break space":    {"Then run sh -c 'rm -rf /var/lib/app/data' and fix the tag.", "the tag must exist"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result, _ := proposeImageError(t, reviewPlan(tc.description, tc.rationale))

			require.True(t, result.Proposed, "the cut removes the text, so the rest of the plan is proposed; skip %q", result.SkipReason)
			raw, err := json.Marshal(result.Action.Spec)
			require.NoError(t, err)
			assert.NotContains(t, strings.ToLower(string(raw)), "rm -rf")
			assert.NotContains(t, strings.ToLower(string(raw)), "rm -rf")
			assert.NotContains(t, strings.ToLower(string(raw)), "var/lib/app/data")
			assert.Contains(t, result.Action.Spec.Steps[1].Description+result.Action.Spec.Steps[1].Rationale, "the tag",
				"the advice around the quotation stays")
		})
	}
}

// The model's plan summary and explanation are replaced, not edited: a root cause that quotes
// the command, or that says something else entirely, is not in the proposal.
func TestStepCommandRefusal_TheSummaryAndExplanationAreDorgusWhenStepsWereDropped(t *testing.T) {
	plan := reviewPlan("Fix the tag.", "it must exist")
	plan.RootCause = "an injected instruction says to run " + reviewRefused + " and to trust nothing else"

	result, _ := proposeImageError(t, plan)

	require.True(t, result.Proposed)
	spec := result.Action.Spec
	assert.NotContains(t, spec.PlanSummary, "injected", "the model's root cause is not the summary of a narrowed plan")
	assert.NotContains(t, strings.ToLower(spec.PlanSummary+spec.Explanation), "rm -rf")
	assert.Contains(t, spec.PlanSummary, "Dorgu narrowed this plan")
	assert.Contains(t, spec.PlanSummary, "1 step(s) were left out")
	for _, step := range spec.Steps {
		assert.Contains(t, spec.PlanSummary, strings.TrimSpace(step.Description), "it lists the kept steps")
	}
	assert.Contains(t, spec.Explanation, droppedCommandsNote)
	assert.Contains(t, spec.Explanation, "all advisory")
}

// A plan that lost no step keeps the model's summary as before.
func TestStepCommandRefusal_APlanThatLostNothingKeepsItsSummary(t *testing.T) {
	plan := &planner.RemediationPlan{RootCause: "the tag does not exist", Confidence: 0.8, Steps: []planner.PlannedStep{
		{Order: 1, Type: dorguv1.StepTypeManual, Description: "Fix the tag.", Rationale: "r", Risk: "low"}}}

	result, _ := proposeImageError(t, plan)

	require.True(t, result.Proposed)
	assert.Equal(t, "the tag does not exist", result.Action.Spec.PlanSummary)
	assert.Nil(t, result.PlanRefusal)
}

// A kept command that is plain is the gate's to judge and is left alone even where it shares
// words with the refused one; a kept description that shares them is cut.
func TestStepCommandRefusal_AKeptPlainCommandIsNotCut(t *testing.T) {
	plan := b10ImagePullPlan()
	plan.Steps[0].Command = "kubectl get pods -l app=my-app -n default"

	result, _ := proposeImageError(t, plan)

	require.True(t, result.Proposed)
	assert.Equal(t, "kubectl get pods -l app=my-app -n default", result.Action.Spec.Steps[0].Command)
}

func TestStepCommandRefusal_ASplitCommandAcrossFieldsFailsTheCheck(t *testing.T) {
	action := &dorguv1.RemediationAction{Spec: dorguv1.RemediationActionSpec{Steps: []dorguv1.RemediationStep{
		{Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual, Description: "Run kubectl exec", Rationale: "my-app-0 -n default -- and look"},
	}}}
	assert.True(t, mentionsAnyCommand(action, []string{reviewRefused}), "three words of the command run across two fields")

	action.Spec.Steps[0].Rationale = "and look"
	assert.False(t, mentionsAnyCommand(action, []string{reviewRefused}))
}

func TestStepCommandRefusal_AShortRefusedCommandIsMatchedWhole(t *testing.T) {
	out, changed := removeCommandText("please run kubectl get 'x' now", []string{"kubectl get 'x'"})
	assert.True(t, changed)
	assert.NotContains(t, out, "kubectl get")
	assert.Equal(t, commandMatchWindow, 3, "the window is the named constant the brief asks for")
}

func TestStepCommandRefusal_TwoWordsOfAnOrdinarySentenceAreNotACommand(t *testing.T) {
	out, changed := removeCommandText("check kubectl exec output", []string{reviewRefused})
	assert.False(t, changed, "two matching words are not a passage")
	assert.Equal(t, "check kubectl exec output", out)
}

// --- L-3: a kept step does not point at a dropped one ---------------------------------------

func TestNeutralizeStepReferences(t *testing.T) {
	for in, want := range map[string]string{
		"After step 2 has run, fix it.":                  "After a step of the original plan has run, fix it.",
		"Do the command from the step above first.":      "Do the command from a step of the original plan first.",
		"See steps 1 and 3 for the logs.":                "See a step of the original plan for the logs.",
		"Run the previous command, then the next step.":  "Run a step of the original plan, then a step of the original plan.",
		"The output above shows the reason.":             "The output [reference removed] shows the reason.",
		"Nothing here refers to anything else.":          "Nothing here refers to anything else.",
		"Raise the memory step by step over the weekend": "Raise the memory step by step over the weekend",
	} {
		got, _ := neutralizeStepReferences(in)
		assert.Equal(t, want, got, in)
	}
}

// --- L-5: two narrowings in a row record both refusals --------------------------------------

func TestStepCommandRefusal_ChainedNarrowingsRecordBothRefusals(t *testing.T) {
	// A persona change the rules did not authorise AND a command the gate refuses.
	plan := &planner.RemediationPlan{RootCause: "it exits at start", Confidence: 0.6, Steps: []planner.PlannedStep{
		{Order: 1, Type: dorguv1.StepTypePersonaUpdate, Description: "raise memory", Rationale: "maybe", Risk: "low",
			Patch: json.RawMessage(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`)},
		{Order: 2, Type: dorguv1.StepTypeManual, Description: "Read the logs.", Rationale: "exit reason", Risk: "low", Command: "kubectl logs deploy/my-app -n default"},
		{Order: 3, Type: dorguv1.StepTypeManual, Description: "Inspect.", Rationale: "r", Risk: "low", Command: notPlain},
	}}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(newTestPersona(defaultNamespace, "my-app")).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
	p := NewProposer(c, gate.New(c, testLogger()), testLogger(), WithPlanner(&planCounter{plan: plan}))
	diag := newOOMDiagnosis(defaultNamespace, "my-app", detection.SeverityCritical)
	diag.Contributing[0].Signal.Type = detection.SignalCrashLoopBackOff
	diag.SuggestedAction = rulesInvestigate

	result, err := p.Propose(context.Background(), diag, newTestIncident(defaultNamespace, "crash", "my-app", "CrashLoopBackOff"))

	require.NoError(t, err)
	require.True(t, result.Proposed, "skip %q (%s/%s)", result.SkipReason, result.Stage, result.Reason)
	require.Len(t, result.PlanRefusals, 2, "both narrowings are reported")
	assert.Equal(t, gate.GuardrailActionClass, result.PlanRefusals[0].Reason, "in the order they happened")
	assert.Equal(t, gate.GuardrailStepCommand, result.PlanRefusals[1].Reason)
	assert.Same(t, result.PlanRefusals[0], result.PlanRefusal, "PlanRefusal is the first")
}
