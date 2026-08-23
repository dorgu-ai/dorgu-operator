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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/workload"
)

const (
	// RecoveryStabilityWindow is how long a workload must stay continuously
	// Ready, with no container restarts, before Dorgu will call it recovered.
	//
	// A crash loop backs off in lengthening intervals, up to five minutes
	// between restarts. Inside any one of those gaps the pod produces no fresh
	// signal while remaining completely dead, which is how an incident reached
	// 51 occurrences and was then marked Resolved with the pod still in
	// CrashLoopBackOff (F-01). The window has to outlast the longest backoff
	// interval, or silence keeps reading as health.
	RecoveryStabilityWindow = 6 * time.Minute

	// ResolutionActionPrefix marks a resolution Dorgu reached on its own. What
	// follows it is the evidence, so the reason an incident closed lives on the
	// object and not only in a log line that has since rotated away.
	ResolutionActionPrefix = "auto-resolved"
)

// resolutionAction renders what gets written to spec.resolution.action.
func resolutionAction(evidence recoveryEvidence) string {
	return ResolutionActionPrefix + ": " + evidence.Reason
}

// recoveryVerdict is what a single observation says about recovery.
//
// Three states, not two. "Says nothing" has to be distinguishable from
// "healthy", or a set of observations that are all silent adds up to a
// confident yes: a pod on its way out does not disqualify recovery, but it is
// not evidence of it either, and treating the two the same is how an absence
// gets counted as a presence all over again.
type recoveryVerdict int

const (
	// verdictHealthy is a positive observation: this was seen working.
	verdictHealthy recoveryVerdict = iota

	// verdictUnknown neither confirms nor rules out recovery.
	verdictUnknown

	// verdictBroken rules recovery out.
	verdictBroken
)

// recoveryEvidence is one observation and what it says.
//
// The Reason is user-facing in both directions: it is logged when an incident
// stays open, and written to spec.resolution.action when one closes.
type recoveryEvidence struct {
	verdict recoveryVerdict

	// Reason states what was observed.
	Reason string
}

// Recovered reports whether this evidence is enough to close an incident.
func (e recoveryEvidence) Recovered() bool { return e.verdict == verdictHealthy }

func healthy(reason string) recoveryEvidence {
	return recoveryEvidence{verdict: verdictHealthy, Reason: reason}
}

func unknown(format string, args ...any) recoveryEvidence {
	return recoveryEvidence{verdict: verdictUnknown, Reason: fmt.Sprintf(format, args...)}
}

func broken(format string, args ...any) recoveryEvidence {
	return recoveryEvidence{verdict: verdictBroken, Reason: fmt.Sprintf(format, args...)}
}

// verifyRecovery decides whether an incident's workload can be shown to have
// recovered, at the moment of asking.
//
// Auto-resolution used to need only two things: no matching signal in the
// current cycle, and a grace period since the last one. Both are absences.
// This asks for a presence instead: pods that exist, are Ready, have been Ready
// for RecoveryStabilityWindow, and have not restarted or gone back into a
// waiting state inside that window. If the cluster cannot be read, or the pods
// cannot be found, or they are there but not yet stable, the incident stays
// open. Under-reporting an outage is the one failure this product cannot
// afford.
func (r *HealthCheckReconciler) verifyRecovery(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	now time.Time,
) recoveryEvidence {
	namespace := incidentNamespace(im)
	if namespace == "" {
		return broken("the incident names no namespace, so its workload cannot be inspected")
	}

	pods, err := r.workloadPods(ctx, im, namespace)
	if err != nil {
		return broken("the workload's pods could not be listed: %v", err)
	}

	confirmed := 0
	for i := range pods {
		evidence := podRecoveryEvidence(&pods[i], now)
		if evidence.verdict == verdictBroken {
			return evidence
		}
		if evidence.verdict == verdictHealthy {
			confirmed++
		}
	}

	if confirmed == 0 {
		// Either there were no pods, or every one of them was on its way out or
		// already finished. Nothing was observed working, so this falls through
		// to asking the workload itself whether it should have pods running at
		// all, rather than reading a set of silent observations as a yes.
		return r.verifyAbsentWorkload(ctx, im, namespace)
	}

	return healthy(fmt.Sprintf("%d pod(s) Ready with no restarts for %s",
		confirmed, RecoveryStabilityWindow))
}

// verifyAbsentWorkload decides what "no pods at all" means.
//
// A workload the user deleted is genuinely gone and its incident should close,
// or it would stay open forever. A workload that still exists and wants
// replicas but has none running is not recovered, it is down, and the absence
// of pods is the outage rather than the end of it.
func (r *HealthCheckReconciler) verifyAbsentWorkload(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	namespace string,
) recoveryEvidence {
	var deployments appsv1.DeploymentList
	if err := r.Client.List(ctx, &deployments, client.InNamespace(namespace)); err != nil {
		return broken("no pods were found and the Deployments could not be listed: %v", err)
	}

	deploy, _, err := workload.Resolve(deployments.Items, incidentWorkloadName(im))
	if err != nil {
		return broken("no pods were found and the workload is ambiguous: %v", err)
	}
	if deploy == nil {
		return healthy("the workload has no pods and no Deployment: it is no longer running")
	}

	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	if desired > 0 {
		return broken("Deployment %s wants %d running and none are",
			deploy.Name, desired)
	}

	return healthy(fmt.Sprintf("Deployment %s is scaled to zero", deploy.Name))
}

