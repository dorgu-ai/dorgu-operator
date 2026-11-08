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
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// Re-review N1, N2, N4 at the controller: what a healthy verification credits, and
// how the new approval and ticket status writes behave when they lose a race.

// withDeploymentManagedFields serves the given managedFields on every Deployment
// listed, as the API server does and the fake client does not.
func withDeploymentManagedFields(c client.WithWatch, entries []metav1.ManagedFieldsEntry) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := inner.List(ctx, list, opts...); err != nil {
				return err
			}
			if deployments, ok := list.(*appsv1.DeploymentList); ok {
				for i := range deployments.Items {
					deployments.Items[i].ManagedFields = entries
				}
			}
			return nil
		},
	})
}

func lcLimits(memory string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)}
}

func lcStamp(manager, fields string) metav1.ManagedFieldsEntry {
	at := metav1.NewTime(time.Now().Add(time.Second))
	return metav1.ManagedFieldsEntry{
		Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1",
		FieldsType: "FieldsV1", Time: &at, FieldsV1: &metav1.FieldsV1{Raw: []byte(fields)},
	}
}

// lcTicketFields is what the CLI's patch of the ticket owns on the Deployment.
var lcTicketFields = fmt.Sprintf(`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"%s\"}":{".":{},"f:name":{},"f:resources":{".":{},"f:limits":{".":{},"f:memory":{}}}}}}}}}`, lcPersona)

const (
	lcRevisionFields = `{"f:metadata":{"f:annotations":{".":{},"f:deployment.kubernetes.io/revision":{}}}}`
	lcSpoofedFields  = `{"f:spec":{"f:replicas":{},"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"app\"}":{"f:image":{}}}}}}}`
)

func reconcileWithManagedFields(t *testing.T, generation int64, entries ...metav1.ManagedFieldsEntry) (*dorguv1.RemediationAction, client.WithWatch) {
	t.Helper()
	ra := lcRA("Verifying", lcPatchedByTheCLI)
	dep := lcDeploymentAtGeneration(generation)
	dep.ResourceVersion = "9" // the writes below moved it
	rc, c := healthyController(t, ra, dep)
	served := withDeploymentManagedFields(c, entries)
	rc.Client = served
	rc.Gate = gate.New(served, logr.Discard())
	_, got := lcReconcile(t, rc, ra)
	return got, c
}

func TestVerifyingHealthy_ASpoofedControllerStampIsNotTheCLIsPatch(t *testing.T) {
	// kube-controller-manager is a name any client can send. This entry owns
	// replicas and an image, so it is somebody else writing the workload.
	got, c := reconcileWithManagedFields(t, 2,
		lcStamp(gate.DorguFieldManager, lcTicketFields),
		lcStamp("kube-controller-manager", lcSpoofedFields))

	if out := lcIncidentOutcome(t, c); out != IncidentOutcomePartial {
		t.Errorf("incident outcome = %q, want partial: a spoofed controller write must not hide divergence", out)
	}
	if cond := lcCondition(got, ConditionWorkloadDiverged); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("%s = %+v, want True", ConditionWorkloadDiverged, cond)
	}
	if cond := lcCondition(got, ConditionHealCredited); cond != nil && cond.Status == metav1.ConditionTrue {
		t.Errorf("%s = %+v: a diverged workload must not earn heal credit", ConditionHealCredited, cond)
	}
}

func TestVerifyingHealthy_TheRealControllerStampAndTheCLIsPatchAreClean(t *testing.T) {
	got, c := reconcileWithManagedFields(t, 2,
		lcStamp(gate.DorguFieldManager, lcTicketFields),
		lcStamp("kube-controller-manager", lcRevisionFields))

	if out := lcIncidentOutcome(t, c); out != IncidentOutcomeResolved {
		t.Errorf("incident outcome = %q, want resolved", out)
	}
	if cond := lcCondition(got, ConditionHealCredited); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("%s = %+v, want True", ConditionHealCredited, cond)
	}
}

