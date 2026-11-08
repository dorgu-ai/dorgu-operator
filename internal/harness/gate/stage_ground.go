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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/workload"
)

// groundStage re-reads the live workload and the persona. Every number a later
// stage measures against comes from here, never from the plan: a persona is a
// point-in-time import that drifts, and a proposal can be hours old.
type groundStage struct{ g *Gate }

func (groundStage) Name() string { return StageGround }

func (s groundStage) Run(ctx context.Context, cand *Candidate, d *Decision) error {
	now := metav1.NewTime(s.g.now())
	key := personaKey(cand)

	ref, obs, ambiguous, err := s.observeWorkload(ctx, key.Namespace, key.Name, now)
	if err != nil {
		return err
	}
	d.WorkloadRef, d.Observation = ref, obs

	persona, err := s.readPersona(ctx, cand, key)
	if err != nil {
		return err
	}
	d.Persona = persona

	d.Observed = &dorguv1.GateObserved{ObservedAt: &now}
	if persona != nil {
		d.Observed.PersonaGeneration = persona.Generation
	}
	if Resolved(ref) {
		d.Observed.LiveValues = ref.ObservedResources.DeepCopy()
	}
	if obs != nil && obs.Deployment != nil {
		d.Observed.WorkloadUID = string(obs.Deployment.UID)
		d.Observed.WorkloadResourceVersion = obs.Deployment.ResourceVersion
	}

	if d.Phase == PhaseApprovalRecheck {
		if ambiguous {
			d.refuse(StageGround, GuardrailWorkloadAmbiguous,
				"more than one Deployment now answers to this persona, so the plan cannot be tied to one workload", nil)
			return nil
		}
		if !s.checkStillTheSameWorkload(cand, d) {
			return nil
		}
	}

	// A change to a persona that is not there has nowhere to land, at proposal as
	// much as at approval: a plan written against a missing persona is a plan
	// nobody could have measured. (R4)
	if persona == nil && hasPersonaUpdate(d) {
		d.refuse(StageGround, GuardrailPersona,
			fmt.Sprintf("ApplicationPersona %s/%s does not exist, so the change has nowhere to land", key.Namespace, key.Name), nil)
		return nil
	}

	d.PrePatchState = prePatchFromLive(d, persona)

	if Resolved(ref) {
		d.pass(StageGround, GuardrailLiveWorkload,
			fmt.Sprintf("read Deployment %s/%s", ref.Namespace, ref.Name), nil)
	} else {
		d.pass(StageGround, GuardrailLiveWorkload,
			"no live Deployment could be resolved; sizing falls back to the persona and the workload is treated as owned", nil)
	}
	return nil
}

// observeWorkload reads the live Deployment for the persona.
//
// A read error is returned at both phases: the gate cannot decide against a
// workload it could not read, and a Deployment list that fails is not the same as
// a persona with no Deployment (R3). Two Deployments answering to one persona is
// reported as ambiguous rather than guessed at: at proposal that degrades to an
// unresolved workload (every consumer treats it as owned and sizes from the
// persona), at approval Run refuses it (R2).
func (s groundStage) observeWorkload(
	ctx context.Context, namespace, name string, now metav1.Time,
) (ref *dorguv1.WorkloadRef, obs *workload.Observation, ambiguous bool, err error) {
	if namespace == "" || name == "" {
		return workload.UnresolvedRef(now), nil, false, nil
	}
	obs, err = workload.Observe(ctx, s.g.client, namespace, name)
	if err != nil {
		var amb *workload.AmbiguousError
		if errors.As(err, &amb) {
			s.g.logger.V(0).Info("several Deployments answer to this persona; the workload is treated as unresolved and owned",
				"persona", name, "namespace", namespace, "error", err.Error())
			return workload.UnresolvedRef(now), nil, true, nil
		}
		return nil, nil, false, fmt.Errorf("reading the live workload for %s/%s: %w", namespace, name, err)
	}
	if obs == nil {
		s.g.logger.V(1).Info("no Deployment resolved for persona; grounding falls back to the persona",
			"persona", name, "namespace", namespace, "tried", workload.ChainDescription())
		return workload.UnresolvedRef(now), nil, false, nil
	}
	return obs.Ref(now), obs, false, nil
}

// readPersona reads the target persona, nil when it does not exist. It returns an
// error only for a failed read.
func (s groundStage) readPersona(ctx context.Context, cand *Candidate, key client.ObjectKey) (*dorguv1.ApplicationPersona, error) {
	if cand.Target.Kind != kindApplicationPersona || key.Namespace == "" {
		return nil, nil
	}
	var persona dorguv1.ApplicationPersona
	if err := s.g.client.Get(ctx, key, &persona); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading ApplicationPersona %s/%s: %w", key.Namespace, key.Name, err)
	}
	return &persona, nil
}

