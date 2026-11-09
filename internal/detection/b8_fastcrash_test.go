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

package detection

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// B8 EKS QA F-4: a container that exits 1 five seconds after it starts was
// detected after about five minutes. It is Running for five seconds in every
// back-off cycle and sits in state Terminated (reason Error) the rest of the
// time, so it was never "Running" when the collector looked, and the restart-loop
// signal (which needed a Running container) never fired; CrashLoopBackOff only
// showed up once the kubelet's back-off had grown long enough to report it.

// terminatedBetweenRuns is a fast crasher as the kubelet reports it between runs:
// the current state is the run that just ended, the previous one is the last state.
func terminatedBetweenRuns(c *fakeClock, restarts int32) corev1.ContainerStatus {
	exit := corev1.ContainerStateTerminated{
		Reason: "Error", ExitCode: 1, StartedAt: ago(c, 10*time.Second), FinishedAt: ago(c, 5*time.Second),
	}
	return corev1.ContainerStatus{
		Ready:                false,
		RestartCount:         restarts,
		State:                corev1.ContainerState{Terminated: &exit},
		LastTerminationState: corev1.ContainerState{Terminated: &exit},
	}
}

func TestFastCrasher_RestartLoopFiresWhileTheContainerIsBetweenRuns(t *testing.T) {
	c := newClock()
	cycles := livenessCycles(t, c, nil,
		terminatedBetweenRuns(c, 1), // first sight only seeds the history
		terminatedBetweenRuns(c, 3), // two more restarts in the window
	)

	assert.False(t, hasLoop(cycles[0]), "first sight only seeds the history")
	require.True(t, hasType(cycles[1], SignalRestartLoop), "a fast crasher between runs is a restart loop, got %v", signalTypes(cycles[1]))
	assert.False(t, hasType(cycles[1], SignalCrashLoopBackOff), "the kubelet has not reported CrashLoopBackOff, so none is raised")
}

func TestFastCrasher_CrashLoopBackOffStillOwnsTheContainerOnceItFires(t *testing.T) {
	c := newClock()
	waiting := terminatedBetweenRuns(c, 5)
	waiting.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}

	cycles := livenessCycles(t, c, nil, terminatedBetweenRuns(c, 3), waiting)

	assert.True(t, hasType(cycles[1], SignalCrashLoopBackOff))
	assert.False(t, hasLoop(cycles[1]), "CrashLoopBackOff fired for this container this cycle, so it owns it")
}

func TestFastCrasher_OneShapePerCycleFromRestartLoopToCrashLoopBackOff(t *testing.T) {
	c := newClock()
	waiting := terminatedBetweenRuns(c, 5)
	waiting.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}

	cycles := livenessCycles(t, c, nil, terminatedBetweenRuns(c, 1), terminatedBetweenRuns(c, 3), waiting)

	assert.Equal(t, []SignalType{SignalRestartLoop}, signalTypes(cycles[1]))
	assert.Equal(t, []SignalType{SignalCrashLoopBackOff}, signalTypes(cycles[2]))
}

func TestFastCrasher_ACleanExitIsNotALoop(t *testing.T) {
	c := newClock()
	done := corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0, StartedAt: ago(c, 10*time.Second), FinishedAt: ago(c, 5*time.Second)}
	status := func(n int32) corev1.ContainerStatus {
		return corev1.ContainerStatus{RestartCount: n, State: corev1.ContainerState{Terminated: &done}, LastTerminationState: corev1.ContainerState{Terminated: &done}}
	}

	cycles := livenessCycles(t, c, nil, status(1), status(3))

	assert.False(t, hasLoop(cycles[1]), "a Completed exit with no kubelet liveness kills is not a crash")
}

func TestFastCrasher_AnOOMKillBetweenRunsStaysTheResourceSignals(t *testing.T) {
	c := newClock()
	oom := corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, StartedAt: ago(c, 10*time.Second), FinishedAt: ago(c, 5*time.Second)}
	status := func(n int32) corev1.ContainerStatus {
		return corev1.ContainerStatus{RestartCount: n, State: corev1.ContainerState{Terminated: &oom}, LastTerminationState: corev1.ContainerState{Terminated: &oom}}
	}

	cycles := livenessCycles(t, c, nil, status(1), status(3))

	assert.True(t, hasType(cycles[1], SignalOOMKilled))
	assert.False(t, hasLoop(cycles[1]))
}