// podRecoveryEvidence judges one pod.
func podRecoveryEvidence(pod *corev1.Pod, now time.Time) recoveryEvidence {
	if pod.DeletionTimestamp != nil {
		// On its way out. It says nothing about recovery either way, and
		// whatever replaces it is what gets judged.
		return unknown("pod %s is terminating", pod.Name)
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		// A run-to-finish pod is never Ready and never will be, so it is no
		// evidence of a healthy workload and no evidence against one.
		return unknown("pod %s completed", pod.Name)
	}
	if pod.Status.Phase == corev1.PodFailed {
		// Terminal, and terminal in the wrong direction. Named explicitly so
		// the user can see which pod is holding the incident open, since an
		// evicted or failed pod can outlive the failure that produced it.
		return broken("pod %s is in phase Failed (%s)", pod.Name, pod.Status.Reason)
	}

	ready := podReadyCondition(pod)
	if ready == nil || ready.Status != corev1.ConditionTrue {
		return broken("pod %s is not Ready", pod.Name)
	}

	if stableFor := now.Sub(ready.LastTransitionTime.Time); stableFor < RecoveryStabilityWindow {
		return broken("pod %s has only been Ready for %s, and recovery needs %s",
			pod.Name, stableFor.Round(time.Second), RecoveryStabilityWindow)
	}

	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.ContainerStatuses,
		pod.Status.InitContainerStatuses,
	} {
		for i := range statuses {
			if evidence := containerRecoveryEvidence(pod.Name, &statuses[i], now); evidence.verdict == verdictBroken {
				return evidence
			}
		}
	}

	return healthy(fmt.Sprintf("pod %s is Ready and stable", pod.Name))
}

// containerRecoveryEvidence checks that a container is running and has not
// restarted recently. A restart count that is still climbing is the clearest
// possible statement that the problem has not gone away.
func containerRecoveryEvidence(podName string, cs *corev1.ContainerStatus, now time.Time) recoveryEvidence {
	if cs.State.Waiting != nil {
		return broken("container %s in pod %s is waiting: %s",
			cs.Name, podName, cs.State.Waiting.Reason)
	}
	if !cs.Ready && cs.State.Terminated == nil {
		return broken("container %s in pod %s is not ready", cs.Name, podName)
	}

	last := cs.LastTerminationState.Terminated
	if last == nil || last.FinishedAt.IsZero() {
		return healthy("container has not restarted")
	}
	if since := now.Sub(last.FinishedAt.Time); since < RecoveryStabilityWindow {
		return broken("container %s in pod %s last restarted %s ago (%s), inside the %s stability window",
			cs.Name, podName, since.Round(time.Second), last.Reason, RecoveryStabilityWindow)
	}

	return healthy("container has been up for the stability window")
}

// workloadPods returns the pods that belong to the incident's workload: the
// ones it named as affected, plus any pod in the namespace the workload claims
// by name. Both halves matter. The named pods may have been replaced by the
// time we look, and the claimed pods are how a fresh, healthy ReplicaSet gets
// seen at all.
func (r *HealthCheckReconciler) workloadPods(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	namespace string,
) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing pods in %s: %w", namespace, err)
	}

	affected := make(map[string]bool)
	for _, ref := range im.Spec.Detection.AffectedResources {
		if ref.Kind == "Pod" && (ref.Namespace == "" || ref.Namespace == namespace) {
			affected[ref.Name] = true
		}
	}

	name := incidentWorkloadName(im)

	var pods []corev1.Pod
	for i := range list.Items {
		pod := list.Items[i]
		if affected[pod.Name] || detection.NameClaimedByPersona(pod.Name, name) {
			pods = append(pods, pod)
		}
	}
	return pods, nil
}

// podReadyCondition returns the pod's Ready condition, or nil when it has none.
func podReadyCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// incidentNamespace returns the namespace whose pods an incident is about.
func incidentNamespace(im *dorguv1.IncidentMemory) string {
	if im.Spec.PersonaRef.Namespace != "" {
		return im.Spec.PersonaRef.Namespace
	}
	return im.Namespace
}

// incidentWorkloadName returns the workload name an incident is filed under.
// For an attributed incident that is the persona name; for an unattributed one
// personaRef carries the workload name instead, which is exactly what we want
// to match pods against either way.
func incidentWorkloadName(im *dorguv1.IncidentMemory) string {
	return im.Spec.PersonaRef.Name
}