func TestVerifyingHealthy_AGenerationNothingExplainsIsDiverged(t *testing.T) {
	// Generation 7 against the ticket's 1 plus the CLI's one patch: somebody removed
	// a field (a JSON patch leaves no stamp), and the old managedFields predate the ticket.
	old := lcStamp("kubectl-client-side-apply", lcTicketFields)
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	old.Time = &past
	got, c := reconcileWithManagedFields(t, 7, old)

	if out := lcIncidentOutcome(t, c); out != IncidentOutcomePartial {
		t.Errorf("incident outcome = %q, want partial", out)
	}
	if cond := lcCondition(got, ConditionWorkloadDiverged); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("%s = %+v, want True", ConditionWorkloadDiverged, cond)
	}
}

func TestF10_HealCreditNeedsTheLiveResourcesToBeTheTickets(t *testing.T) {
	// The CLI recorded its patch and the generation moved, but the container holds
	// 128Mi, not the ticket's 512Mi: whatever changed it was not this heal.
	ra := lcRA("Verifying", lcPatchedByTheCLI)
	dep := lcDeploymentAtGeneration(2)
	dep.Spec.Template.Spec.Containers[0].Resources.Limits = lcLimits("128Mi")
	rc, c := healthyController(t, ra, dep)

	_, got := lcReconcile(t, rc, ra)

	if out := lcIncidentOutcome(t, c); out != IncidentOutcomeSelfResolved {
		t.Errorf("incident outcome = %q, want %s", out, IncidentOutcomeSelfResolved)
	}
	cond := lcCondition(got, ConditionHealCredited)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("%s = %+v, want False", ConditionHealCredited, cond)
	}
	if cond.Reason != ReasonResourcesNotTicket {
		t.Errorf("reason = %q, want %s", cond.Reason, ReasonResourcesNotTicket)
	}
}

// --- N4: the approval and ticket status writes lose races quietly -----------------

// conflictOnStatusUpdate makes the RemediationAction's status write lose a race.
func conflictOnStatusUpdate(c client.WithWatch) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, inner client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*dorguv1.RemediationAction); ok {
				return apierrors.NewConflict(schema.GroupResource{Group: "dorgu.io", Resource: "remediationactions"}, obj.GetName(),
					fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
			}
			return inner.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
}

func errorLines(sink *recordingSink) int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	n := 0
	for _, e := range *sink.entries {
		if e.isError {
			n++
		}
	}
	return n
}

func TestN4_AConflictOnAnApprovalOrTicketWriteRequeuesQuietly(t *testing.T) {
	for _, phase := range []string{"Approved", "Ticketed", "Applying", "Verifying"} {
		t.Run(phase, func(t *testing.T) {
			ra := lcRA(phase, func(ra *dorguv1.RemediationAction) {
				if phase == "Applying" {
					past := metav1.NewTime(time.Now().Add(-time.Hour))
					ra.Status.AppliedAt = &past
				}
			})
			rc, c := healthyController(t, ra)
			logger, sink := newRecordingLogger()
			rc.Logger = logger
			rc.Client = conflictOnStatusUpdate(c)

			res, err := rc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})

			if err != nil {
				t.Fatalf("a status conflict must not be returned as an error (it is counted and logged at ERROR): %v", err)
			}
			if res.RequeueAfter <= 0 {
				t.Errorf("result = %+v, want a short requeue", res)
			}
			if n := errorLines(sink); n != 0 {
				t.Errorf("%d ERROR lines for a benign conflict, want none: %v", n, sink.messages())
			}
		})
	}
}

func TestN4_AFailedApprovalReCheckIsReturnedOnceNotLoggedAndReturned(t *testing.T) {
	ra := lcRA("Approved")
	rc, c := healthyController(t, ra)
	logger, sink := newRecordingLogger()
	rc.Logger = logger
	broken := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*appsv1.DeploymentList); ok {
				return fmt.Errorf("etcdserver: request timed out")
			}
			return inner.List(ctx, list, opts...)
		},
	})
	rc.Gate = gate.New(broken, logr.Discard())

	_, err := rc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})

	if err == nil {
		t.Fatal("an approval re-check that could not run must be retried (returned), never passed")
	}
	if n := errorLines(sink); n != 0 {
		t.Errorf("%d ERROR lines: the returned error is logged by controller-runtime, logging it here too doubles it: %v", n, sink.messages())
	}
}
