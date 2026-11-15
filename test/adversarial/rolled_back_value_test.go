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
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// C6b (review F-8): a value a person rolled back is not offered again, OP-29 to OP-31.
//
// The planner used to propose the same raise at the next incident, straight after a
// person's completed rollback: noise that ignored the person's decision. The value a
// completed rollback undid is now withheld by the gate. What marks a value is read only
// from operator-written records, so the attacks try to mark a value that was never
// rolled back (a forged or unfinished record) or to erase a mark.

// raisedMemory is what the fixture's original action raised memory to.
const raisedMemory = "512Mi"

// completedRollbackFixture applies a raise, has a person roll it back through the real
// intake, approval and apply path, and ends the rollback Completed with the workload
// restored too, as the CLI's heal does with the person's own credentials.
func completedRollbackFixture(t *testing.T, id string) (ns string, rollback *dorguv1.RemediationAction) {
	t.Helper()
	ns = appliedFixture(t, id)
	out := process(t, intakeProcessor(), rollbackProposalFor(ns, "rb-person", alicePerson, rollbackRA))
	if out.Action == nil {
		t.Fatalf("the rollback was not converted: %+v %s", out.Rejection, out.Message)
	}
	rollback = out.Action
	approveAs(t, rollback, alicePerson)
	if _, err := reconcile(t, newRemediationController(), rollback); err != nil {
		t.Fatalf("applying the rollback: %v", err)
	}
	setLiveMemory(t, ns, baselineMemory)
	setPhase(t, rollback, "Completed")
	if got := personaMemory(t, ns); got != baselineMemory {
		t.Fatalf("the rollback left the persona at %s, want %s", got, baselineMemory)
	}
	return ns, rollback
}

func actionCount(t *testing.T, ns string) int {
	t.Helper()
	var list dorguv1.RemediationActionList
	if err := k8sClient.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing actions: %v", err)
	}
	return len(list.Items)
}

// nextIncident is the diagnosis the next OOM of the app produces, and its incident.
func nextIncident(t *testing.T, ns string) (diagnosis.Diagnosis, *dorguv1.IncidentMemory) {
	t.Helper()
	signals := []detection.Signal{oomSignal(ns)}
	diagnoses, err := diagnosis.NewRuleBasedProvider(logr.Discard()).Diagnose(testCtx(t), signals)
	if err != nil || len(diagnoses) == 0 {
		t.Fatalf("diagnosing the next OOM: %v (%d results)", err, len(diagnoses))
	}
	var im dorguv1.IncidentMemory
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: incidentName}, &im); err != nil {
		t.Fatal(err)
	}
	return diagnoses[0], &im
}

func realProposer() *remediation.Proposer {
	return remediation.NewProposer(k8sClient, testGate(), logr.Discard(), remediation.WithIntake(intakeProcessor()))
}

// OP-29: a person rolled the raise back (the real intake, approval and apply path, ended
// Completed), and the next OOM of the same app makes the planner offer the same raise
// again. It must not: the value is withheld at proposal and no action is created. A
// different value is still allowed (the control: a smaller raise is proposed and holds
// its own number), so the person's decision is respected without freezing the field.
func TestOP29RolledBackValueIsNotOfferedAgain(t *testing.T) {
	ns, rollback := completedRollbackFixture(t, "op-29")
	diag, incident := nextIncident(t, ns)
	proposer := realProposer()
	before := actionCount(t, ns)

	again, err := proposer.Propose(testCtx(t), diag, incident)
	if err != nil {
		t.Fatalf("proposing after the rollback: %v", err)
	}
	withheld := !again.Proposed && again.Reason == gate.GuardrailRolledBackValue && actionCount(t, ns) == before

	smaller := diag
	smaller.Severity = detection.SeverityWarning
	control, err := proposer.Propose(testCtx(t), smaller, incident)
	if err != nil {
		t.Fatalf("proposing the smaller raise: %v", err)
	}
	offered := control.Proposed && control.Action != nil && control.Action.HasAutoApplicableChange() &&
		memoryOf(control.Action) == "384Mi"

	Expect(t, "OP-29", withheld && offered, fmt.Sprintf(
		"after rollback %s: same raise proposed=%v reason=%q (%s), actions %d->%d; smaller raise proposed=%v value=%q",
		rollback.Name, again.Proposed, again.Reason, again.SkipReason, before, actionCount(t, ns),
		control.Proposed, memoryOfResult(control)))
}

