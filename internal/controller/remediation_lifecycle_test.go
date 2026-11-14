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

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// Lifecycle tests for the RemediationController that need no API server: a fake
// client is enough for the phase machine, the verifier retry counter, expiry
// and the rollback outcome.

const (
	lcDeploymentUID = "uid-web"
	lcDeploymentRV  = "7"
	lcNamespace     = "shop"
	lcPersona       = "web"
	lcIncident      = "im-web-oom"
	lcBaseline      = "256Mi"
	lcApplied       = "512Mi"
)

func lcScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := dorguv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// lcApprover is who approved the actions the fixtures hold.
const lcApprover = "alice@example.com"

func lcPersonaObj(memory string) *dorguv1.ApplicationPersona {
	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: lcPersona, Namespace: lcNamespace},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: lcPersona, Type: "api", Tier: "standard",
			Resources: &dorguv1.ResourceConstraints{Limits: &dorguv1.ResourceValues{Memory: memory, CPU: "250m"}},
		},
	}
}

func lcIncidentObj() *dorguv1.IncidentMemory {
	return &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: lcIncident, Namespace: lcNamespace},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: lcPersona, Namespace: lcNamespace},
			Category:   "resource",
			Detection: dorguv1.DetectionInfo{
				Signal:    string(detection.SignalOOMKilled),
				FirstSeen: metav1.NewTime(time.Now().Add(-time.Hour)),
				LastSeen:  metav1.NewTime(time.Now()),
			},
		},
	}
}

func lcRA(phase string, mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: "ra-1", Namespace: lcNamespace},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{Name: lcIncident, Namespace: lcNamespace},
			PersonaRef:  dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: lcPersona, Namespace: lcNamespace},
			TrustLevel:  2,
			Explanation: "Increase memory limit from 256Mi to 512Mi",
			Confidence:  "0.85",
			Action: dorguv1.RemediationActionDetail{
				Type:          "persona-update",
				Patch:         &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
				PrePatchState: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)},
			},
			Approval: &dorguv1.ApprovalSpec{Required: true},
			Rollback: &dorguv1.RemediationRollbackSpec{
				Enabled: true, HealthCheckAfter: &metav1.Duration{Duration: time.Minute}, MaxRetries: 1,
			},
		},
		Status: dorguv1.RemediationActionStatus{Phase: phase},
	}
	// steps[] is the only plan; the single action above is its derived form.
	ra.Spec.Steps = []dorguv1.RemediationStep{{
		Order: 1, ID: "step-1", Type: "persona-update", AutoExecutable: true,
		Description: ra.Spec.Explanation,
		Patch:       ra.Spec.Action.Patch.DeepCopy(), PrePatchState: ra.Spec.Action.PrePatchState.DeepCopy(),
	}}
	lcRecordProposal(ra)
	for _, m := range mutate {
		m(ra)
	}
	return ra
}