// B8 EKS QA F-1: a pod whose image cannot be pulled stays Pending, so it also
// crossed the long-pending threshold five minutes in and opened a second incident
// (and a second proposal) for the one fault.

func pendingPod(c *fakeClock, age time.Duration, statuses ...corev1.ContainerStatus) corev1.Pod {
	pod := runningPod("web-1", corev1.ContainerStatus{})
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = statuses
	pod.CreationTimestamp = ago(c, age)
	return pod
}

func waitingFor(reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "nope"}}}
}

func TestPendingLong_IsNotRaisedWhileTheContainerHasAnImageError(t *testing.T) {
	for _, reason := range []string{"ImagePullBackOff", "ErrImagePull", "InvalidImageName"} {
		t.Run(reason, func(t *testing.T) {
			c := newClock()
			sigs := collectWith(t, c, []corev1.Pod{pendingPod(c, 20*time.Minute, waitingFor(reason))}, nil)

			assert.Equal(t, []SignalType{SignalImagePullBackOff}, signalTypes(sigs),
				"one pod, one fault: the image error owns it, got %v", signalTypes(sigs))
		})
	}
}

func TestPendingLong_AnInitContainerImageErrorOwnsThePodToo(t *testing.T) {
	c := newClock()
	pod := pendingPod(c, 20*time.Minute)
	init := waitingFor("ImagePullBackOff")
	init.Name = "init"
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{init}

	sigs := collectWith(t, c, []corev1.Pod{pod}, nil)

	assert.False(t, hasType(sigs, SignalPodPendingLong), "got %v", signalTypes(sigs))
	assert.True(t, hasType(sigs, SignalImagePullBackOff))
}

func TestPendingLong_StillRaisedForAPodThatIsPendingForAnotherReason(t *testing.T) {
	c := newClock()
	for name, statuses := range map[string][]corev1.ContainerStatus{
		"unscheduled": nil,
		"creating":    {waitingFor("ContainerCreating")},
	} {
		t.Run(name, func(t *testing.T) {
			sigs := collectWith(t, c, []corev1.Pod{pendingPod(c, 20*time.Minute, statuses...)}, nil)
			assert.Equal(t, []SignalType{SignalPodPendingLong}, signalTypes(sigs))
		})
	}
}

// The kubelet's back-off spaces a fast crasher's restarts further apart every time,
// so asking for two restarts to be OBSERVED after the first sighting took about two
// minutes (measured on EKS: 117 s). For a pod younger than the window every restart
// it has had happened inside it, so its restart count is the delta, and the first
// sighting is already evidence.

func youngCrasher(c *fakeClock, age time.Duration, restarts int32) corev1.Pod {
	pod := runningPod("api-1", terminatedBetweenRuns(c, restarts))
	pod.CreationTimestamp = ago(c, age)
	return pod
}

func TestFastCrasher_AYoungPodsRestartCountIsTheDeltaAtFirstSight(t *testing.T) {
	c := newClock()
	sigs := collectWith(t, c, []corev1.Pod{youngCrasher(c, 45*time.Second, 2)}, nil)

	require.True(t, hasType(sigs, SignalRestartLoop), "two restarts in the pod's 45 seconds of life, got %v", signalTypes(sigs))
}

func TestFastCrasher_OneRestartOfAYoungPodIsNotALoop(t *testing.T) {
	c := newClock()
	sigs := collectWith(t, c, []corev1.Pod{youngCrasher(c, 20*time.Second, 1)}, nil)

	assert.False(t, hasLoop(sigs))
}

func TestFastCrasher_AnOldPodsLifetimeCountIsNotEvidenceAtFirstSight(t *testing.T) {
	c := newClock()
	sigs := collectWith(t, c, []corev1.Pod{youngCrasher(c, 3*time.Hour, 5)}, nil)

	assert.False(t, hasLoop(sigs), "first sight of an old pod only seeds the history")
}

func TestFastCrasher_AYoungPodWithNoCreationTimeIsNotAssumedYoung(t *testing.T) {
	c := newClock()
	pod := youngCrasher(c, 0, 5)
	pod.CreationTimestamp = metav1.Time{}

	assert.False(t, hasLoop(collectWith(t, c, []corev1.Pod{pod}, nil)))
}