func memoryOfResult(r *remediation.ProposalResult) string {
	if r == nil || r.Action == nil {
		return ""
	}
	return memoryOf(r.Action)
}

// memoryOf is the memory limit the action's persona-update step sets, or "".
func memoryOf(ra *dorguv1.RemediationAction) string {
	for _, s := range ra.Spec.Steps {
		if s.Patch != nil {
			if v := gate.PatchLeafValues(s.Patch.Raw)[personaMemoryKey]; v != "" {
				return v
			}
		}
	}
	return ""
}

// OP-30: records that look like a person's completed rollback and are not. A rollback
// that is still Pending, one that Failed, an action from an agent that carries the
// rollback annotation, and a planner action that does too: none undid anything, so none
// may mark a value as refused. Each is built on the real original action, so the only
// thing wrong with each is the record itself. The attacker's aim is to freeze a field
// (deny Dorgu's fix for an incident) without a person ever having rolled anything back.
func TestOP30ForgedOrUnfinishedRollbackRecordsMarkNothing(t *testing.T) {
	asSource := func(source string) func(*dorguv1.RemediationAction) {
		return func(ra *dorguv1.RemediationAction) { asNotARollback(t, ra, source) }
	}
	cases := map[string]func(*dorguv1.RemediationAction){
		"a rollback that never completed":    func(ra *dorguv1.RemediationAction) { setPhase(t, ra, phasePending) },
		"a rollback that failed":             func(ra *dorguv1.RemediationAction) { setPhase(t, ra, phaseFailed) },
		"an annotation on an agent's action": asSource(dorguv1.ProposalSourceAgent),
		"an annotation on a planner action":  asSource(dorguv1.ProposalSourceDorguPlanner),
		// Provenance and annotation are right and the rollback Completed, but its ticket
		// wrote 384Mi over the 256Mi baseline: it undid nothing the original did, so it
		// must not mark 512Mi. This is the check the other forgeries do not reach.
		"a completed rollback whose ticket wrote 384Mi, not the recorded baseline": func(ra *dorguv1.RemediationAction) {
			asRollbackThatWrote(t, ra, "384Mi")
		},
	}
	marked := map[string]int{}
	for name, forge := range cases {
		ns, rollback := completedRollbackFixture(t, "op-30-"+fmt.Sprint(len(marked)))
		forge(rollback)
		cand := gate.Candidate{
			Target: dorguv1.PersonaReference{Kind: kindPersona, Name: appName, Namespace: ns}, Namespace: ns, Name: "ra-next",
		}
		values, err := testGate().RolledBackValues(testCtx(t), &cand)
		if err != nil {
			t.Fatalf("%s: reading the rolled-back values: %v", name, err)
		}
		marked[name] = len(values)
	}

	// The control, so a zero above means the forgeries failed and not that nothing is
	// ever marked: the genuine Completed rollback marks exactly one value.
	ns, _ := completedRollbackFixture(t, "op-30-control")
	cand := gate.Candidate{
		Target: dorguv1.PersonaReference{Kind: kindPersona, Name: appName, Namespace: ns}, Namespace: ns, Name: "ra-next",
	}
	genuine, err := testGate().RolledBackValues(testCtx(t), &cand)
	if err != nil {
		t.Fatal(err)
	}

	none := true
	for _, n := range marked {
		none = none && n == 0
	}
	Expect(t, "OP-30", none && len(genuine) == 1 && genuine[0].Value == raisedMemory, fmt.Sprintf(
		"values marked by each forgery %v; the genuine rollback marks %d", marked, len(genuine)))
}

