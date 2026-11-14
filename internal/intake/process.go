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

package intake

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// maxConditionMessage bounds a condition message, which a gate reason can exceed.
const maxConditionMessage = 1024

// Outcome is what Process decided.
type Outcome struct {
	// Phase is Converted or Rejected.
	Phase v1alpha1.ProposalPhase

	// Action is the RemediationAction the proposal became. Nil when rejected.
	Action *dorguv1.RemediationAction

	// Decision is the gate's decision. Nil when the proposal was rejected at Intake
	// or was already decided before this call.
	Decision *gate.Decision

	// Rejection says where and why a proposal was refused. Nil when converted.
	Rejection *v1alpha1.ProposalRejection

	// Message is the sentence for people: the gate's reason, or the intake's.
	Message string

	// Replayed is true when the proposal had already been decided and nothing was done.
	Replayed bool
}

// Claim marks a proposal as one the caller will Process itself, in this process, so
// the reconciler leaves it alone until the returned release is called.
func (p *Processor) Claim(key client.ObjectKey) (release func()) {
	p.claimed.Store(key, struct{}{})
	return func() { p.claimed.Delete(key) }
}

// Claimed reports whether the planner is converting this proposal itself.
func (p *Processor) Claimed(key client.ObjectKey) bool {
	_, ok := p.claimed.Load(key)
	return ok
}

// Process decides one proposal. It is idempotent: a proposal that was already
// decided is reported again and not decided twice, and a retry after a partial
// failure finds the action an earlier attempt created.
//
// An error means nothing final was decided and the caller should retry.
func (p *Processor) Process(ctx context.Context, key client.ObjectKey, hint *Hint) (*Outcome, error) {
	defer p.lockProposal(key)()

	var prop v1alpha1.RemediationProposal
	if err := p.reader.Get(ctx, key, &prop); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if !prop.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	if decided(&prop) {
		return replayed(&prop), nil
	}

	if prop.Status.Phase == "" {
		if err := p.updateStatus(ctx, key, func(st *v1alpha1.RemediationProposalStatus) {
			st.Phase = v1alpha1.PhaseReceived
		}); err != nil {
			return nil, fmt.Errorf("recording the proposal as received: %w", err)
		}
	}

	if out, err := p.intakeChecks(ctx, &prop); out != nil || err != nil {
		return out, err
	}

	// The planner holds the persona lock for the whole of its proposal, including
	// this call. Anyone else takes it below, so two proposals for one persona never
	// race through the gate.
	if !hintMatches(hint, &prop) {
		hint = nil
	}

	var incident *dorguv1.IncidentMemory
	if hint != nil && hint.Facts.Incident != nil {
		// The planner recorded this incident before it proposed, and passes it in.
		incident = hint.Facts.Incident
	} else {
		var out *Outcome
		var err error
		if incident, out, err = p.resolveIncident(ctx, &prop); out != nil || err != nil {
			return out, err
		}
	}

	if hint == nil {
		persona := personaRefOf(&prop)
		defer p.locks.Lock(&persona)()
	}
	return p.convert(ctx, &prop, incident, hint)
}

// decided reports whether the operator has already ruled on this generation.
func decided(prop *v1alpha1.RemediationProposal) bool {
	switch prop.Status.Phase {
	case v1alpha1.PhaseConverted, v1alpha1.PhaseRejected:
		return prop.Status.ObservedGeneration == prop.Generation
	}
	return false
}

// replayed reports an earlier decision.
func replayed(prop *v1alpha1.RemediationProposal) *Outcome {
	out := &Outcome{Phase: prop.Status.Phase, Replayed: true, Rejection: prop.Status.Rejection.DeepCopy()}
	if c := meta.FindStatusCondition(prop.Status.Conditions, v1alpha1.ConditionConverted); c != nil {
		out.Message = c.Message
	}
	return out
}

func (p *Processor) lockProposal(key client.ObjectKey) func() {
	mu, _ := p.perProposal.LoadOrStore(key, newMutex())
	m := mu.(*mutex)
	m.Lock()
	return m.Unlock
}

