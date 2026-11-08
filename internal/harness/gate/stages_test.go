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

package gate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ------------------------------------------------------------------- ownership

func helmOwned(d *appsv1.Deployment) {
	d.Labels["app.kubernetes.io/managed-by"] = "Helm"
	d.Annotations = map[string]string{"meta.helm.sh/release-name": "shop", "meta.helm.sh/release-namespace": testNS}
}

func TestOwnership_IsResolvedFromTheLiveWorkload(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi", helmOwned)))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

	require.NotNil(t, d.Ownership)
	assert.Equal(t, dorguv1.ManagedByHelm, d.Ownership.ManagedBy)
	assert.True(t, d.Ownership.Resolved)
	assert.NotEmpty(t, d.Ownership.Detail)
	assert.Equal(t, dorguv1.ManagedByHelm, verdict(d, GuardrailOwnership).Numbers["managedBy"])
	assert.Contains(t, verdict(d, GuardrailOwnership).Reason, "recommends and does not write")
}

func TestOwnership_UnmanagedWorkload(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	d := run(t, g, planCandidate(), PhaseProposal)
	assert.Equal(t, dorguv1.ManagedByUnmanaged, d.Ownership.ManagedBy)
	assert.Equal(t, "workload is unmanaged", verdict(d, GuardrailOwnership).Reason)
}

func TestOwnership_FinalPlanMayNotWriteAnOwnedWorkload(t *testing.T) {
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi", helmOwned)))
	writer := dorguv1.RemediationStep{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypeManual,
		Command: "kubectl set resources deployment/web -n shop --limits=memory=512Mi",
	}
	reader := dorguv1.RemediationStep{
		Order: 2, ID: "step-2", Type: dorguv1.StepTypeManual, Command: "kubectl get pods -n shop",
	}

	// Mid-flight the proposer has yet to reshape the step: not a violation.
	mid := planCandidate(writer, reader)
	assert.NotEqual(t, "refuse", verdictKind(run(t, g, mid, PhaseProposal), GuardrailOwnerCommands))

	// Sealed, it is.
	final := planCandidate(writer, reader)
	final.Final = true
	d := run(t, g, final, PhaseProposal)
	assert.Equal(t, ResultRefuse, d.Result)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailOwnerCommands))
	assert.Equal(t, "1", verdict(d, GuardrailOwnerCommands).Numbers["step"])

	// A read-only command, or an unmanaged workload, is fine.
	readOnly := planCandidate(reader)
	readOnly.Final = true
	assert.True(t, run(t, g, readOnly, PhaseProposal).Allowed())

	unmanaged := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi")))
	assert.True(t, run(t, unmanaged, final, PhaseProposal).Allowed())
}

// ------------------------------------------------------------------------ mode

func TestModePolicy(t *testing.T) {
	tests := []struct {
		name    string
		objects []clientObject
		want    string
		allowed bool
	}{
		{"no ClusterPersona defaults to propose", nil, dorguv1.SelfHealingModePropose, true},
		{"propose", []clientObject{clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModePropose })},
			dorguv1.SelfHealingModePropose, true},
		{"observe withholds the proposal", []clientObject{clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModeObserve })},
			dorguv1.SelfHealingModeObserve, false},
		{"auto-approve is not implemented and acts as propose", []clientObject{clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModeAutoApprove })},
			dorguv1.SelfHealingModeAutoApprove, true},
		{"an unrecognised mode acts as propose", []clientObject{clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = "yolo" })},
			"yolo", true},
		{"a persona without a mode is skipped", []clientObject{
			clusterPersona(nil),
			func() clientObject {
				cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModeObserve })
				cp.Name = "second"
				return cp
			}(),
		}, dorguv1.SelfHealingModeObserve, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append([]clientObject{persona("256Mi"), deployment("256Mi")}, tt.objects...)
			d := run(t, newTestGate(t, fakeClient(t, objs...)), planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseProposal)

			assert.Equal(t, tt.allowed, d.Allowed(), d.SkipReason())
			assert.Equal(t, tt.want, verdict(d, GuardrailSelfHealingMode).Numbers["mode"])
			if !tt.allowed {
				assert.Equal(t, SkipReasonObserveMode, d.SkipReason())
			}
		})
	}
}

