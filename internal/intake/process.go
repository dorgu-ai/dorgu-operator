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
	"errors"
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
	"github.com/dorgu-ai/dorgu-operator/internal/safetext"
)

// maxConditionMessage bounds a condition message, which a gate reason can exceed.
const maxConditionMessage = 1024

// maxAPIMessage bounds how much of an API server's refusal is repeated in a
// proposal's status.
const maxAPIMessage = 512

// shortened is an API server's message with control characters removed, cut to
// maxAPIMessage characters.
func shortened(msg string) string {
	msg = safetext.Clean(msg, false)
	if r := []rune(msg); len(r) > maxAPIMessage {
		return string(r[:maxAPIMessage]) + "..."
	}
	return msg
}

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
	// A closed intake decides nothing: not even a status is written, so a proposal
	// waits exactly as it was written until the intake opens.
	if f := p.Blocked(); f != nil {
		return nil, &NotReadyError{Failure: *f}
	}

	unlockProposal, err := p.lockProposal(ctx, key)
	if err != nil {
		return nil, err
	}
	defer unlockProposal()

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

	// The planner holds the persona lock for the whole of its proposal, including
	// this call, and says so. Whether the hint describes this proposal decides what
	// is believed; it never decides whether the lock is taken, or a mismatched hint
	// would wait forever on the lock its own caller holds.
	callerLocked := hint != nil && hint.PersonaLocked
	plannerRun := hintMatches(hint, &prop, p.username)
	if !plannerRun {
		hint = nil
	}

	if out, err := p.intakeChecks(ctx, &prop, plannerRun); out != nil || err != nil {
		return out, err
	}

	if !callerLocked {
		// Anyone else takes the lock here, so two proposals for one persona never
		// race through the gate. A rollback decides on what is open for its action
		// under it, so it is taken before anything is read.
		persona := personaRefOf(&prop)
		unlock, err := p.locks.Acquire(ctx, &persona, "", p.lockTimeout)
		if err != nil {
			return nil, fmt.Errorf("waiting for the lock on persona %s/%s: %w", persona.Namespace, persona.Name, err)
		}
		defer unlock()
	}

	var derived *gate.RollbackDerivation
	if prop.Spec.RollbackOf != nil {
		out, d, err := p.planRollback(ctx, &prop)
		if out != nil || err != nil {
			return out, err
		}
		derived = d
	}

	var incident *dorguv1.IncidentMemory
	switch {
	case hint != nil && hint.Facts.Incident != nil:
		// The planner recorded this incident before it proposed, and passes it in.
		// It is held to the same rule as a named one: a bug there must not turn into
		// a change on the wrong persona.
		incident = hint.Facts.Incident
		if out, err := p.checkIncident(ctx, &prop, incident); out != nil || err != nil {
			return out, err
		}
	default:
		var out *Outcome
		var err error
		if incident, out, err = p.resolveIncident(ctx, &prop); out != nil || err != nil {
			return out, err
		}
	}

	return p.convert(ctx, &prop, incident, hint, derived)
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

func (p *Processor) lockProposal(ctx context.Context, key client.ObjectKey) (func(), error) {
	unlock, err := p.perProposal.acquire(ctx, key.String(), p.lockTimeout)
	if err != nil {
		return nil, fmt.Errorf("waiting for proposal %s: %w", key, err)
	}
	return unlock, nil
}

