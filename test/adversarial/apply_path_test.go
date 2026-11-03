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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Scenarios on the approve -> apply -> verify path. Each one hands the real
// RemediationController a RemediationAction and checks what reached the
// persona.

// OP-01: a hand-written RA with a 16x memory patch, an empty prePatchState and
// no Dorgu labels is approved. The persona must not change.
func TestOP01HandCreatedSixteenXPatch(t *testing.T) {
	ns := newNamespace(t, "op-01")
	createPersona(t, ns)
	createIncident(t, ns)

	ra := createRA(t, newRA(ns, "hand-made", withoutLabels(), func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action.Patch = memoryPatch("4Gi")
		ra.Spec.Action.PrePatchState = rawJSON(`{}`)
	}), phaseApproved)

	_, err := reconcile(t, newRemediationController(), ra)

	mem := personaMemory(t, ns)
	blocked := mem == baselineMemory && ra.Status.Phase != phaseApplying
	Expect(t, "OP-01", blocked, fmt.Sprintf("persona memory %s -> %s, RA phase %q, reconcile err %v",
		baselineMemory, mem, ra.Status.Phase, err))
}

// OP-04: Dorgu proposed 256Mi -> 512Mi against a live 256Mi; before approval
// the live limit was cut 4x to 64Mi, so 512Mi is now 8x live. Approval must
// re-gate against live: refuse, or re-clamp to at most 2x live.
func TestOP04LiveValueChangedBeforeApproval(t *testing.T) {
	ns := newNamespace(t, "op-04")
	createPersona(t, ns)
	createIncident(t, ns)
	deploy := createDeployment(t, ns, baselineMemory)

	observed := metav1.Now()
	ra := createRA(t, newRA(ns, "grounded", func(ra *dorguv1.RemediationAction) {
		ra.Spec.WorkloadRef = &dorguv1.WorkloadRef{
			Kind: "Deployment", Name: appName, Namespace: ns, Container: containerName,
			ManagedBy: dorguv1.ManagedByUnmanaged,
			ObservedResources: &dorguv1.ObservedResources{
				Limits: &dorguv1.ResourceValues{Memory: baselineMemory},
			},
			ObservedAt: &observed,
		}
	}), "Pending")

	const live = "64Mi"
	deploy.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse(live)
	if err := k8sClient.Update(context.Background(), deploy); err != nil {
		t.Fatalf("changing the live limit: %v", err)
	}

	setPhase(t, ra, phaseApproved)
	_, err := reconcile(t, newRemediationController(), ra)

	mem := personaMemory(t, ns)
	ceiling := resource.MustParse(live)
	ceiling.Add(resource.MustParse(live))
	applied := resource.MustParse(mem)
	blocked := mem == baselineMemory || applied.Cmp(ceiling) <= 0
	Expect(t, "OP-04", blocked, fmt.Sprintf("live %s, persona now %s (2x live is %s), RA phase %q, err %v",
		live, mem, ceiling.String(), ra.Status.Phase, err))
}

// OP-05: two Pending Dorgu proposals for one persona, on different fields,
// are both approved. Only one may apply; the second must be refused.
func TestOP05TwoApprovedProposalsForOnePersona(t *testing.T) {
	ns := newNamespace(t, "op-05")
	createPersona(t, ns)
	createIncident(t, ns)

	first := createRA(t, newRA(ns, "memory"), "Pending")
	second := createRA(t, newRA(ns, "cpu", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Explanation = "Increase CPU limit from 250m to 500m"
		ra.Spec.Action.Patch = rawJSON(`{"spec":{"resources":{"limits":{"cpu":"500m"}}}}`)
		ra.Spec.Action.PrePatchState = rawJSON(`{"spec":{"resources":{"limits":{"cpu":"250m"}}}}`)
	}), "Pending")
	setPhase(t, first, phaseApproved)
	setPhase(t, second, phaseApproved)

	rc := newRemediationController()
	if _, err := reconcile(t, rc, first); err != nil {
		t.Fatalf("reconciling the first approval: %v", err)
	}
	_, err := reconcile(t, rc, second)

	cpu := personaCPU(t, ns)
	blocked := second.Status.Phase != phaseApplying && cpu == baselineCPU
	Expect(t, "OP-05", blocked, fmt.Sprintf("first RA %q, second RA %q, persona cpu %s -> %s, err %v",
		first.Status.Phase, second.Status.Phase, baselineCPU, cpu, err))
}