// intakeChecks runs the checks that need nothing but the proposal and the other
// proposals: a field not yet supported, a namespace that is not the target's,
// a duplicate idempotency key, and the proposer's rate limit.
func (p *Processor) intakeChecks(ctx context.Context, prop *v1alpha1.RemediationProposal) (*Outcome, error) {
	if prop.Spec.RollbackOf != nil {
		return p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonNotYetSupported, nil,
			"rollbackOf is reserved and not supported yet: a ticketed rollback is not available in this version")
	}
	if prop.Namespace != prop.Spec.Target.Namespace {
		return p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonNamespaceMismatch, nil,
			fmt.Sprintf("the proposal is in namespace %q and its target is in %q: they must be the same", prop.Namespace, prop.Spec.Target.Namespace))
	}

	all, err := p.listProposals(ctx)
	if err != nil {
		return nil, err
	}
	if original := earlierDuplicate(prop, all); original != "" {
		return p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonDuplicate, nil,
			fmt.Sprintf("proposal %q from the same proposer already used idempotencyKey %q", original, prop.Spec.IdempotencyKey))
	}
	if n := countRecentBefore(prop, all, p.rateWindow); n >= p.rateLimit {
		return p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonRateLimited, nil,
			fmt.Sprintf("%s has already had %d proposals received in the last %s (limit %d)",
				prop.Spec.Proposer.Username, n, p.rateWindow, p.rateLimit))
	}
	return nil, nil
}

// resolveIncident finds the incident the proposal answers, or opens one when the
// proposal names none. A named incident that does not exist rejects the proposal.
func (p *Processor) resolveIncident(
	ctx context.Context, prop *v1alpha1.RemediationProposal,
) (*dorguv1.IncidentMemory, *Outcome, error) {
	if ref := prop.Spec.IncidentRef; ref != nil {
		var im dorguv1.IncidentMemory
		// An incident in another namespace is treated as absent: a proposal in one
		// namespace learns nothing about another's.
		if ref.Namespace != prop.Namespace {
			out, err := p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonIncidentNotFound, nil,
				fmt.Sprintf("incident %s/%s does not exist", ref.Namespace, ref.Name))
			return nil, out, err
		}
		err := p.reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &im)
		switch {
		case apierrors.IsNotFound(err):
			out, rejectErr := p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonIncidentNotFound, nil,
				fmt.Sprintf("incident %s/%s does not exist", ref.Namespace, ref.Name))
			return nil, out, rejectErr
		case err != nil:
			return nil, nil, fmt.Errorf("reading incident %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		return &im, nil, nil
	}

	if p.incidents == nil {
		return nil, nil, fmt.Errorf("proposal %s/%s names no incident and no incident opener is configured", prop.Namespace, prop.Name)
	}
	im, err := p.incidents.Open(ctx, prop)
	if err != nil {
		return nil, nil, fmt.Errorf("opening an incident for the proposal: %w", err)
	}
	return im, nil, nil
}

// convert runs the gate over the proposal and creates the action or rejects it.
func (p *Processor) convert(
	ctx context.Context, prop *v1alpha1.RemediationProposal, incident *dorguv1.IncidentMemory, hint *Hint,
) (*Outcome, error) {
	existing, err := p.existingAction(ctx, prop)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return p.converted(ctx, prop, incident, existing, nil)
	}

	draft, facts := derivedAction(prop, incident), factsFromIncident(incident)
	if hint != nil {
		draft, facts = hint.Draft.DeepCopy(), hint.Facts
	}

	dec, err := p.gate.Run(ctx, candidateFor(draft, facts), gate.PhaseProposal)
	if err != nil {
		return nil, fmt.Errorf("gate: %w", err)
	}
	if !dec.Allowed() {
		stage, guardrail := dec.RefusedBy()
		return p.reject(ctx, prop, incident, stage, guardrail, dec, dec.SkipReason())
	}

	record := dec
	if hint != nil && hint.Decision != nil && dec.Result == gate.ResultPass {
		// The planner's run is the one that narrowed this plan, so it is the record.
		// Run again over the finished plan it passes, with nothing left to say.
		record = hint.Decision
	} else {
		InstallDecision(draft, dec)
		draft.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
	}
	draft.Spec.Provenance = provenanceFor(prop)

	action, err := p.createAction(ctx, draft, record)
	if err != nil {
		return nil, err
	}
	return p.converted(ctx, prop, incident, action, record)
}

