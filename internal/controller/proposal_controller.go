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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

// claimedRecheck is how long the reconciler waits before looking again at a
// proposal the planner is converting itself in process.
const claimedRecheck = 5 * time.Second

// ProposalReconciler is the operator's intake: it decides every
// RemediationProposal, and is the only code that turns one into a
// RemediationAction (through intake.Processor).
//
// A proposal's rationale is untrusted text. Nothing here reads it.
type ProposalReconciler struct {
	client.Client
	Processor *intake.Processor
}

// +kubebuilder:rbac:groups=dorgu.io,resources=remediationproposals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationproposals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationactions,verbs=create
// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories,verbs=create
// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories/status,verbs=update

// Reconcile decides a proposal once, and prunes a Rejected one when its retention
// ends.
func (r *ProposalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("proposal", req.NamespacedName)

	// The planner converts its own proposals in process, with knowledge it keeps out
	// of the spec. Leave those to it; if it fails and lets go, the proposal is ours.
	if r.Processor.Claimed(req.NamespacedName) {
		return ctrl.Result{RequeueAfter: claimedRecheck}, nil
	}

	var prop dorguv1alpha1.RemediationProposal
	if err := r.Get(ctx, req.NamespacedName, &prop); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !prop.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if prop.Status.Phase == dorguv1alpha1.PhaseRejected && prop.Status.ObservedGeneration == prop.Generation {
		return r.pruneRejected(ctx, &prop)
	}

	out, err := r.Processor.Process(ctx, req.NamespacedName, nil)
	if err != nil {
		logger.Error(err, "could not decide the proposal; will retry")
		return ctrl.Result{}, fmt.Errorf("deciding proposal %s: %w", req.NamespacedName, err)
	}
	if out != nil && out.Phase == dorguv1alpha1.PhaseRejected {
		return ctrl.Result{RequeueAfter: r.Processor.RejectedTTL()}, nil
	}
	return ctrl.Result{}, nil
}

// pruneRejected deletes a Rejected proposal once it has been kept for the
// retention period (P2-D12), measured from when it was rejected, and otherwise
// asks to be called again when that time comes.
func (r *ProposalReconciler) pruneRejected(ctx context.Context, prop *dorguv1alpha1.RemediationProposal) (ctrl.Result, error) {
	rejectedAt := prop.CreationTimestamp.Time
	if c := meta.FindStatusCondition(prop.Status.Conditions, dorguv1alpha1.ConditionConverted); c != nil && !c.LastTransitionTime.IsZero() {
		rejectedAt = c.LastTransitionTime.Time
	}
	remaining := r.Processor.RejectedTTL() - r.Processor.Now().Sub(rejectedAt)
	if remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	if err := r.Delete(ctx, prop, client.Preconditions{UID: &prop.UID}); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("pruning rejected proposal %s/%s: %w", prop.Namespace, prop.Name, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler.
func (r *ProposalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dorguv1alpha1.RemediationProposal{}).
		Named("remediationproposal").
		Complete(r)
}