func TestModePolicy_ListFailureDegradesToPropose(t *testing.T) {
	g := newTestGate(t, failingListClient(t, isKind[*dorguv1.ClusterPersonaList], persona("256Mi")))
	assert.Equal(t, dorguv1.SelfHealingModePropose, modePolicyStage{g}.selfHealingMode(t.Context()))
}

func TestModePolicy_AlsoGatesAnApproval(t *testing.T) {
	cp := clusterPersona(func(p *dorguv1.SelfHealingPolicy) { p.Mode = dorguv1.SelfHealingModeObserve })
	g := newTestGate(t, fakeClient(t, persona("256Mi"), deployment("256Mi"), cp))
	d := run(t, g, planCandidate(personaUpdateStep(memPatch("300Mi"))), PhaseApprovalRecheck)
	assert.Equal(t, ResultRefuse, d.Result)
}

// ---------------------------------------------------------------------- dedup

func incident(name string) *dorguv1.IncidentMemory {
	im := &dorguv1.IncidentMemory{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}}
	im.Spec.Severity = "warning"
	im.Spec.Detection.Signal = "OOMKilled"
	return im
}

func dedupCandidate() Candidate {
	cand := planCandidate(personaUpdateStep(memPatch("300Mi")))
	cand.IncidentRef.Name = incidentOne
	cand.Facts = Facts{Incident: incident(incidentOne), Severity: "warning", PrimarySignal: "OOMKilled", TargetPath: ResourcePathMemory}
	return cand
}

func dedupGate(t *testing.T, objs ...clientObject) *Gate {
	t.Helper()
	return newTestGate(t, fakeClient(t, append([]clientObject{persona("256Mi"), deployment("256Mi")}, objs...)...))
}

func TestDedup_AnActiveActionForTheSameIncident(t *testing.T) {
	for _, phase := range []string{"", "Pending"} {
		t.Run("phase="+phase, func(t *testing.T) {
			dup := sibling("dup", phase, func(ra *dorguv1.RemediationAction) { ra.Spec.IncidentRef.Name = incidentOne })
			d := run(t, dedupGate(t, dup), dedupCandidate(), PhaseProposal)
			assert.Equal(t, ResultRefuse, d.Result)
			assert.Equal(t, "refuse", verdictKind(d, GuardrailActiveAction))
			assert.Contains(t, d.SkipReason(), "active remediation already exists for incident im-1")
		})
	}
}

func TestDedup_AnActiveActionForTheSameFixOfAnotherIncident(t *testing.T) {
	sameFix := sibling("same-fix", "Pending", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: memPatch("384Mi")}
	})
	d := run(t, dedupGate(t, sameFix), dedupCandidate(), PhaseProposal)
	assert.Equal(t, "refuse", verdictKind(d, GuardrailActiveAction))
	assert.Contains(t, d.SkipReason(), "targeting "+ResourcePathMemory+" (via same-fix)")

	viaStep := sibling("via-step", "Pending", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Steps = []dorguv1.RemediationStep{personaUpdateStep(memPatch("384Mi"))}
	})
	assert.Equal(t, "refuse", verdictKind(run(t, dedupGate(t, viaStep), dedupCandidate(), PhaseProposal), GuardrailActiveAction))

	cpu := sibling("cpu-fix", "Pending", func(ra *dorguv1.RemediationAction) {
		ra.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: jsonOf(`{"spec":{"resources":{"limits":{"cpu":"1"}}}}`)}
	})
	assert.Equal(t, "pass", verdictKind(run(t, dedupGate(t, cpu), dedupCandidate(), PhaseProposal), GuardrailActiveAction),
		"a different resource is a different fix")
}

func TestDedup_TerminalAndOtherPersonaActionsDoNotBlock(t *testing.T) {
	for _, phase := range []string{"Completed", "Failed", "RolledBack", "Rejected", "Expired", "Acknowledged"} {
		t.Run(phase, func(t *testing.T) {
			old := sibling("old", phase, func(ra *dorguv1.RemediationAction) {
				ra.Spec.IncidentRef.Name = incidentOne
				ra.CreationTimestamp = metav1.NewTime(fixedNow.Add(-3 * time.Hour))
			})
			d := run(t, dedupGate(t, old), dedupCandidate(), PhaseProposal)
			assert.Equal(t, "pass", verdictKind(d, GuardrailActiveAction))
		})
	}
}

