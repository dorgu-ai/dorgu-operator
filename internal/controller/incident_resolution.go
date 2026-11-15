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

	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// resolveCleared closes the incidents whose applications can be shown to have
// recovered, and leaves every other incident open.
//
// The old rule was two absences: no matching signal this cycle, and a grace
// period since the last one. Neither is evidence of anything. A crash loop
// backs off in lengthening intervals up to five minutes, so a pod that is
// completely dead falls silent inside the grace period and reads as fixed. That
// is how platform/checkout reached 51 occurrences, went Resolved, and stayed in
// CrashLoopBackOff, while dorgu health reported one active incident with three
// applications down (F-01).
//
// Silence now only opens the question. verifyRecovery has to answer it with
// something observed: pods that exist, are Ready, and have stayed Ready without
// restarting for RecoveryStabilityWindow. Anything else, including any error
// reading the cluster, leaves the incident open.
func (r *HealthCheckReconciler) resolveCleared(ctx context.Context, activeSignalKeys map[string]bool) error {
	return r.resolveClearedHolding(ctx, activeSignalKeys, detection.ResolutionSnapshot{})
}

// resolveClearedHolding is resolveCleared with what the engine said about silence when
// the signals were collected (Engine.ResolutionSnapshot, read right after CollectAll): the
// families that were blind, and the containers that were hot without a signal yet. An
// incident of a held family stays open even if the source has recovered since: a verifier
// cycle between the collect and this step must not turn a blind collection into evidence.
// The live answer is asked too, so a source that went blind after the collect holds as
// well.
func (r *HealthCheckReconciler) resolveClearedHolding(
	ctx context.Context, activeSignalKeys map[string]bool, held detection.ResolutionSnapshot,
) error {
	// Use label selector to only fetch non-resolved incidents.
	var list dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &list,
		client.MatchingLabels{LabelPhase: PhaseDetected},
	); err != nil {
		return fmt.Errorf("listing active IncidentMemories: %w", err)
	}

	// Also check Investigating phase incidents.
	var investigatingList dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &investigatingList,
		client.MatchingLabels{LabelPhase: PhaseInvestigating},
	); err != nil {
		return fmt.Errorf("listing investigating IncidentMemories: %w", err)
	}
	list.Items = append(list.Items, investigatingList.Items...)

	now := time.Now()
	for i := range list.Items {
		im := &list.Items[i]

		// Build signal key from incident fields.
		key := incidentSignalKey(im)
		if activeSignalKeys[key] {
			continue // Still active.
		}

		// Silence from a source that cannot look is not silence. A CPU throttling
		// incident is about a pod that is Ready and never restarts, so verifyRecovery
		// below would pass it whether or not it is still throttled. Blindness is scoped
		// to the signal's family (detection.FamilyCollector): every other incident is
		// judged as usual. It includes the warm-up after a blind spell: until the source
		// has read as many consecutive cycles as the signal needs, a pod that is still
		// throttled has not had time to look throttled.
		if r.resolutionBlind(detection.SignalType(im.Spec.Detection.Signal), held.BlindFamilies) {
			r.Logger.V(1).Info("incident stays open: the source that would report this signal cannot look",
				"name", im.Name, "signal", im.Spec.Detection.Signal)
			continue
		}

		// Check grace period: signal must be absent for ResolutionGracePeriod.
		if now.Sub(im.Spec.Detection.LastSeen.Time) < ResolutionGracePeriod {
			continue
		}

		// The source as a whole can be warm and still new to one container of this
		// workload: a pod pinned at its limit that has not yet been read for as many
		// cycles as raise the signal reads as quiet. Its incident stays open until it is.
		if ref, pending := r.resolutionPending(ctx, im, held); pending {
			r.Logger.V(1).Info("incident stays open: a container of its workload is at the usage line and has not been read long enough to raise the signal",
				"name", im.Name, "signal", im.Spec.Detection.Signal,
				"pod", ref.Pod, "container", ref.Container)
			continue
		}

		evidence := r.resolutionEvidence(ctx, im, now)
		if !evidence.Recovered() {
			r.Logger.V(1).Info("incident stays open: recovery could not be established",
				"name", im.Name,
				"signal", im.Spec.Detection.Signal,
				"reason", evidence.Reason,
			)
			continue
		}

		if err := r.markResolved(ctx, im, evidence); err != nil {
			logFailure(r.Logger, err, "failed to resolve incident", "name", im.Name)
			continue
		}

		r.Logger.Info("auto-resolved incident",
			"name", im.Name,
			"category", im.Spec.Category,
			"signal", im.Spec.Detection.Signal,
			"lastSeen", im.Spec.Detection.LastSeen.Time,
			"evidence", evidence.Reason,
		)

	}

	return nil
}

// resolutionBlind reports whether the detection engine cannot vouch for the absence of a
// signal of this type well enough to close an incident: the collector that alone observes
// its family is failing, has no source, has not yet looked in this process (a restart),
// or has not yet read enough consecutive cycles since it could not (Engine.ResolutionBlind).
// It is read from the engine in this process, not from a persisted status, which after a
// restart still holds the value from before it. held is the snapshot taken at collect time.
func (r *HealthCheckReconciler) resolutionBlind(signal detection.SignalType, held map[detection.Family]bool) bool {
	if family, ok := detection.FamilyOf(signal); ok && held[family] {
		return true
	}
	return r.Detection != nil && r.Detection.ResolutionBlind(signal)
}