// intakeChecks runs the checks that need nothing but the proposal and the other
// proposals: a field not yet supported, a namespace that is not the target's,
// a duplicate idempotency key, and the proposer's rate limit.
//
// plannerRun is true only for a proposal decided with a matched in-process hint
// (hintMatches): proof that Dorgu's own planner wrote it, in this process. Only
// that is exempt from the per-proposer rate limit. proposer.username is a claim
// until the admission policy verifies it, so the username never exempts anyone.
// The gate's per-persona limits are not the intake's and still apply.
func (p *Processor) intakeChecks(
	ctx context.Context, prop *v1alpha1.RemediationProposal, plannerRun bool,
) (*Outcome, error) {
	if problem := usernameProblem(prop.Spec.Proposer.Username); problem != "" {
		return p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonProposerInvalid, nil,
			fmt.Sprintf("spec.proposer.username %s: an identity is recorded exactly as it is or not at all, "+
				"so write the proposal as a user whose name has none of those", problem))
	}
	if prop.Spec.RollbackOf != nil && strings.HasPrefix(prop.Spec.Proposer.Username, systemUserPrefix) {
		return p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonRollbackNotHuman, nil,
			"a rollback is a request for a person to approve a change to a workload, so only a person may ask for one: "+
				"a service account, an agent or the operator cannot")
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
	if plannerRun {
		return nil, nil
	}
	if n := countRecentBefore(prop, all, p.rateWindow); n >= p.rateLimit {
		return p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonRateLimited, nil,
			fmt.Sprintf("%q has already had %d proposals received in the last %s (limit %d)",
				prop.Spec.Proposer.Username, n, p.rateWindow, p.rateLimit))
	}
	return nil, nil
}