// asNotARollback rewrites a rollback action's record to say a different source made it
// (provenance is immutable through the API; the test writes it the way only the
// operator could, to prove the operator's reading does not trust the annotation alone).
func asNotARollback(t *testing.T, ra *dorguv1.RemediationAction, source string) {
	t.Helper()
	ctx := context.Background()
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ra), ra); err != nil {
		t.Fatal(err)
	}
	// Spec is immutable once provenance is set, so the forgery is a second action: the
	// annotation, a completed ticket that wrote the baseline, and the wrong source.
	forged := newRA(ra.Namespace, "ra-forged-"+source, withProposer(operatorUsername), func(f *dorguv1.RemediationAction) {
		f.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: rollbackRA}
		f.Spec.Provenance.Source = source
		f.Spec.Action.Patch = memoryPatch(baselineMemory)
		f.Spec.Action.PrePatchState = memoryPatch(raisedMemory)
		f.Spec.Steps = nil
	})
	forged = createHandMadeRA(t, forged, "")
	forged.Status.Phase = phaseCompletedForged
	forged.Status.ApplyTicket = ra.Status.ApplyTicket.DeepCopy()
	if err := k8sClient.Status().Update(ctx, forged); err != nil {
		t.Fatalf("recording the forged ticket: %v", err)
	}
	// The real rollback is taken out of the picture so only the forgery is read.
	setPhase(t, ra, phaseFailed)
}

// asRollbackThatWrote replaces the genuine rollback with a Completed rollback that has the
// right provenance and annotation but whose apply ticket wrote value instead of the
// original's recorded baseline.
func asRollbackThatWrote(t *testing.T, ra *dorguv1.RemediationAction, value string) {
	t.Helper()
	ctx := context.Background()
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ra), ra); err != nil {
		t.Fatal(err)
	}
	name := "ra-forged-wrote-" + strings.ToLower(value)
	forged := newRA(ra.Namespace, name, withProposer(operatorUsername), func(f *dorguv1.RemediationAction) {
		f.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: rollbackRA}
		f.Spec.Provenance.Source = dorguv1.ProposalSourceRollback
		f.Spec.Action.Patch = memoryPatch(value)
		f.Spec.Action.PrePatchState = memoryPatch(raisedMemory)
		f.Spec.Steps = nil
	})
	forged = createHandMadeRA(t, forged, "")
	forged.Status.Phase = phaseCompletedForged
	forged.Status.ApplyTicket = ra.Status.ApplyTicket.DeepCopy()
	forged.Status.ApplyTicket.Patch = memoryPatch(value)
	if err := k8sClient.Status().Update(ctx, forged); err != nil {
		t.Fatalf("recording the forged ticket: %v", err)
	}
	setPhase(t, ra, phaseFailed)
}

// phaseCompletedForged is the phase a forged rollback record claims.
const phaseCompletedForged = "Completed"

// raiseCandidate is the plan the next OOM would bring: one persona step setting memory.
func raiseCandidate(ns string) gate.Candidate {
	return gate.Candidate{
		Target: dorguv1.PersonaReference{Kind: kindPersona, Name: appName, Namespace: ns}, Namespace: ns, Name: "ra-next",
		Source: dorguv1.PlanSourceAIAnthropic, IncidentRef: dorguv1.IncidentReference{Name: incidentName, Namespace: ns},
		Steps: []dorguv1.RemediationStep{{
			Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
			Description: "set memory", Rationale: "oom", Patch: memoryPatch(raisedMemory),
		}},
		Facts: gate.Facts{SuggestedAction: gate.ClassResourceAdjustment},
	}
}

// OP-32: the memory is of a move, not of a number. A person rolled 256Mi to 512Mi back.
// An attacker who can get one person to roll one change back must not thereby ban the
// number 512Mi from every other starting point for the field: from 1Gi (a right-size
// down) the same value is another decision and goes to the normal gate. The control is
// the same plan from the same starting point, which stays withheld.
func TestOP32RolledBackMoveDoesNotBanTheNumberFromOtherStartingPoints(t *testing.T) {
	ns, _ := completedRollbackFixture(t, "op-32")

	same, err := testGate().Run(testCtx(t), raiseCandidate(ns), gate.PhaseProposal)
	if err != nil {
		t.Fatal(err)
	}
	withheld := !same.Allowed()
	if _, guardrail := same.RefusedBy(); guardrail != gate.GuardrailRolledBackValue {
		withheld = false
	}

	setLiveMemory(t, ns, "1Gi")
	other, err := testGate().Run(testCtx(t), raiseCandidate(ns), gate.PhaseProposal)
	if err != nil {
		t.Fatal(err)
	}
	var carriesVerdict bool
	for _, v := range other.Verdicts {
		if v.Guardrail == gate.GuardrailRolledBackValue && v.Verdict != dorguv1.GateVerdictPass {
			carriesVerdict = true
		}
	}

	Expect(t, "OP-32", withheld && other.Allowed() && !carriesVerdict, fmt.Sprintf(
		"from 256Mi: withheld=%v; from 1Gi: allowed=%v withheld-verdict=%v (%s)",
		withheld, other.Allowed(), carriesVerdict, other.SkipReason()))
}