// lcRecordProposal gives ra what the operator's proposer writes when the gate
// clears a plan (a Proposal decision against the live Deployment, and the Proposed
// condition at the spec generation), and, for the phases after approval, what the
// approval re-check writes: the apply ticket and the baseline it measured against.
func lcRecordProposal(ra *dorguv1.RemediationAction) {
	now := metav1.Now()
	ra.Status.Gate = &dorguv1.GateStatus{
		Phase: string(gate.PhaseProposal), Result: string(gate.ResultPass), EvaluatedAt: now,
		Observed: &dorguv1.GateObserved{WorkloadUID: lcDeploymentUID, ObservedAt: &now},
	}
	ra.Status.Conditions = append(ra.Status.Conditions, metav1.Condition{
		Type: dorguv1.ConditionProposed, Status: metav1.ConditionTrue, Reason: dorguv1.ReasonGateCleared,
		ObservedGeneration: ra.Generation, LastTransitionTime: now,
	})

	if ra.Status.Phase == RemediationPhaseApproved {
		approved := metav1.NewTime(time.Now().Add(-time.Second))
		ra.Status.ApprovedAt = &approved
		ra.Status.ApprovedBy = lcApprover
	}

	switch ra.Status.Phase {
	case RemediationPhaseTicketed, RemediationPhaseApplying, RemediationPhaseVerifying, RemediationPhaseCompleted, RemediationPhaseRolledBack:
		ra.Status.Gate.Phase = string(gate.PhaseApprovalRecheck)
		approved := metav1.NewTime(time.Now().Add(-time.Second))
		ra.Status.ApprovedAt = &approved
		ra.Status.ApprovedBy = lcApprover
		ra.Status.Gate.PrePatchState = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"256Mi"}}}}`)}
		ra.Status.Gate.Observed.LiveValues = &dorguv1.ObservedResources{Limits: &dorguv1.ResourceValues{Memory: lcBaseline}}
		ra.Status.ApplyTicket = &dorguv1.ApplyTicket{
			WorkloadUID: lcDeploymentUID, Container: lcPersona,
			Patch:                   &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
			ObservedResourceVersion: lcDeploymentRV,
			IssuedAt:                &now,
			ExpiresAt:               metav1.NewTime(now.Add(gate.DefaultTicketTTL)),
			ApprovedBy:              lcApprover,
		}
		if hash, err := ra.Spec.Hash(); err == nil {
			ra.Status.ApplyTicket.SpecHash = hash
		}
		applied := metav1.NewTime(time.Now().Add(-11 * time.Minute))
		if ra.Status.Phase != RemediationPhaseTicketed {
			ra.Status.AppliedAt = &applied
		}
	}
}

func lcOOMSignal() detection.Signal {
	namespace := lcNamespace
	return detection.Signal{
		Type:     detection.SignalOOMKilled,
		Severity: detection.SeverityCritical,
		Category: detection.CategoryResource,
		Source:   "pod-collector",
		Resource: dorguv1.ResourceReference{Kind: "Pod", Name: lcPersona + "-abc", Namespace: namespace},
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: lcPersona, Namespace: namespace,
		},
		DetectedAt: time.Now(),
	}
}

// lcController builds the real controller over a fake client, with a detection
// engine reporting the given signals.
func lcController(t *testing.T, objs []client.Object, signals ...detection.Signal) (*RemediationController, client.WithWatch) {
	t.Helper()
	return lcControllerWithCollector(t, objs, &stubCollector{name: "lc", signals: signals})
}

func lcControllerWithCollector(t *testing.T, objs []client.Object, collector detection.SignalCollector) (*RemediationController, client.WithWatch) {
	t.Helper()
	hasDeployment := false
	for _, o := range objs {
		if _, ok := o.(*appsv1.Deployment); ok {
			hasDeployment = true
		}
	}
	if !hasDeployment {
		objs = append(objs, lcDeployment(lcBaseline))
	}
	c := fake.NewClientBuilder().WithScheme(lcScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&dorguv1.RemediationAction{}, &dorguv1.IncidentMemory{}, &dorguv1.ApplicationPersona{}).
		Build()
	log := logr.Discard()
	engine := detection.NewEngine(log, collector)
	g := gate.New(c, log)
	return &RemediationController{
		Client:   c,
		Gate:     g,
		Executor: remediation.NewExecutor(c, g, log),
		Verifier: remediation.NewVerifier(engine, c, log),
		Rollback: remediation.NewRollback(c, log),
		Logger:   log,
	}, c
}

func lcReconcile(t *testing.T, rc *RemediationController, ra *dorguv1.RemediationAction) (ctrl.Result, *dorguv1.RemediationAction) {
	t.Helper()
	ctx := context.Background()
	res, err := rc.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got dorguv1.RemediationAction
	if err := rc.Get(ctx, client.ObjectKeyFromObject(ra), &got); err != nil {
		t.Fatalf("re-reading %s: %v", ra.Name, err)
	}
	return res, &got
}

func lcPersonaMemory(t *testing.T, c client.Client) string {
	t.Helper()
	var p dorguv1.ApplicationPersona
	if err := c.Get(context.Background(), client.ObjectKey{Name: lcPersona, Namespace: lcNamespace}, &p); err != nil {
		t.Fatal(err)
	}
	return p.Spec.Resources.Limits.Memory
}

func lcCondition(ra *dorguv1.RemediationAction, condType string) *metav1.Condition {
	for i := range ra.Status.Conditions {
		if ra.Status.Conditions[i].Type == condType {
			return &ra.Status.Conditions[i]
		}
	}
	return nil
}

func lcIncidentOutcome(t *testing.T, c client.Client) string {
	t.Helper()
	var im dorguv1.IncidentMemory
	if err := c.Get(context.Background(), client.ObjectKey{Name: lcIncident, Namespace: lcNamespace}, &im); err != nil {
		t.Fatal(err)
	}
	if im.Spec.Resolution == nil {
		return ""
	}
	return im.Spec.Resolution.Outcome
}

// ---------------------------------------------------------------- 1.8: Unknown

// OP-12 and the stuck counter: setCondition keeps the message when status and
// reason are unchanged, so "retry 1/2" never advanced and the RA sat in
// Verifying forever. With no incident to read, every verification is Unknown.
func TestVerifyingUnknown_RetryCounterAdvancesThenRollsBack(t *testing.T) {
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcApplied), lcRA("Verifying")})
	ra := lcRA("Verifying")

	_, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseVerifying {
		t.Fatalf("pass 1: phase = %q, want Verifying", got.Status.Phase)
	}
	if cond := lcCondition(got, ConditionVerified); cond.Reason != "VerificationUnknownRetry1" || !strings.Contains(cond.Message, "retry 1/2") {
		t.Fatalf("pass 1: Verified = %+v, want reason VerificationUnknownRetry1 (the counter is the enum)", cond)
	}

	_, got = lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseVerifying {
		t.Fatalf("pass 2: phase = %q, want Verifying", got.Status.Phase)
	}
	if cond := lcCondition(got, ConditionVerified); cond.Reason != "VerificationUnknownRetry2" || !strings.Contains(cond.Message, "retry 2/2") {
		t.Fatalf("pass 2: Verified = %+v, want reason VerificationUnknownRetry2 (the counter must advance)", cond)
	}

	_, got = lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseRolledBack {
		t.Fatalf("pass 3: phase = %q, want RolledBack: a change that cannot be verified is not left applied", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s, want the pre-change %s", mem, lcBaseline)
	}
	rb := lcCondition(got, ConditionRolledBack)
	if rb == nil || rb.Reason != "VerificationUnknown" {
		t.Errorf("RolledBack condition = %+v, want reason VerificationUnknown", rb)
	}
}

// B2b: the verifier used to see no signals when every collector failed and
// report Healthy, so a remediation was marked Completed without ever having
// been checked. With the incident readable (the case that reached Healthy), a
// blind detection engine must read as Unknown, burn the retries, and roll back.
func TestVerifyingBlindCollectors_NeverCompleteAndRollBack(t *testing.T) {
	blind := &stubCollector{name: "lc", err: fmt.Errorf("pods is forbidden")}
	rc, c := lcControllerWithCollector(t,
		[]client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), lcRA("Verifying")}, blind)
	ra := lcRA("Verifying")

	for pass := 1; pass <= 2; pass++ {
		_, got := lcReconcile(t, rc, ra)
		if got.Status.Phase != RemediationPhaseVerifying {
			t.Fatalf("pass %d: phase = %q, want Verifying: a blind verifier must not complete the action", pass, got.Status.Phase)
		}
		if got.Status.VerificationResult != string(remediation.VerificationUnknown) {
			t.Fatalf("pass %d: verificationResult = %q, want Unknown", pass, got.Status.VerificationResult)
		}
	}

	_, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseRolledBack {
		t.Fatalf("pass 3: phase = %q, want RolledBack", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s, want the pre-change %s", mem, lcBaseline)
	}
}

func TestVerifyingUnknown_NoRollbackAvailableSaysTheChangeIsStillApplied(t *testing.T) {
	noRollback := func(ra *dorguv1.RemediationAction) { ra.Spec.Rollback.Enabled = false }
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcApplied), lcRA("Verifying", noRollback)})
	ra := lcRA("Verifying", noRollback)

	var got *dorguv1.RemediationAction
	for range 3 {
		_, got = lcReconcile(t, rc, ra)
	}
	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	msg := lcCondition(got, ConditionApplied).Message
	if !strings.Contains(msg, "STILL APPLIED") {
		t.Errorf("message = %q, want it to say the change is still applied", msg)
	}
	if mem := lcPersonaMemory(t, c); mem != lcApplied {
		t.Errorf("persona memory = %s: a disabled rollback must not touch the persona", mem)
	}
}

func TestVerifyingUnknown_RecordedFinalRetryRollsBackOnTheNextPass(t *testing.T) {
	// The counter is read back from the condition, so a pass that finds retry 2/2
	// already recorded must roll back instead of re-recording it.
	ra := lcRA("Verifying")
	ra.Status.Conditions = []metav1.Condition{{
		Type: ConditionVerified, Status: metav1.ConditionFalse, Reason: "VerificationUnknownRetry2",
		Message: fmt.Sprintf("Verification returned Unknown, retry %d/%d", 2, maxUnknownRetries),
	}}
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcApplied), ra.DeepCopy()})
	_, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseRolledBack {
		t.Fatalf("phase = %q, want RolledBack", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s, want %s", mem, lcBaseline)
	}
}

// B6: nothing parses message text. A Verified message that SAYS retry 2/2 under
// the old reason is prose, and counts for nothing.
func TestVerifyingUnknown_TheMessageIsNeverReadBackAsACounter(t *testing.T) {
	ra := lcRA("Verifying")
	ra.Status.Conditions = []metav1.Condition{{
		Type: ConditionVerified, Status: metav1.ConditionFalse, Reason: "VerificationUnknown",
		Message: "Verification returned Unknown, retry 2/2",
	}}
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcApplied), ra.DeepCopy()})
	_, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseVerifying {
		t.Fatalf("phase = %q, want Verifying: the counter is the reason enum, not the sentence", got.Status.Phase)
	}
	if cond := lcCondition(got, ConditionVerified); cond.Reason != "VerificationUnknownRetry1" {
		t.Errorf("Verified reason = %q, want a fresh count of 1", cond.Reason)
	}
}

// ------------------------------------------------------------ 1.8: partial

func lcDeployment(liveMemory string) *appsv1.Deployment {
	labels := map[string]string{"app": lcPersona}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: lcPersona, Namespace: lcNamespace, Labels: labels,
			UID: lcDeploymentUID, ResourceVersion: lcDeploymentRV,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: lcPersona,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(liveMemory)},
					},
				}}},
			},
		},
	}
}

func TestRollback_DivergedWorkloadIsRecordedAsPartial(t *testing.T) {
	// Degraded: the signal is still firing. The CLI had put 512Mi on the
	// Deployment; the operator can restore the persona but not the workload.
	rc, c := lcController(t,
		[]client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), lcDeployment(lcApplied), lcRA("Verifying")},
		lcOOMSignal())

	_, got := lcReconcile(t, rc, lcRA("Verifying"))
	if got.Status.Phase != RemediationPhaseRolledBack {
		t.Fatalf("phase = %q, want RolledBack", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s, want %s", mem, lcBaseline)
	}
	wd := lcCondition(got, ConditionWorkloadDiverged)
	if wd == nil || wd.Status != metav1.ConditionTrue {
		t.Fatalf("WorkloadDiverged = %+v, want True", wd)
	}
	if out := lcIncidentOutcome(t, c); out != "partial" {
		t.Errorf("incident outcome = %q, want partial: the persona was restored and the workload was not", out)
	}
	rb := lcCondition(got, ConditionRolledBack)
	if rb == nil || !strings.Contains(rb.Message, "persona") || !strings.Contains(strings.ToLower(rb.Message), "workload") {
		t.Errorf("RolledBack condition = %+v, want its message to say which half was restored", rb)
	}
}

func TestRollback_RestoredWorkloadIsRecordedAsRollback(t *testing.T) {
	rc, c := lcController(t,
		[]client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), lcDeployment(lcBaseline), lcRA("Verifying")},
		lcOOMSignal())

	_, got := lcReconcile(t, rc, lcRA("Verifying"))
	if got.Status.Phase != RemediationPhaseRolledBack {
		t.Fatalf("phase = %q, want RolledBack", got.Status.Phase)
	}
	if wd := lcCondition(got, ConditionWorkloadDiverged); wd == nil || wd.Status != metav1.ConditionFalse {
		t.Fatalf("WorkloadDiverged = %+v, want False", wd)
	}
	if out := lcIncidentOutcome(t, c); out != "rollback" {
		t.Errorf("incident outcome = %q, want rollback", out)
	}
}

// A rollback that could not run must say so on the record: Failed, with the
// persona still carrying the change, not a silent half state.
func TestRollback_FailedRollbackSaysThePersonaStillCarriesTheChange(t *testing.T) {
	// The gate recorded no baseline to restore: Execute refuses.
	noPre := func(ra *dorguv1.RemediationAction) { ra.Status.Gate.PrePatchState = nil }
	rc, c := lcController(t,
		[]client.Object{lcPersonaObj(lcApplied), lcIncidentObj(), lcRA("Verifying", noPre)},
		lcOOMSignal())

	_, got := lcReconcile(t, rc, lcRA("Verifying", noPre))
	if got.Status.Phase != RemediationPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	msg := lcCondition(got, ConditionApplied).Message
	if !strings.Contains(msg, "STILL APPLIED") {
		t.Errorf("message = %q, want it to say the change is still applied", msg)
	}
	if mem := lcPersonaMemory(t, c); mem != lcApplied {
		t.Errorf("persona memory = %s, want it untouched at %s", mem, lcApplied)
	}
}

// ---------------------------------------------------------------- 1.9: expiry

func lcAged(age time.Duration) func(*dorguv1.RemediationAction) {
	return func(ra *dorguv1.RemediationAction) {
		ra.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
	}
}

func TestExpiry_PendingOlderThanTTLExpires(t *testing.T) {
	ra := lcRA("Pending", lcAged(25*time.Hour))
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), ra.DeepCopy()})
	rc.PendingTTL = 24 * time.Hour

	res, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseExpired {
		t.Fatalf("phase = %q, want Expired", got.Status.Phase)
	}
	if !res.IsZero() {
		t.Errorf("an Expired action is terminal: result = %+v", res)
	}
	cond := lcCondition(got, ConditionExpired)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonApprovalTimedOut {
		t.Errorf("Expired condition = %+v, want True/%s", cond, ReasonApprovalTimedOut)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: expiry must not touch the persona", mem)
	}

	// Terminal: another pass changes nothing.
	_, again := lcReconcile(t, rc, ra)
	if again.Status.Phase != RemediationPhaseExpired {
		t.Errorf("phase after a second pass = %q, want Expired", again.Status.Phase)
	}
}

func TestExpiry_PendingWithinTTLWaitsAndRequeuesForTheDeadline(t *testing.T) {
	ra := lcRA("Pending", lcAged(time.Hour))
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), ra.DeepCopy()})
	rc.PendingTTL = 24 * time.Hour

	res, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhasePending {
		t.Fatalf("phase = %q, want Pending", got.Status.Phase)
	}
	if res.RequeueAfter < 22*time.Hour || res.RequeueAfter > 23*time.Hour+time.Minute {
		t.Errorf("RequeueAfter = %s, want about 23h so the action expires without waiting for a resync", res.RequeueAfter)
	}
}

func TestExpiry_ZeroTTLDisablesTTLExpiry(t *testing.T) {
	ra := lcRA("Pending", lcAged(1000*time.Hour))
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), ra.DeepCopy()})
	rc.PendingTTL = 0

	res, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhasePending || res.RequeueAfter != 0 {
		t.Errorf("phase = %q, requeue = %s: TTL 0 means no TTL expiry", got.Status.Phase, res.RequeueAfter)
	}
}

func TestExpiry_ExplicitDeadlineEarlierThanTTLWins(t *testing.T) {
	deadline := metav1.NewTime(time.Now().Add(-time.Minute))
	withDeadline := func(ra *dorguv1.RemediationAction) {
		ra.Spec.Approval = &dorguv1.ApprovalSpec{Required: true, Deadline: &deadline}
	}
	ra := lcRA("Pending", lcAged(time.Hour), withDeadline)
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), ra.DeepCopy()})
	rc.PendingTTL = 24 * time.Hour

	_, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseExpired {
		t.Fatalf("phase = %q, want Expired: the author's deadline has passed", got.Status.Phase)
	}
}

func TestExpiry_AuthorDeadlineBeyondTTLCannotExtendIt(t *testing.T) {
	deadline := metav1.NewTime(time.Now().Add(30 * 24 * time.Hour))
	withDeadline := func(ra *dorguv1.RemediationAction) {
		ra.Spec.Approval = &dorguv1.ApprovalSpec{Required: true, Deadline: &deadline}
	}
	ra := lcRA("Pending", lcAged(25*time.Hour), withDeadline)
	rc, _ := lcController(t, []client.Object{lcPersonaObj(lcBaseline), ra.DeepCopy()})
	rc.PendingTTL = 24 * time.Hour

	_, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseExpired {
		t.Fatalf("phase = %q, want Expired: the TTL is operator-owned and bounds the author's deadline", got.Status.Phase)
	}
}

func TestExpiry_StaleApprovalIsRefusedNotApplied(t *testing.T) {
	ra := lcRA("Approved", lcAged(25*time.Hour))
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), ra.DeepCopy()})
	rc.PendingTTL = 24 * time.Hour

	_, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseExpired {
		t.Fatalf("phase = %q, want Expired", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcBaseline {
		t.Errorf("persona memory = %s: a stale approval must not apply", mem)
	}
}

func TestExpiry_FreshApprovalStillApplies(t *testing.T) {
	ra := lcRA("Approved", lcAged(time.Hour))
	rc, c := lcController(t, []client.Object{lcPersonaObj(lcBaseline), ra.DeepCopy()})
	rc.PendingTTL = 24 * time.Hour

	_, got := lcReconcile(t, rc, ra)
	if got.Status.Phase != RemediationPhaseApplying {
		t.Fatalf("phase = %q, want Applying", got.Status.Phase)
	}
	if mem := lcPersonaMemory(t, c); mem != lcApplied {
		t.Errorf("persona memory = %s, want %s", mem, lcApplied)
	}
}

// Expired is not a rejection and not a failure: neither the rejection cooldown
// nor the failed-remediation cooldown may hold it against the persona.
func TestExpiry_TriggersNoCooldown(t *testing.T) {
	expired := lcRA(RemediationPhaseExpired)
	expired.Labels = map[string]string{LabelPersonaName: lcPersona, LabelPersonaKind: "ApplicationPersona"}
	expired.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	expired.Status.Conditions = []metav1.Condition{{
		Type: ConditionExpired, Status: metav1.ConditionTrue, Reason: ReasonApprovalTimedOut,
		LastTransitionTime: metav1.Now(), Message: "x",
	}}
	im := lcIncidentObj()
	im.Status.Phase = PhaseDetected
	_, c := lcController(t, []client.Object{expired, im})

	// The gate, not the controller, owns the failed-remediation cooldown. An Expired
	// action must not trip it.
	g := gate.New(c, logr.Discard())
	cand := gate.Candidate{
		Target:      dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: lcPersona, Namespace: lcNamespace},
		Namespace:   lcNamespace,
		Name:        "ra-2",
		IncidentRef: dorguv1.IncidentReference{Name: lcIncident, Namespace: lcNamespace},
		Facts:       gate.Facts{Incident: im},
	}
	decision, err := g.Run(context.Background(), cand, gate.PhaseProposal)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range decision.Verdicts {
		if v.Verdict == dorguv1.GateVerdictRefuse &&
			(v.Guardrail == gate.GuardrailFailedCooldown || v.Guardrail == gate.GuardrailRejection) {
			t.Errorf("an Expired action tripped a cooldown: %+v", v)
		}
	}
}