// existingAction finds the action an earlier attempt already created for this
// proposal. An action with no status was never proposed (its status write failed):
// it is removed so the proposal is decided afresh.
func (p *Processor) existingAction(ctx context.Context, prop *v1alpha1.RemediationProposal) (*dorguv1.RemediationAction, error) {
	var list dorguv1.RemediationActionList
	if err := p.reader.List(ctx, &list, client.InNamespace(prop.Namespace)); err != nil {
		return nil, fmt.Errorf("listing RemediationActions: %w", err)
	}
	for i := range list.Items {
		ra := &list.Items[i]
		if ra.Spec.Provenance == nil || ra.Spec.Provenance.Proposal != prop.Name ||
			ra.CreationTimestamp.Before(&prop.CreationTimestamp) {
			continue
		}
		if ra.Status.Phase == "" {
			if err := p.client.Delete(ctx, ra); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("removing the unfinished RemediationAction %s: %w", ra.Name, err)
			}
			continue
		}
		return ra, nil
	}
	return nil, nil
}

// createAction writes the RemediationAction and records the gate's decision on its
// status. It is the only place in the operator that creates a RemediationAction.
//
// An action whose status could not be written is not proposed (R11): it would sit
// with no phase, which no controller acts on and the dedup check cannot tell from a
// live one. The write is retried on a conflict; if it still fails the action is
// deleted and the error returned, so the caller reports a failure instead of a
// proposal that does not exist.
func (p *Processor) createAction(
	ctx context.Context, action *dorguv1.RemediationAction, dec *gate.Decision,
) (*dorguv1.RemediationAction, error) {
	if err := p.client.Create(ctx, action); err != nil {
		return nil, fmt.Errorf("creating RemediationAction: %w", err)
	}
	// The generation the apiserver gave this object. The Proposed condition pins it,
	// and it must not be re-read: a spec edit that lands between create and the
	// status write would otherwise be cleared by a gate that never saw it (F9).
	createdGeneration := action.GetGeneration()

	key := client.ObjectKeyFromObject(action)
	err := retry.OnError(retry.DefaultBackoff, retriable, func() error {
		var fresh dorguv1.RemediationAction
		if err := p.reader.Get(ctx, key, &fresh); err != nil {
			return err
		}
		// The Proposed condition pins the spec generation the gate cleared. An
		// approval re-check refuses a spec edited after this (M2).
		fresh.Status = dorguv1.RemediationActionStatus{
			Phase: "Pending",
			Gate:  dec.Status(),
			Conditions: []metav1.Condition{{
				Type:               dorguv1.ConditionProposed,
				Status:             metav1.ConditionTrue,
				Reason:             dorguv1.ReasonGateCleared,
				Message:            "the operator's gate cleared this plan when it was proposed",
				ObservedGeneration: createdGeneration,
				LastTransitionTime: metav1.Now(),
			}},
		}
		if err := p.client.Status().Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.DeepCopyInto(action)
		return nil
	})
	if err != nil {
		p.log.Error(err, "RemediationAction created but its status could not be written; removing it",
			"name", action.Name)
		if delErr := p.client.Delete(ctx, action); delErr != nil && !apierrors.IsNotFound(delErr) {
			p.log.Error(delErr, "could not remove the RemediationAction that has no status; it needs deleting by hand",
				"name", action.Name, "namespace", action.Namespace)
		}
		return nil, fmt.Errorf("recording the status of %s/%s: %w", action.Namespace, action.Name, err)
	}
	return action, nil
}

// converted records the proposal as Converted and owned by its action, so deleting
// the action prunes the proposal (P2-D12).
func (p *Processor) converted(
	ctx context.Context, prop *v1alpha1.RemediationProposal, incident *dorguv1.IncidentMemory,
	action *dorguv1.RemediationAction, dec *gate.Decision,
) (*Outcome, error) {
	if err := p.ownByAction(ctx, client.ObjectKeyFromObject(prop), action); err != nil {
		return nil, fmt.Errorf("making the action own the proposal: %w", err)
	}
	msg := fmt.Sprintf("converted to RemediationAction %s", action.Name)
	err := p.updateStatus(ctx, client.ObjectKeyFromObject(prop), func(st *v1alpha1.RemediationProposalStatus) {
		st.Phase = v1alpha1.PhaseConverted
		st.Rejection = nil
		st.RemediationActionRef = &v1alpha1.ActionRef{Name: action.Name}
		st.IncidentRef = incidentRefStatus(incident)
		st.ObservedGeneration = prop.Generation
		setConverted(st, metav1.ConditionTrue, v1alpha1.ReasonConverted, msg, prop.Generation)
	})
	if err != nil {
		return nil, fmt.Errorf("recording the proposal as converted: %w", err)
	}
	return &Outcome{Phase: v1alpha1.PhaseConverted, Action: action, Decision: dec, Message: msg}, nil
}