// checkStillTheSameWorkload refuses an approval whose workload was deleted or
// replaced since the proposal: a Deployment that is gone, or one with a different
// UID, is not the workload the facts the plan was reasoned against describe.
//
// A plan proposed against no live workload (no prior UID) has nothing to be
// replaced; it is re-measured against whatever is there now.
func (groundStage) checkStillTheSameWorkload(cand *Candidate, d *Decision) bool {
	prior := cand.Facts.PriorObserved
	if prior == nil || prior.WorkloadUID == "" {
		return true
	}
	live := d.Observed.WorkloadUID
	switch {
	case live == "":
		d.refuse(StageGround, GuardrailWorkloadGone,
			"the live Deployment this was proposed against no longer exists, so the plan no longer describes anything",
			map[string]string{"proposedUID": prior.WorkloadUID})
		return false
	case prior.WorkloadUID != live:
		d.refuse(StageGround, GuardrailLiveWorkload,
			"the live Deployment was replaced since this was proposed (its UID changed), so the plan no longer describes it",
			map[string]string{"proposedUID": prior.WorkloadUID, "liveUID": live})
		return false
	}
	return true
}

// hasPersonaUpdate reports whether the plan, as the gate holds it, would write to
// the persona: any persona-update step, or a persona-update action. The executor
// applies Action.Patch, so the action counts whether or not steps are present.
func hasPersonaUpdate(d *Decision) bool {
	for i := range d.Steps {
		if d.Steps[i].Type == dorguv1.StepTypePersonaUpdate {
			return true
		}
	}
	return d.Action.Type == dorguv1.ActionTypePersonaUpdate
}

// hasAutoApplicable reports whether the plan, as the gate now holds it, still has
// something the operator would apply: an auto-executable persona-update step, or
// the Action the executor actually writes (R1).
func hasAutoApplicable(d *Decision) bool {
	for i := range d.Steps {
		step := &d.Steps[i]
		if step.AutoExecutable && step.Type == dorguv1.StepTypePersonaUpdate && step.Patch != nil && len(step.Patch.Raw) > 0 {
			return true
		}
	}
	return actionAppliable(d)
}

// actionAppliable reports whether Action carries a persona-update patch. The
// executor applies Action.Patch and nothing else, so this is the change that
// reaches the cluster, whatever the steps say.
func actionAppliable(d *Decision) bool {
	return d.Action.Type == dorguv1.ActionTypePersonaUpdate && d.Action.Patch != nil && len(d.Action.Patch.Raw) > 0
}

// baselineValues returns, for each resource leaf of a patch, the value the change
// is measured against: the live workload's when it resolved, otherwise the
// persona's. It is computed here from the cluster; a plan's own prePatchState is
// never read, because the author of a plan chooses what that says.
//
// A resolved workload that does not set a key yields no baseline for it: the
// change would introduce the key, which the absent-field guardrail refuses.
func baselineValues(patch []byte, d *Decision) map[string]string {
	out := make(map[string]string)
	for _, path := range PatchLeafPaths(patch) {
		if !IsResourcePath(path) {
			continue
		}
		if live, ok := ObservedValue(d.WorkloadRef, path); ok {
			out[path] = live
		}
	}
	if Resolved(d.WorkloadRef) || d.Persona == nil {
		return out
	}
	if snapshot := SnapshotPrePatch(d.Persona, patch); snapshot != nil {
		for path, value := range PatchLeafValues(snapshot) {
			if _, ok := out[path]; !ok && IsResourcePath(path) {
				out[path] = value
			}
		}
	}
	return out
}

// prePatchFromLive is the baseline snapshot recorded on status.gate: every
// resource path the plan touches, with the value the guardrails measure against.
func prePatchFromLive(d *Decision, _ *dorguv1.ApplicationPersona) *apiextensionsv1.JSON {
	merged := make(map[string]string)
	collect := func(patch *apiextensionsv1.JSON) {
		if patch == nil || len(patch.Raw) == 0 {
			return
		}
		maps.Copy(merged, baselineValues(patch.Raw, d))
	}
	for i := range d.Steps {
		if d.Steps[i].Type == dorguv1.StepTypePersonaUpdate {
			collect(d.Steps[i].Patch)
		}
	}
	if d.Action.Type == dorguv1.ActionTypePersonaUpdate {
		collect(d.Action.Patch)
	}
	if len(merged) == 0 {
		return nil
	}

	root := make(map[string]any)
	for _, path := range SortedStringKeys(merged) {
		mergeNestedPath(root, strings.Split(path, "."), merged[path])
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return nil
	}
	return &apiextensionsv1.JSON{Raw: raw}
}