// resolutionPending reports whether the engine, at collect time (held) or now, names a
// container of the incident's workload as pending for the incident's signal family: hot,
// or throttled and not yet read again (detection.ResolutionPender). The workload's pods are
// the ones verifyRecovery inspects. When they cannot be listed the incident is held: an
// answer that could not be checked is not a reason to close it.
func (r *HealthCheckReconciler) resolutionPending(
	ctx context.Context, im *dorguv1.IncidentMemory, held detection.ResolutionSnapshot,
) (detection.ContainerRef, bool) {
	family, ok := detection.FamilyOf(detection.SignalType(im.Spec.Detection.Signal))
	if !ok {
		return detection.ContainerRef{}, false
	}
	var live detection.ResolutionSnapshot
	if r.Detection != nil {
		live = r.Detection.ResolutionSnapshot()
	}
	if len(held.PendingContainers[family]) == 0 && len(live.PendingContainers[family]) == 0 {
		return detection.ContainerRef{}, false
	}

	namespace := incidentNamespace(im)
	pods, err := r.workloadPods(ctx, im, namespace)
	if err != nil {
		logFailure(r.Logger, err, "could not list the workload's pods to check them for pending CPU readings", "name", im.Name)
		return detection.ContainerRef{}, true
	}
	for i := range pods {
		for _, ctr := range pods[i].Spec.Containers {
			ref := detection.ContainerRef{Namespace: pods[i].Namespace, Pod: pods[i].Name, Container: ctr.Name}
			if held.Pending(family, ref) || live.Pending(family, ref) {
				return ref, true
			}
		}
	}
	return detection.ContainerRef{}, false
}

// resolutionEvidence is the full test an incident must pass to close: either its
// workload has been observed healthy, or the incident has been superseded by
// one filed against a persona that now claims the same workload.
func (r *HealthCheckReconciler) resolutionEvidence(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	now time.Time,
) recoveryEvidence {
	if evidence, superseded := r.supersededByPersona(ctx, im); superseded {
		return evidence
	}
	return r.verifyRecovery(ctx, im, now)
}

// supersededByPersona closes an unattributed incident once an attributed one is
// already tracking the same workload, so that onboarding an app mid-outage does
// not leave it counted twice.
//
// The test is the replacement incident, not the persona. A persona existing
// only means the workload could be attributed from now on; it says nothing
// about whether the outage is still recorded anywhere. Closing on that alone
// would hand back exactly the failure this round exists to remove: a broken
// application with no open incident. So this hands over only when there is
// something to hand over to, and the resolution it writes says handover rather
// than recovery, because nothing here observed the workload at all.
func (r *HealthCheckReconciler) supersededByPersona(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
) (recoveryEvidence, bool) {
	if im.Spec.Attribution != AttributionUnattributed {
		return recoveryEvidence{}, false
	}

	namespace := incidentNamespace(im)
	if namespace == "" {
		return recoveryEvidence{}, false
	}
	workloadName := incidentWorkloadName(im)

	var attributed dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &attributed,
		client.InNamespace(namespace),
		client.MatchingLabels{
			LabelPersonaKind: "ApplicationPersona",
			LabelPersonaName: workloadName,
			LabelAttribution: AttributionPersona,
		},
	); err != nil {
		r.Logger.V(1).Info("could not check whether an attributed incident now covers the workload",
			"incident", im.Name, "error", err)
		return recoveryEvidence{}, false
	}

	for i := range attributed.Items {
		other := &attributed.Items[i]
		if other.Name == im.Name || other.Status.Phase == PhaseResolved {
			continue
		}
		return healthy(fmt.Sprintf(
			"superseded: workload %s is now tracked by incident %s, which is open against its persona",
			workloadName, other.Name)), true
	}

	return recoveryEvidence{}, false
}

// markResolved writes the resolution and the phase, recording on the object
// itself what was observed. Both writes retry on Conflict and on NotFound: this
// runs alongside the incident and remediation controllers, and a resolution
// dropped on a stale ResourceVersion leaves an incident that reads as open for
// an application that is fine.
func (r *HealthCheckReconciler) markResolved(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	evidence recoveryEvidence,
) error {
	specErr := retryIncidentWrite(func(int) error {
		var fresh dorguv1.IncidentMemory
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), &fresh); err != nil {
			return err
		}
		if fresh.Labels == nil {
			fresh.Labels = map[string]string{}
		}
		fresh.Labels[LabelPhase] = PhaseResolved
		fresh.Spec.Resolution = &dorguv1.ResolutionInfo{
			Action:  resolutionAction(evidence),
			Outcome: "resolved",
		}
		if err := r.Client.Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.DeepCopyInto(im)
		return nil
	})
	if specErr != nil {
		return fmt.Errorf("recording the resolution: %w", specErr)
	}

	statusErr := retryIncidentWrite(func(int) error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
			return err
		}
		im.Status.Phase = PhaseResolved
		resolveProposalRefusal(&im.Status.Conditions)
		return r.Client.Status().Update(ctx, im)
	})
	if statusErr != nil {
		return fmt.Errorf("recording the resolved phase: %w", statusErr)
	}

	// No RemediationAction was involved: the incident recovered on its own evidence.
	// It is counted apart from a heal.
	metrics.RecordIncidentResolved(metrics.OutcomeCleared)
	return nil
}