// resolveIncident finds the incident the proposal answers, or opens one when the
// proposal names none. A named incident that does not exist rejects the proposal.
func (p *Processor) resolveIncident(
	ctx context.Context, prop *v1alpha1.RemediationProposal,
) (*dorguv1.IncidentMemory, *Outcome, error) {
	// A rollback answers an incident of its own, never one the requester named: the
	// original action's incident keeps the resolution it was given.
	if ref := prop.Spec.IncidentRef; ref != nil && prop.Spec.RollbackOf == nil {
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
		out, err := p.checkIncident(ctx, prop, &im)
		if out != nil || err != nil {
			return nil, out, err
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

// checkIncident refuses an incident that cannot answer this proposal: one detected
// on a different persona than the proposal's target (it would unlock a change
// the detection never justified, and defeat "advisory only without a Dorgu-detected
// incident"), or one that is already Resolved (the planner never proposes against
// one). Either way the proposal is Rejected at Intake. An incident that names no
// persona namespace means its own.
func (p *Processor) checkIncident(
	ctx context.Context, prop *v1alpha1.RemediationProposal, im *dorguv1.IncidentMemory,
) (*Outcome, error) {
	have := im.Spec.PersonaRef
	if have.Namespace == "" {
		have.Namespace = im.Namespace
	}
	if want := personaRefOf(prop); have != want {
		return p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonIncidentTargetMismatch, nil,
			fmt.Sprintf("incident %s/%s is for %s %s/%s, not the proposal's target %s %s/%s",
				im.Namespace, im.Name, have.Kind, have.Namespace, have.Name, want.Kind, want.Namespace, want.Name))
	}
	if im.Status.Phase == IncidentPhaseResolved {
		return p.reject(ctx, prop, nil, v1alpha1.StageIntake, v1alpha1.ReasonIncidentResolved, nil,
			fmt.Sprintf("incident %s/%s is already resolved", im.Namespace, im.Name))
	}
	return nil, nil
}

// convert runs the gate over the proposal and creates the action or rejects it.
func (p *Processor) convert(
	ctx context.Context, prop *v1alpha1.RemediationProposal, incident *dorguv1.IncidentMemory, hint *Hint,
	derived *gate.RollbackDerivation,
) (*Outcome, error) {
	existing, err := p.existingAction(ctx, prop)
	var orphan *orphanedActionError
	if errors.As(err, &orphan) {
		return p.reject(ctx, prop, incident, v1alpha1.StageIntake, v1alpha1.ReasonActionInvalid, nil, orphan.Error())
	}
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return p.converted(ctx, prop, incident, existing, nil)
	}

	draft, facts := derivedAction(prop, incident), factsFromIncident(incident)
	switch {
	case derived != nil:
		// The operator derived this plan from its own record of a resource change, so it
		// is a resource adjustment whatever the rollback's incident says: the class the
		// gate admits a persona-update for. The requester's words decided nothing.
		draft, facts = rollbackDraft(prop, incident, derived), factsFromIncident(incident)
		facts.SuggestedAction = gate.ClassResourceAdjustment
	case hint != nil:
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

	// status.gate is the record to trust, so it records this run: the one that
	// authorised the creation. A planner's earlier decision over the same plan is
	// never substituted for it.
	if hint == nil || dec.Result != gate.ResultPass {
		InstallDecision(draft, dec)
		draft.Spec.Action = dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification}
	}
	if derived != nil {
		draft.Spec.Provenance = rollbackProvenance(prop, p.username)
	} else {
		draft.Spec.Provenance = provenanceFor(prop, hint != nil)
	}
	if draft.Annotations == nil {
		draft.Annotations = map[string]string{}
	}
	draft.Annotations[AnnotationProposalUID] = string(prop.UID)

	action, err := p.createAction(ctx, draft, dec)
	if apierrors.IsInvalid(err) {
		// The API server will refuse this action however often it is asked, so the
		// proposal ends here: an error would make the reconciler retry it forever.
		//
		// But not over an orphan. When the refusal was of the status written after the
		// create, the action created a moment ago was deleted, and if that delete failed
		// it is still there with no phase, which the gate counts as an active remediation
		// for this persona. A rejected proposal never reaches convert again, so nothing
		// would ever remove it: take it away now, and retry the whole proposal if that
		// cannot be done.
		if _, cleanErr := p.actionFor(ctx, prop, true, p.reader); cleanErr != nil && !errors.As(cleanErr, new(*orphanedActionError)) {
			return nil, fmt.Errorf("removing the unfinished RemediationAction before rejecting the proposal: %w", cleanErr)
		}
		return p.reject(ctx, prop, incident, v1alpha1.StageIntake, v1alpha1.ReasonActionInvalid, dec,
			"the API server refused the RemediationAction this proposal would become: "+shortened(err.Error()))
	}
	if err != nil {
		return nil, err
	}
	return p.converted(ctx, prop, incident, action, dec)
}

// AnnotationProposalUID is stamped on every action the intake creates: the UID of
// the proposal it was converted from. Provenance names the proposal; this pins the
// one object, so a retry after a partial failure finds the action it made, and
// never one made for an earlier proposal of the same name.
const AnnotationProposalUID = "dorgu.io/proposal-uid"

// existingAction finds the action an earlier attempt already created for this
// proposal. An action with no status was never proposed (its status write failed):
// it is removed so the proposal is decided afresh.
func (p *Processor) existingAction(ctx context.Context, prop *v1alpha1.RemediationProposal) (*dorguv1.RemediationAction, error) {
	return p.actionFor(ctx, prop, true, p.lister)
}

// FindAction returns the proposed action that exists for the proposal, or nil. It
// changes nothing. The planner uses it after a failed decision: if the action was
// created, the proposal is not undone, because that would leave an action whose
// provenance names a proposal that is gone. It reads the API server, not the
// cache: a cache that has not yet seen an action created a moment ago must not
// make the planner delete the proposal that action names.
func (p *Processor) FindAction(ctx context.Context, key client.ObjectKey) (*dorguv1.RemediationAction, error) {
	var prop v1alpha1.RemediationProposal
	if err := p.reader.Get(ctx, key, &prop); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return p.actionFor(ctx, &prop, false, p.reader)
}

// actionFor finds the action made for this proposal: the one stamped with the
// proposal's UID. from is where the list comes from (the cache, or the API server
// when a stale answer would do harm). An action with no phase is unfinished, and is
// removed when removeUnfinished is set, whether or not it carries provenance: one
// that lost its provenance and could not be deleted at the time is such an orphan,
// and must not stand in the way of the proposal's own action. An action with a
// phase is never removed.
//
// The list may be the cache's own objects (UnsafeDisableDeepCopy), so nothing
// returned aliases it.
func (p *Processor) actionFor(
	ctx context.Context, prop *v1alpha1.RemediationProposal, removeUnfinished bool, from client.Reader,
) (*dorguv1.RemediationAction, error) {
	var list dorguv1.RemediationActionList
	if err := from.List(ctx, &list, client.InNamespace(prop.Namespace), client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("listing RemediationActions: %w", err)
	}
	for i := range list.Items {
		ra := &list.Items[i]
		uid, stamped := ra.Annotations[AnnotationProposalUID]
		if !stamped || uid != string(prop.UID) ||
			(ra.Spec.Provenance != nil && ra.Spec.Provenance.Proposal != prop.Name) {
			continue
		}
		if ra.Status.Phase == "" {
			if !removeUnfinished {
				continue
			}
			// The list may come from a cache that has not seen the status yet. Only an
			// action the API server itself shows with no phase is removed.
			var fresh dorguv1.RemediationAction
			if err := p.reader.Get(ctx, client.ObjectKeyFromObject(ra), &fresh); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("re-reading the RemediationAction %s: %w", ra.Name, err)
			}
			if fresh.Status.Phase != "" {
				if fresh.Spec.Provenance == nil {
					return nil, orphanOf(&fresh)
				}
				return &fresh, nil
			}
			// The delete names the version just read as well as the UID: anyone who
			// writes a phase in between makes it a conflict (an ordinary retry), and the
			// action they gave a phase to is not taken with it.
			if err := p.deleteAction(ctx, &fresh, true); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("removing the unfinished RemediationAction %s: %w", ra.Name, err)
			}
			continue
		}
		if ra.Spec.Provenance == nil {
			// Stamped with this proposal's UID, with a phase, and with no provenance: an
			// action this intake made, lost the provenance of (an API server whose
			// schema dropped it) and could not delete, and that somebody then gave a
			// phase. It cannot be removed (it has a phase) and cannot be adopted (nothing
			// says who proposed it), so the proposal ends here, naming it.
			if removeUnfinished {
				return nil, orphanOf(ra)
			}
			continue
		}
		return ra.DeepCopy(), nil
	}
	return nil, nil
}