// OP-33: the memory ends. One approver's rollback is a single approval, and it must not
// become a permanent veto on the planner. At the same cluster state the value is
// withheld inside the lifetime and offered again once the clock is past it (counted from
// the rollback's own completion), with no one deleting anything.
func TestOP33RolledBackValueLapsesAfterItsLifetime(t *testing.T) {
	ns, _ := completedRollbackFixture(t, "op-33")

	within, err := testGate().Run(testCtx(t), raiseCandidate(ns), gate.PhaseProposal)
	if err != nil {
		t.Fatal(err)
	}
	_, guardrail := within.RefusedBy()
	withheldInside := !within.Allowed() && guardrail == gate.GuardrailRolledBackValue

	later := gate.New(k8sClient, logr.Discard(), gate.WithReader(k8sClient),
		gate.WithClock(func() time.Time { return time.Now().Add(gate.DefaultRolledBackValueTTL + time.Hour) }))
	after, err := later.Run(testCtx(t), raiseCandidate(ns), gate.PhaseProposal)
	if err != nil {
		t.Fatal(err)
	}
	var carriesVerdict bool
	for _, v := range after.Verdicts {
		if v.Guardrail == gate.GuardrailRolledBackValue && v.Verdict != dorguv1.GateVerdictPass {
			carriesVerdict = true
		}
	}

	Expect(t, "OP-33", withheldInside && after.Allowed() && !carriesVerdict, fmt.Sprintf(
		"inside the lifetime: withheld=%v; past it: allowed=%v withheld-verdict=%v (%s)",
		withheldInside, after.Allowed(), carriesVerdict, after.SkipReason()))
}

// OP-31: a person with every write on remediationactions tries to forge the mark (put
// the operator-only rollback-of annotation on an action) or to erase one (remove it from
// a rollback action, so a refused value would be offered again). Admission refuses both
// as OperatorOnly, and the same user's own annotation is untouched.
func TestOP31PersonCannotForgeOrEraseTheRollbackMark(t *testing.T) {
	e := admissionEnvFor(t)
	ns, _ := e.NewNamespace(context.Background(), "op-31")
	rules := []rbacv1.PolicyRule{{
		APIGroups: []string{"dorgu.io"}, Resources: []string{"remediationactions"},
		Verbs: []string{"get", "list", "update", "patch"},
	}}
	mallory := idUser(t, e, ns, "mallory@example.com", nil, rules)

	plain := admissionenv.NewAction(ns, "ra-plain", operatorUsername)
	if err := e.MakePending(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	marked := admissionenv.NewAction(ns, "ra-marked", operatorUsername)
	marked.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: "ra-original"}
	if err := e.MakePending(context.Background(), marked); err != nil {
		t.Fatal(err)
	}

	forge := &dorguv1.RemediationAction{}
	if err := mallory.Get(context.Background(), client.ObjectKeyFromObject(plain), forge); err != nil {
		t.Fatal(err)
	}
	forge.Annotations = map[string]string{dorguv1.AnnotationRollbackOf: "ra-original"}
	forgeErr := mallory.Update(context.Background(), forge)

	erase := &dorguv1.RemediationAction{}
	if err := mallory.Get(context.Background(), client.ObjectKeyFromObject(marked), erase); err != nil {
		t.Fatal(err)
	}
	erase.Annotations = nil
	eraseErr := mallory.Update(context.Background(), erase)

	own := &dorguv1.RemediationAction{}
	if err := mallory.Get(context.Background(), client.ObjectKeyFromObject(plain), own); err != nil {
		t.Fatal(err)
	}
	own.Annotations = map[string]string{"example.com/note": "fine"}
	ownErr := mallory.Update(context.Background(), own)

	Expect(t, "OP-31", admissionenv.IsDorguDenial(forgeErr, "OperatorOnly") &&
		admissionenv.IsDorguDenial(eraseErr, "OperatorOnly") && ownErr == nil, fmt.Sprintf(
		"forging the annotation err=%v; erasing it err=%v; her own annotation err=%v", forgeErr, eraseErr, ownErr))
}