// OP-06: the spec is edited after a human approved it. The edited plan was
// never approved, so it must not apply.
func TestOP06SpecEditedAfterApproval(t *testing.T) {
	ns := newNamespace(t, "op-06")
	createPersona(t, ns)
	createIncident(t, ns)

	ra := createRA(t, newRA(ns, "edited", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action.Patch = memoryPatch("384Mi")
	}), "Pending")
	setPhase(t, ra, phaseApproved)

	ra.Spec.Action.Patch = memoryPatch("512Mi")
	if err := k8sClient.Update(context.Background(), ra); err != nil {
		t.Fatalf("editing the approved spec: %v", err)
	}

	_, err := reconcile(t, newRemediationController(), ra)

	mem := personaMemory(t, ns)
	blocked := mem != "512Mi"
	Expect(t, "OP-06", blocked, fmt.Sprintf("approved 384Mi, edited to 512Mi, persona now %s, RA phase %q, err %v",
		mem, ra.Status.Phase, err))
}

// OP-07: the patch reaches outside spec.resources, flipping a security policy
// and deleting the CPU limit with a JSON merge-patch null.
func TestOP07PatchOutsideResourceAllowlist(t *testing.T) {
	ns := newNamespace(t, "op-07")
	createPersona(t, ns)
	createIncident(t, ns)

	ra := createRA(t, newRA(ns, "out-of-scope", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action.Patch = rawJSON(
			`{"spec":{"policies":{"security":{"runAsNonRoot":false}},"resources":{"limits":{"cpu":null}}}}`)
		ra.Spec.Action.PrePatchState = rawJSON(
			`{"spec":{"policies":{"security":{"runAsNonRoot":true}},"resources":{"limits":{"cpu":"250m"}}}}`)
	}), phaseApproved)

	_, err := reconcile(t, newRemediationController(), ra)

	p := getPersona(t, ns)
	nonRoot := p.Spec.Policies != nil && p.Spec.Policies.Security != nil &&
		p.Spec.Policies.Security.RunAsNonRoot != nil && *p.Spec.Policies.Security.RunAsNonRoot
	cpu := personaCPU(t, ns)
	blocked := nonRoot && cpu == baselineCPU
	Expect(t, "OP-07", blocked, fmt.Sprintf("runAsNonRoot still true: %v, cpu %s -> %q, RA phase %q, err %v",
		nonRoot, baselineCPU, cpu, ra.Status.Phase, err))
}

// verificationFloor is the shortest verification window the harness may
// accept from a proposal. The operator default is 10m; P1 owns the exact
// bound, and this test only demands that a 1s author value is not honoured.
const verificationFloor = time.Minute

// OP-08: the author sets healthCheckAfter: 1s and rollback.enabled: false.
// Both are operator-owned: the window is bounded, and a degraded result
// still rolls back.
func TestOP08AuthorControlsVerificationAndRollback(t *testing.T) {
	ns := newNamespace(t, "op-08")
	createPersona(t, ns)
	createIncident(t, ns)

	ra := createRA(t, newRA(ns, "no-rollback", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Rollback = &dorguv1.RemediationRollbackSpec{
			Enabled:          false,
			HealthCheckAfter: &metav1.Duration{Duration: time.Second},
		}
	}), phaseApproved)

	// The OOM signal is still present after the change: verification degrades.
	rc := newRemediationController(oomSignal(ns))
	res, err := reconcile(t, rc, ra)
	if err != nil {
		t.Fatalf("applying: %v", err)
	}
	window := res.RequeueAfter

	setPhase(t, ra, phaseVerifying)
	_, verifyErr := reconcile(t, rc, ra)

	mem := personaMemory(t, ns)
	blocked := window >= verificationFloor && mem == baselineMemory
	Expect(t, "OP-08", blocked, fmt.Sprintf(
		"verification window %s (floor %s), after degraded verify persona %s, RA phase %q, err %v",
		window, verificationFloor, mem, ra.Status.Phase, verifyErr))
}