// orphanedActionError is an action this intake made for the proposal that can be
// neither removed (it has a phase) nor adopted (it has no provenance).
type orphanedActionError struct{ name string }

func (e *orphanedActionError) Error() string {
	return fmt.Sprintf("the RemediationAction %q was made for this proposal but has no provenance and has already been given a phase, "+
		"so it can be neither removed nor adopted: delete it by hand, then write the proposal again", e.name)
}

func orphanOf(ra *dorguv1.RemediationAction) error {
	return &orphanedActionError{name: shortened(ra.Name)}
}

// deleteAction removes the action it was given and no other: the delete carries the
// object's UID, so a name that has since been reused, or an object recreated, is not
// removed in its place. With withVersion it carries the object's resourceVersion as
// well, for an action that was read just before to be sure it had no phase; the ones
// this call has just created and nobody else has touched keep the UID alone.
func (p *Processor) deleteAction(ctx context.Context, action *dorguv1.RemediationAction, withVersion bool) error {
	uid := action.UID
	if uid == "" {
		return p.client.Delete(ctx, action)
	}
	pre := client.Preconditions{UID: &uid}
	if rv := action.ResourceVersion; withVersion && rv != "" {
		pre.ResourceVersion = &rv
	}
	return p.client.Delete(ctx, action, pre)
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

	// The client fills action from the response, so what the API server kept is
	// what is here. A schema without spec.provenance prunes it without an error, and
	// an action with none reads as one the operator proposed itself, which is the
	// fail-open direction for the approval rules: take it back and close the intake.
	if action.Spec.Provenance == nil {
		if delErr := p.deleteAction(ctx, action, false); delErr != nil && !apierrors.IsNotFound(delErr) {
			p.log.Error(delErr, "could not remove a RemediationAction that lost its provenance; it needs deleting by hand",
				"name", action.Name, "namespace", action.Namespace)
		}
		failure := Failure{Check: "action-provenance", Reason: ReasonActionProvenanceMissing, Message: missingProvenance().Error()}
		p.preflight.Report(failure)
		return nil, &NotReadyError{Failure: failure}
	}

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
		if delErr := p.deleteAction(ctx, action, false); delErr != nil && !apierrors.IsNotFound(delErr) {
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