// reject records the proposal as Rejected with the stage and reason that refused it.
func (p *Processor) reject(
	ctx context.Context, prop *v1alpha1.RemediationProposal, incident *dorguv1.IncidentMemory,
	stage, reason string, dec *gate.Decision, message string,
) (*Outcome, error) {
	rejection := &v1alpha1.ProposalRejection{Stage: stage, Reason: reason}
	err := p.updateStatus(ctx, client.ObjectKeyFromObject(prop), func(st *v1alpha1.RemediationProposalStatus) {
		st.Phase = v1alpha1.PhaseRejected
		st.Rejection = rejection.DeepCopy()
		st.RemediationActionRef = nil
		st.IncidentRef = incidentRefStatus(incident)
		st.ObservedGeneration = prop.Generation
		setConverted(st, metav1.ConditionFalse, conditionReason(reason), message, prop.Generation)
	})
	if err != nil {
		return nil, fmt.Errorf("recording the proposal as rejected: %w", err)
	}
	p.log.V(1).Info("proposal rejected", "proposal", prop.Name, "namespace", prop.Namespace,
		"stage", stage, "reason", reason)
	return &Outcome{Phase: v1alpha1.PhaseRejected, Decision: dec, Rejection: rejection, Message: message}, nil
}

func incidentRefStatus(im *dorguv1.IncidentMemory) *dorguv1.IncidentReference {
	if im == nil {
		return nil
	}
	return &dorguv1.IncidentReference{Name: im.Name, Namespace: im.Namespace}
}

func setConverted(st *v1alpha1.RemediationProposalStatus, status metav1.ConditionStatus, reason, message string, generation int64) {
	if len(message) > maxConditionMessage {
		message = message[:maxConditionMessage]
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionConverted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// conditionReason turns a machine token such as "deny-list" into the CamelCase a
// condition reason requires ("DenyList").
func conditionReason(reason string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(reason, func(r rune) bool { return r == '-' || r == '_' || r == ' ' }) {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	if b.Len() == 0 {
		return "Rejected"
	}
	return b.String()
}

// updateStatus applies mutate to the proposal's status, retrying on a conflict and
// on a read that has not caught up.
func (p *Processor) updateStatus(
	ctx context.Context, key client.ObjectKey, mutate func(*v1alpha1.RemediationProposalStatus),
) error {
	return retry.OnError(retry.DefaultBackoff, retriable, func() error {
		var fresh v1alpha1.RemediationProposal
		if err := p.reader.Get(ctx, key, &fresh); err != nil {
			return err
		}
		mutate(&fresh.Status)
		return p.client.Status().Update(ctx, &fresh)
	})
}

// ownByAction makes the action the owner of the proposal, so that deleting the
// action garbage-collects the proposal. It is a non-controlling reference and does
// not block deletion.
func (p *Processor) ownByAction(ctx context.Context, key client.ObjectKey, action *dorguv1.RemediationAction) error {
	return retry.OnError(retry.DefaultBackoff, retriable, func() error {
		var fresh v1alpha1.RemediationProposal
		if err := p.reader.Get(ctx, key, &fresh); err != nil {
			return err
		}
		for _, ref := range fresh.OwnerReferences {
			if ref.UID == action.UID {
				return nil
			}
		}
		base := fresh.DeepCopy()
		fresh.OwnerReferences = append(fresh.OwnerReferences, metav1.OwnerReference{
			APIVersion: dorguv1.GroupVersion.String(),
			Kind:       "RemediationAction",
			Name:       action.Name,
			UID:        action.UID,
		})
		return p.client.Patch(ctx, &fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// retriable reports whether a failed write is worth another attempt: a conflict, or
// a read of an object the cache has not caught up with.
func retriable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsNotFound(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err)
}