func TestDedup_NoIncidentOrApprovalSkipsTheStage(t *testing.T) {
	dup := sibling("dup", "Pending", func(ra *dorguv1.RemediationAction) { ra.Spec.IncidentRef.Name = incidentOne })

	noIncident := dedupCandidate()
	noIncident.Facts.Incident = nil
	d := run(t, dedupGate(t, dup), noIncident, PhaseProposal)
	assert.Nil(t, verdict(d, GuardrailActiveAction))

	d = run(t, dedupGate(t, dup), dedupCandidate(), PhaseApprovalRecheck)
	assert.Nil(t, verdict(d, GuardrailActiveAction), "at approval the plan IS the active remediation")
}

func rejected(rejectedAt *time.Time) *dorguv1.RemediationAction {
	return sibling("rejected", "Rejected", func(ra *dorguv1.RemediationAction) {
		ra.Spec.IncidentRef.Name = incidentOne
		if rejectedAt != nil {
			ra.Status.Conditions = []metav1.Condition{{
				Type: ConditionRejected, Status: metav1.ConditionTrue, Reason: "UserRejected",
				LastTransitionTime: metav1.NewTime(*rejectedAt),
			}}
		}
	})
}

func TestRejectionCooldown(t *testing.T) {
	at := func(d time.Duration) *time.Time { v := fixedNow.Add(-d); return &v }

	tests := []struct {
		name   string
		mutate func(*Candidate)
		ra     *dorguv1.RemediationAction
		want   string
	}{
		{"a recent no is respected", nil, rejected(at(2 * time.Minute)), "refuse"},
		{"an un-timestamped no is still a no", nil, rejected(nil), "refuse"},
		{"the cooldown expires", nil, rejected(at(RejectionCooldown + time.Minute)), "pass"},
		{"a more severe signal is a different question", func(c *Candidate) { c.Facts.Severity = "critical" },
			rejected(at(time.Minute)), "pass"},
		{"a different signal is a different proposal", func(c *Candidate) { c.Facts.PrimarySignal = "CrashLoopBackOff" },
			rejected(at(time.Minute)), "pass"},
		{"a rejection for another incident does not mute this one", nil,
			func() *dorguv1.RemediationAction {
				ra := rejected(at(time.Minute))
				ra.Spec.IncidentRef.Name = "im-other"
				return ra
			}(), "pass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cand := dedupCandidate()
			if tt.mutate != nil {
				tt.mutate(&cand)
			}
			d := run(t, dedupGate(t, tt.ra), cand, PhaseProposal)
			assert.Equal(t, tt.want, verdictKind(d, GuardrailRejection))
			if tt.want == "refuse" {
				stage, guardrail := d.RefusedBy()
				assert.Equal(t, StageDedupCooldown, stage)
				assert.Equal(t, GuardrailRejection, guardrail)
			}
		})
	}
}

func TestRejectionTime(t *testing.T) {
	_, ok := rejectionTime(&dorguv1.RemediationAction{})
	assert.False(t, ok)
	ra := &dorguv1.RemediationAction{Status: dorguv1.RemediationActionStatus{Conditions: []metav1.Condition{
		{Type: ConditionRejected}, // present but never stamped
	}}}
	_, ok = rejectionTime(ra)
	assert.False(t, ok)
}

// ------------------------------------------------------------- operator namespace

func TestOperatorNamespace(t *testing.T) {
	t.Run("from POD_NAMESPACE", func(t *testing.T) {
		t.Setenv(OperatorNamespaceEnv, "dorgu-system")
		assert.Equal(t, "dorgu-system", OperatorNamespace())
	})
	t.Run("from the service account file", func(t *testing.T) {
		t.Setenv(OperatorNamespaceEnv, "")
		path := filepath.Join(t.TempDir(), "namespace")
		require.NoError(t, os.WriteFile(path, []byte("ops\n"), 0o600))
		restore := serviceAccountNamespaceFile
		serviceAccountNamespaceFile = path
		t.Cleanup(func() { serviceAccountNamespaceFile = restore })
		assert.Equal(t, "ops", OperatorNamespace())
	})
	t.Run("unknown is empty", func(t *testing.T) {
		t.Setenv(OperatorNamespaceEnv, "")
		restore := serviceAccountNamespaceFile
		serviceAccountNamespaceFile = filepath.Join(t.TempDir(), "missing")
		t.Cleanup(func() { serviceAccountNamespaceFile = restore })
		assert.Empty(t, OperatorNamespace())
	})
}