// OP-09: an external RA carries a forged guardrail verdict claiming its 16x
// step is within the cap. The operator must recompute verdicts rather than
// leave a forged one on the record a human reads.
func TestOP09ForgedSafetyVerdict(t *testing.T) {
	ns := newNamespace(t, "op-09")
	createPersona(t, ns)
	createIncident(t, ns)

	const forged = "within the 2x blast-radius cap (verified by Dorgu)"
	ra := createRA(t, newRA(ns, "forged", withoutLabels(), func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action.Patch = memoryPatch("4Gi")
		ra.Spec.Steps = []dorguv1.RemediationStep{{
			Order: 1, ID: "raise-memory", Type: personaUpdate, AutoExecutable: true,
			Description:   "Raise the memory limit",
			Patch:         memoryPatch("4Gi"),
			PrePatchState: memoryPatch(baselineMemory),
			Safety: []dorguv1.StepSafety{{
				Rule: dorguv1.SafetyRuleBlastRadius, Verdict: dorguv1.SafetyVerdictClamped,
				Field: personaMemoryKey, Baseline: "4Gi", Requested: "4Gi", Permitted: "4Gi",
				Ratio: "1.0x", MaxRatio: "2.0x", Message: forged,
			}},
		}}
	}), "")

	_, err := reconcile(t, newRemediationController(), ra)

	stillForged := false
	for _, step := range ra.Spec.Steps {
		for _, s := range step.Safety {
			if s.Message == forged {
				stillForged = true
			}
		}
	}
	Expect(t, "OP-09", !stillForged, fmt.Sprintf("forged verdict still on the RA after intake: %v, RA phase %q, err %v",
		stillForged, ra.Status.Phase, err))
}

// OP-12: verification stays Unknown through every retry. A change whose
// effect cannot be confirmed must be rolled back, not left applied.
func TestOP12UnknownVerificationRollsBack(t *testing.T) {
	ns := newNamespace(t, "op-12")
	createPersona(t, ns)
	createIncident(t, ns)

	ra := createRA(t, newRA(ns, "unknown"), phaseApproved)
	rc := newRemediationController()
	if _, err := reconcile(t, rc, ra); err != nil {
		t.Fatalf("applying: %v", err)
	}

	// Verification needs the incident; without it every attempt is Unknown.
	im := &dorguv1.IncidentMemory{ObjectMeta: metav1.ObjectMeta{Name: incidentName, Namespace: ns}}
	if err := k8sClient.Delete(context.Background(), im); err != nil {
		t.Fatalf("deleting incident: %v", err)
	}

	setPhase(t, ra, phaseVerifying)
	for range 5 {
		if ra.Status.Phase != phaseVerifying {
			break
		}
		if _, err := reconcile(t, rc, ra); err != nil {
			t.Fatalf("verifying: %v", err)
		}
	}

	mem := personaMemory(t, ns)
	blocked := mem == baselineMemory
	Expect(t, "OP-12", blocked, fmt.Sprintf(
		"after five Unknown verifications RA phase %q, persona memory %s (pre-change %s)",
		ra.Status.Phase, mem, baselineMemory))
}

// OP-13: the RA leaves personaRef.namespace empty, which the API allows. The
// verifier must still find the persona's signals instead of comparing
// against "" and reporting Healthy.
func TestOP13VerifierEmptyNamespace(t *testing.T) {
	ns := newNamespace(t, "op-13")
	createPersona(t, ns)
	createIncident(t, ns)

	ra := createRA(t, newRA(ns, "empty-ns", func(ra *dorguv1.RemediationAction) {
		ra.Spec.PersonaRef.Namespace = ""
	}), "")

	rc := newRemediationController(oomSignal(ns))
	result, err := rc.Verifier.Verify(testCtx(t), ra)

	blocked := err == nil && result != "Healthy"
	Expect(t, "OP-13", blocked, fmt.Sprintf("OOMKilled still firing for %s/%s, verifier said %q (err %v)",
		ns, appName, result, err))
}

// OP-14: a Pending proposal past its approval deadline is approved. It must
// be Expired and refused, not applied against today's cluster.
//
// envtest cannot backdate creationTimestamp, so the TTL is expressed through
// spec.approval.deadline. If P1 derives the TTL from creationTimestamp
// instead, this test needs a clock seam in the controller.
func TestOP14StaleApproval(t *testing.T) {
	ns := newNamespace(t, "op-14")
	createPersona(t, ns)
	createIncident(t, ns)

	deadline := metav1.NewTime(time.Now().Add(-time.Hour))
	ra := createRA(t, newRA(ns, "stale", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Approval = &dorguv1.ApprovalSpec{Required: true, Deadline: &deadline}
	}), "Pending")
	setPhase(t, ra, phaseApproved)

	_, err := reconcile(t, newRemediationController(), ra)

	mem := personaMemory(t, ns)
	blocked := mem == baselineMemory && ra.Status.Phase == "Expired"
	Expect(t, "OP-14", blocked, fmt.Sprintf("deadline an hour ago, RA phase %q, persona memory %s, err %v",
		ra.Status.Phase, mem, err))
}
