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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

// ============================================================================
// Fixtures
// ============================================================================

// truthScheme carries everything the incident-truth tests read: the CRDs, plus
// the core and apps types the recovery check inspects.
func truthScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	return scheme
}

// brokenApp is one of the clean-room applications: a persona, the Deployment it
// describes, and a pod that is failing in the given way.
type brokenApp struct {
	namespace string
	name      string
	podSuffix string
	crashLoop bool
	oomKilled bool
}

func (a brokenApp) podName() string { return a.name + "-" + a.podSuffix }

func (a brokenApp) objects() []client.Object {
	pod := basePod(a.namespace, a.podName(), a.name)
	cs := &pod.Status.ContainerStatuses[0]

	if a.crashLoop {
		cs.State = corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		}
		cs.Ready = false
		cs.RestartCount = 24
	}
	if a.oomKilled {
		cs.LastTerminationState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     reasonOOMKilled,
				FinishedAt: metav1.NewTime(time.Now().Add(-30 * time.Second)),
			},
		}
	}

	return []client.Object{
		&dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: a.name, Namespace: a.namespace},
			Spec:       dorguv1.ApplicationPersonaSpec{Name: a.name, Type: "api"},
		},
		deploymentFor(a.namespace, a.name, 1),
		pod,
	}
}

// basePod is a running, Ready pod owned by a Deployment's ReplicaSet.
func basePod(namespace, name, deployment string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": deployment},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: deployment + "-7b98cd89d4", APIVersion: "apps/v1"},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: deployment, Image: "example/" + deployment + ":1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  deployment,
				Ready: true,
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
					},
				},
			}},
		},
	}
}

func deploymentFor(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
	}
}

// crashLoopingPod is a pod stuck in CrashLoopBackOff: the exact state that
// stops producing fresh signals while remaining completely dead.
func crashLoopingPod(namespace, name, deployment string) *corev1.Pod {
	pod := basePod(namespace, name, deployment)
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	pod.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name:         deployment,
		Ready:        false,
		RestartCount: 24,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		},
	}
	return pod
}

// openIncident is an active IncidentMemory whose signal stopped arriving long
// enough ago that the old rule would have closed it.
func openIncident(namespace, persona, name string) *dorguv1.IncidentMemory {
	stale := metav1.NewTime(time.Now().Add(-ResolutionGracePeriod - time.Minute))
	return &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelPersonaKind:      "ApplicationPersona",
				LabelPersonaName:      persona,
				LabelPersonaNamespace: namespace,
				LabelCategory:         "health",
				LabelSeverity:         "critical",
				LabelSignal:           string(detection.SignalCrashLoopBackOff),
				LabelPhase:            PhaseDetected,
				LabelAttribution:      AttributionPersona,
			},
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind: "ApplicationPersona", Name: persona, Namespace: namespace,
			},
			Attribution: AttributionPersona,
			Category:    "health",
			Severity:    "critical",
			Detection: dorguv1.DetectionInfo{
				Signal:    string(detection.SignalCrashLoopBackOff),
				Source:    "pod-failure-detector",
				FirstSeen: stale,
				LastSeen:  stale,
			},
		},
		Status: dorguv1.IncidentMemoryStatus{
			Phase:           PhaseDetected,
			OccurrenceCount: 51,
		},
	}
}

// resolverFor builds a reconciler that reads the given objects and detects
// nothing this cycle, which is the state resolveCleared judges.
func resolverFor(t *testing.T, objects ...client.Object) (*HealthCheckReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.ApplicationPersona{}).
		Build()

	logger, _ := newRecordingLogger()
	return &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
	}, c
}

func phaseOf(t *testing.T, c client.Client, im *dorguv1.IncidentMemory) string {
	t.Helper()
	var got dorguv1.IncidentMemory
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
	return got.Status.Phase
}

// ============================================================================
// F-01 — resolution requires positive evidence of recovery
// ============================================================================

// TestResolveCleared_F01_CrashLoopingPodIsNeverResolved reproduces F-01.
//
// platform/checkout reached 51 occurrences and was then marked Resolved while
// still in CrashLoopBackOff, so dorgu health reported one active incident with
// three applications down. The backoff between restarts stretches to five
// minutes, so a dead pod stops producing signals inside the grace period and
// the old two-absences rule read that silence as recovery.
func TestResolveCleared_F01_CrashLoopingPodIsNeverResolved(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crashloopbackoff-186ca74c089c")
	r, c := resolverFor(t,
		im,
		deploymentFor("platform", "checkout", 1),
		crashLoopingPod("platform", "checkout-57c95bf9b8-47vp9", "checkout"),
	)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, PhaseDetected, phaseOf(t, c, im),
		"an incident whose pod is still in CrashLoopBackOff must stay open")
}

// TestResolveCleared_ReadyButNotYetStableStaysOpen covers the second half of the
// rule: readiness alone is not recovery, it has to hold. A pod that has just
// come up may be one restart away from the next crash.
func TestResolveCleared_ReadyButNotYetStableStaysOpen(t *testing.T) {
	pod := basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout")
	pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-30 * time.Second))

	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), pod)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, PhaseDetected, phaseOf(t, c, im),
		"30 seconds of readiness is not %s of stability", RecoveryStabilityWindow)
}

// TestResolveCleared_RecentRestartKeepsIncidentOpen is the restart-count half of
// the rule. The pod is Ready and has been for an hour, but a container was
// killed a minute ago, so the problem is still happening.
func TestResolveCleared_RecentRestartKeepsIncidentOpen(t *testing.T) {
	pod := basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout")
	pod.Status.ContainerStatuses[0].RestartCount = 25
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{
			Reason:     reasonOOMKilled,
			FinishedAt: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
	}

	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), pod)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, PhaseDetected, phaseOf(t, c, im),
		"a container that restarted inside the stability window is not recovered")
}

// TestResolveCleared_SustainedReadinessResolves is the positive case. Without
// it the fix would just be "never resolve anything", which is a different way
// of lying about the cluster.
func TestResolveCleared_SustainedReadinessResolves(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t,
		im,
		deploymentFor("platform", "checkout", 1),
		basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout"),
	)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	var got dorguv1.IncidentMemory
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
	assert.Equal(t, PhaseResolved, got.Status.Phase)
	require.NotNil(t, got.Spec.Resolution)
	assert.Contains(t, got.Spec.Resolution.Action, ResolutionActionPrefix)
	assert.Contains(t, got.Spec.Resolution.Action, "Ready",
		"the resolution must record what was observed, not just that it happened")
}

// TestResolveCleared_OneSickPodBlocksResolution keeps a partial recovery from
// reading as a full one: two replicas, one healthy, one crash-looping.
func TestResolveCleared_OneSickPodBlocksResolution(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t,
		im,
		deploymentFor("platform", "checkout", 2),
		basePod("platform", "checkout-57c95bf9b8-47vp9", "checkout"),
		crashLoopingPod("platform", "checkout-57c95bf9b8-zzzzz", "checkout"),
	)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, PhaseDetected, phaseOf(t, c, im))
}

// TestResolveCleared_MissingPodsWithLiveDeploymentStayOpen: no pods at all for a
// Deployment that wants one is the outage, not the end of it.
func TestResolveCleared_MissingPodsWithLiveDeploymentStayOpen(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1))

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, PhaseDetected, phaseOf(t, c, im))
}

// TestResolveCleared_DeletedWorkloadResolves stops the fix from creating
// incidents that can never close. The app is gone: no pods, no Deployment.
func TestResolveCleared_DeletedWorkloadResolves(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t, im)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	var got dorguv1.IncidentMemory
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
	assert.Equal(t, PhaseResolved, got.Status.Phase)
	require.NotNil(t, got.Spec.Resolution)
	assert.Contains(t, got.Spec.Resolution.Action, "no longer running",
		"a workload that was deleted must not be reported as having recovered")
}

// TestResolveCleared_ScaledToZeroResolves: a Deployment deliberately scaled to
// zero has nothing left to crash.
func TestResolveCleared_ScaledToZeroResolves(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 0))

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, PhaseResolved, phaseOf(t, c, im))
}

// TestResolveCleared_UnreadableClusterKeepsIncidentOpen pins the direction the
// code fails in. If the pods cannot be listed, Dorgu does not know, and "does
// not know" must never round up to "healthy".
func TestResolveCleared_UnreadableClusterKeepsIncidentOpen(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	base := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithObjects(im).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return fmt.Errorf("the API server said no")
			}
			return cl.List(ctx, list, opts...)
		},
	})

	logger, _ := newRecordingLogger()
	r := &HealthCheckReconciler{Client: c, Logger: logger}

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, PhaseDetected, phaseOf(t, base, im))
}

// TestResolveCleared_ActiveSignalIsUntouched keeps the cheap check first: an
// incident whose signal fired this cycle never reaches the recovery check.
func TestResolveCleared_ActiveSignalIsUntouched(t *testing.T) {
	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t, im, basePod("platform", "checkout-1", "checkout"))

	active := map[string]bool{incidentSignalKey(im): true}
	require.NoError(t, r.resolveCleared(context.Background(), active))

	assert.Equal(t, PhaseDetected, phaseOf(t, c, im))
}

// TestResolveCleared_AllPodsTerminatingIsNotRecovery closes the vacuous-truth
// hole. Every pod matching the workload is on its way out, so nothing was
// observed healthy, and a loop that only ever rejects would have fallen through
// to "N pods Ready" having checked none of them.
func TestResolveCleared_AllPodsTerminatingIsNotRecovery(t *testing.T) {
	terminating := crashLoopingPod("platform", "checkout-57c95bf9b8-47vp9", "checkout")
	deleted := metav1.Now()
	terminating.DeletionTimestamp = &deleted
	terminating.Finalizers = []string{"dorgu.io/test"}

	im := openIncident("platform", "checkout", "im-platform-checkout-crash")
	r, c := resolverFor(t, im, deploymentFor("platform", "checkout", 1), terminating)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, PhaseDetected, phaseOf(t, c, im),
		"a workload whose only pod is terminating has not been observed healthy")
}

// TestResolveCleared_UnattributedIncidentClosesWhenAnAttributedOneTakesOver
// stops one broken app being counted twice after it is onboarded mid-outage.
// The handover is allowed only because a replacement incident is already open.
func TestResolveCleared_UnattributedIncidentClosesWhenAnAttributedOneTakesOver(t *testing.T) {
	im := openIncident("web", "edge-nginx", "im-web-edge-nginx-unattributed-crash")
	im.Spec.Attribution = AttributionUnattributed
	im.Labels[LabelAttribution] = AttributionUnattributed

	replacement := openIncident("web", "edge-nginx", "im-web-edge-nginx-crashloopbackoff-abc")

	r, c := resolverFor(t,
		im,
		replacement,
		&dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-nginx", Namespace: "web"},
			Spec:       dorguv1.ApplicationPersonaSpec{Name: "edge-nginx", Type: "web"},
		},
		deploymentFor("web", "edge-nginx", 1),
		crashLoopingPod("web", "edge-nginx-7b98cd89d4-rk8hf", "edge-nginx"),
	)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	var got dorguv1.IncidentMemory
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
	assert.Equal(t, PhaseResolved, got.Status.Phase)
	require.NotNil(t, got.Spec.Resolution)
	assert.Contains(t, got.Spec.Resolution.Action, "superseded",
		"a superseded incident must not claim the workload recovered")

	assert.Equal(t, PhaseDetected, phaseOf(t, c, replacement),
		"the incident that took over must still be open: the app is still down")
}

// TestResolveCleared_UnattributedIncidentStaysOpenWithoutAReplacement is the
// half that matters more. Onboarding a persona mid-outage does not fix
// anything, and if the reconcile cycle that would raise the attributed incident
// has not run yet, closing the unattributed one leaves a crash-looping app with
// no incident at all.
func TestResolveCleared_UnattributedIncidentStaysOpenWithoutAReplacement(t *testing.T) {
	im := openIncident("web", "edge-nginx", "im-web-edge-nginx-unattributed-crash")
	im.Spec.Attribution = AttributionUnattributed
	im.Labels[LabelAttribution] = AttributionUnattributed

	r, c := resolverFor(t,
		im,
		&dorguv1.ApplicationPersona{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-nginx", Namespace: "web"},
			Spec:       dorguv1.ApplicationPersonaSpec{Name: "edge-nginx", Type: "web"},
		},
		deploymentFor("web", "edge-nginx", 1),
		crashLoopingPod("web", "edge-nginx-7b98cd89d4-rk8hf", "edge-nginx"),
	)

	require.NoError(t, r.resolveCleared(context.Background(), map[string]bool{}))

	assert.Equal(t, PhaseDetected, phaseOf(t, c, im),
		"a persona existing is not a handover, and the pod is still crash-looping")
}

// ============================================================================
// F-02 — an incident is about one application
// ============================================================================

// TestDiagnosis_UngroupedSignalsBundleFourApps reproduces the upstream half of
// F-02 against the rules engine directly. Handed every signal in the cluster at
// once, the OOM rule produces one finding that names four pods in three
// namespaces and attributes them all to whichever persona it saw first. This is
// the input that made the planner announce a node-level memory pressure event
// on nodes sitting at 23%.
func TestDiagnosis_UngroupedSignalsBundleFourApps(t *testing.T) {
	logger, _ := newRecordingLogger()
	engine := diagnosis.NewEngine(logger, diagnosis.NewRuleBasedProvider(logger))

	signals := []detection.Signal{
		oomKilledSignal("apps", "report-worker-788c95d9bc-p9pkz", "report-worker"),
		oomKilledSignal("apps", "frontend-podinfo-b4867d9b4-cddt2", "frontend-podinfo"),
		oomKilledSignal("web", "edge-nginx-7b98cd89d4-rk8hf", "edge-nginx"),
		oomKilledSignal("platform", "checkout-57c95bf9b8-47vp9", "checkout"),
	}

	diagnoses, err := engine.Analyze(context.Background(), signals)
	require.NoError(t, err)
	require.Len(t, diagnoses, 1, "the ungrouped rules collapse four apps into one finding")
	assert.Len(t, diagnoses[0].AffectedResources, 4,
		"and hand the planner a bundle spanning three namespaces")

	// Grouping first is what breaks the bundle apart.
	groups := detection.GroupSignals(signals)
	require.Len(t, groups, 4)
	for i := range groups {
		perApp, err := engine.Analyze(context.Background(), groups[i].Signals)
		require.NoError(t, err)
		require.Len(t, perApp, 1)
		assert.Len(t, perApp[0].AffectedResources, 1,
			"a grouped diagnosis names only its own pod")
	}
}

func oomKilledSignal(namespace, pod, persona string) detection.Signal {
	return detection.Signal{
		Type:     detection.SignalOOMKilled,
		Severity: detection.SeverityCritical,
		Category: detection.CategoryResource,
		Source:   "pod-failure-detector",
		Message:  "Container was OOMKilled",
		Resource: dorguv1.ResourceReference{Kind: "Pod", Name: pod, Namespace: namespace},
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: persona, Namespace: namespace,
		},
		DetectedAt: time.Now(),
	}
}

// TestReconcile_Acceptance_FourBrokenAppsAreFourIncidents is the acceptance
// criterion for CF5-1: with N applications broken, Dorgu reports N, not 1.
//
// It runs the real pipeline (pod collector, persona correlator, rule engine,
// reconciler) over the clean-room's own cluster shape: four applications in
// three namespaces, all failing at once. Before grouping, that produced a
// single IncidentMemory listing pods from all four.
func TestReconcile_Acceptance_FourBrokenAppsAreFourIncidents(t *testing.T) {
	apps := []brokenApp{
		{namespace: "apps", name: "report-worker", podSuffix: "788c95d9bc-p9pkz", oomKilled: true},
		{namespace: "apps", name: "frontend-podinfo", podSuffix: "b4867d9b4-cddt2", crashLoop: true, oomKilled: true},
		{namespace: "web", name: "edge-nginx", podSuffix: "7b98cd89d4-rk8hf", crashLoop: true, oomKilled: true},
		{namespace: "platform", name: "checkout", podSuffix: "57c95bf9b8-47vp9", crashLoop: true, oomKilled: true},
	}

	var objects []client.Object
	for _, app := range apps {
		objects = append(objects, app.objects()...)
	}

	c := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.ApplicationPersona{}).
		Build()

	logger, _ := newRecordingLogger()
	detectionEngine := detection.NewEngine(logger, detection.NewPodCollector(c, logger))
	detectionEngine.SetPersonaCorrelator(detection.NewPersonaCorrelator(c, logger))

	r := &HealthCheckReconciler{
		Client:       c,
		Detection:    detectionEngine,
		Diagnosis:    diagnosis.NewEngine(logger, diagnosis.NewRuleBasedProvider(logger)),
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
		Logger:       logger,
	}

	r.reconcile(context.Background())

	var list dorguv1.IncidentMemoryList
	require.NoError(t, c.List(context.Background(), &list))
	require.NotEmpty(t, list.Items)

	// Every broken application is represented.
	covered := map[string]bool{}
	for i := range list.Items {
		im := &list.Items[i]
		if im.Status.Phase == PhaseResolved {
			continue
		}
		covered[im.Spec.PersonaRef.Namespace+"/"+im.Spec.PersonaRef.Name] = true
	}
	for _, app := range apps {
		assert.True(t, covered[app.namespace+"/"+app.name],
			"application %s/%s is broken and has no active incident", app.namespace, app.name)
	}
	assert.Len(t, covered, len(apps),
		"four broken applications must be four applications' worth of incidents, not one bundle")

	// And no incident describes anybody else's pods.
	for i := range list.Items {
		im := &list.Items[i]
		for _, ref := range im.Spec.Detection.AffectedResources {
			assert.Equal(t, im.Spec.PersonaRef.Namespace, ref.Namespace,
				"incident %s names %s/%s from another namespace", im.Name, ref.Namespace, ref.Name)
			assert.True(t, detection.NameClaimedByPersona(ref.Name, im.Spec.PersonaRef.Name),
				"incident %s names pod %s, which belongs to another application", im.Name, ref.Name)
		}
	}
}

// TestReconcile_UnclaimedWorkloadBecomesAnUnattributedIncident is the "prefer
// unattributed over wrong" half of F-02. The broken pod belongs to no persona,
// so it must produce an incident of its own rather than joining the app that
// happens to be failing next to it.
func TestReconcile_UnclaimedWorkloadBecomesAnUnattributedIncident(t *testing.T) {
	claimed := brokenApp{namespace: "apps", name: "report-worker", podSuffix: "788c-p9pkz", oomKilled: true}

	objects := claimed.objects()
	objects = append(objects,
		deploymentFor("apps", "mystery", 1),
		func() *corev1.Pod {
			pod := crashLoopingPod("apps", "mystery-7c9d-abc12", "mystery")
			return pod
		}(),
	)

	c := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.ApplicationPersona{}).
		Build()

	logger, _ := newRecordingLogger()
	detectionEngine := detection.NewEngine(logger, detection.NewPodCollector(c, logger))
	detectionEngine.SetPersonaCorrelator(detection.NewPersonaCorrelator(c, logger))

	r := &HealthCheckReconciler{
		Client:       c,
		Detection:    detectionEngine,
		Diagnosis:    diagnosis.NewEngine(logger, diagnosis.NewRuleBasedProvider(logger)),
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
		Logger:       logger,
	}

	r.reconcile(context.Background())

	var list dorguv1.IncidentMemoryList
	require.NoError(t, c.List(context.Background(), &list))

	var unattributed *dorguv1.IncidentMemory
	for i := range list.Items {
		if list.Items[i].Spec.Attribution == AttributionUnattributed {
			unattributed = &list.Items[i]
		}
		for _, ref := range list.Items[i].Spec.Detection.AffectedResources {
			if list.Items[i].Spec.PersonaRef.Name == "report-worker" {
				assert.NotContains(t, ref.Name, "mystery",
					"an unclaimed pod must never be folded into another app's incident")
			}
		}
	}

	require.NotNil(t, unattributed, "the unclaimed workload must produce an incident of its own")
	assert.Equal(t, "mystery", unattributed.Spec.PersonaRef.Name)
	assert.Equal(t, AttributionUnattributed, unattributed.Labels[LabelAttribution])
	require.NotNil(t, unattributed.Spec.RootCause)
	assert.Contains(t, unattributed.Spec.RootCause.Summary, "No ApplicationPersona",
		"the incident must say why it has no owner")
	assert.Equal(t, PhaseDetected, unattributed.Status.Phase)
}

// TestProcessDiagnosis_UnattributedIncidentProposesNothing keeps Dorgu from
// planning against a persona that does not exist, which would also bill an AI
// call to produce it.
func TestProcessDiagnosis_UnattributedIncidentProposesNothing(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	proposer := &countingProposer{}
	logger, _ := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		Proposer:     proposer,
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
	}

	subject := incidentSubject{
		personaRef:   dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "mystery", Namespace: "apps"},
		unattributed: true,
		namespace:    "apps",
	}
	diag := aiDiagnosis()
	diag.PersonaRef = nil

	require.NoError(t, r.processDiagnosis(context.Background(), subject, diag, map[string]bool{}))
	assert.Zero(t, proposer.calls, "an unattributed incident has no persona to remediate")
}

// ============================================================================
// F-05 — the discarded-diagnosis race is NotFound, not Conflict
// ============================================================================

// notFounder fails the first n Get calls for a named object with NotFound, the
// way a cached read behaves when it has not caught up with a write the API
// server already accepted. n < 0 means "always".
type notFounder struct {
	name  string
	fail  int
	calls int
}

func (nf *notFounder) get() func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
	return func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if key.Name != nf.name {
			return c.Get(ctx, key, obj, opts...)
		}
		nf.calls++
		if nf.fail < 0 || nf.calls <= nf.fail {
			return notFoundFor(key)
		}
		return c.Get(ctx, key, obj, opts...)
	}
}

func notFoundFor(key client.ObjectKey) error {
	return apierrors.NewNotFound(
		schema.GroupResource{Group: dorguv1.GroupVersion.Group, Resource: "incidentmemories"},
		key.Name)
}

// TestCreateIncident_F05_SurvivesTheNotFoundRace reproduces F-05. Five of the
// first six diagnoses in a fresh install were lost to "IncidentMemory not
// found" straight after a successful Create, because reads come from the
// manager's cache and the retry only covered Conflict.
func TestCreateIncident_F05_SurvivesTheNotFoundRace(t *testing.T) {
	base := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	diag := aiDiagnosis()
	subject := personaSubject(*diag.PersonaRef)
	name := generateIncidentName("default", subject.incidentNameKey(), reasonOOMKilled)

	// The cache never catches up during this call. The write still has to land.
	nf := &notFounder{name: name, fail: -1}
	c := interceptor.NewClient(base, interceptor.Funcs{Get: nf.get()})

	logger, sink := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		EventStore:   &capturingEventStore{},
		EventEmitter: &capturingEmitter{},
	}

	require.NoError(t, r.createIncident(context.Background(), subject, diag, metav1.Now()),
		"a cached read that has not caught up must not cost a diagnosis: %v", sink.messages())

	var got dorguv1.IncidentMemory
	require.NoError(t, base.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "default"}, &got))
	assert.Equal(t, PhaseDetected, got.Status.Phase)
	assert.Equal(t, int32(1), got.Status.OccurrenceCount)
	require.NotNil(t, got.Spec.RootCause)
	assert.Equal(t, "ai-anthropic", got.Spec.RootCause.Provider)
}

// TestCreateIncident_RetriesNotFoundOnTheStatusWrite covers the same race one
// layer down: the status write itself reports NotFound because the object is
// not visible yet. retry.RetryOnConflict gave up on the first one.
func TestCreateIncident_RetriesNotFoundOnTheStatusWrite(t *testing.T) {
	base := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	calls := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			calls++
			if calls <= 2 {
				return notFoundFor(client.ObjectKeyFromObject(obj))
			}
			return cl.Status().Update(ctx, obj, opts...)
		},
	})

	logger, sink := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
	}

	diag := aiDiagnosis()
	require.NoError(t,
		r.createIncident(context.Background(), personaSubject(*diag.PersonaRef), diag, metav1.Now()),
		"a NotFound on the status write must be retried: %v", sink.messages())
	assert.GreaterOrEqual(t, calls, 3, "the status write should have been retried")
}

// TestUpdateExistingIncident_RetriesNotFound covers the update path. The
// incident is real; the cached Get is just behind.
func TestUpdateExistingIncident_RetriesNotFound(t *testing.T) {
	im := activeIncident()
	base := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithObjects(im).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	nf := &notFounder{name: im.Name, fail: 2}
	c := interceptor.NewClient(base, interceptor.Funcs{Get: nf.get()})

	logger, sink := newRecordingLogger()
	r := &HealthCheckReconciler{Client: c, Logger: logger}

	diag := aiDiagnosis()
	require.NoError(t,
		r.updateExistingIncident(context.Background(), im, personaSubject(*diag.PersonaRef), diag, metav1.Now()),
		"a transient NotFound must be retried: %v", sink.messages())

	var got dorguv1.IncidentMemory
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(im), &got))
	require.NotNil(t, got.Spec.RootCause)
	assert.Equal(t, diag.Summary, got.Spec.RootCause.Summary)
}

// TestCreateIncident_AdoptsAnIncidentTheCacheMissed covers the mirror image: the
// cached List did not show the incident, so the reconciler tried to create one
// and the API server said it already exists. That is a benign race, and the
// diagnosis in hand still has to be recorded rather than dropped.
func TestCreateIncident_AdoptsAnIncidentTheCacheMissed(t *testing.T) {
	diag := aiDiagnosis()
	subject := personaSubject(*diag.PersonaRef)
	name := generateIncidentName("default", subject.incidentNameKey(), reasonOOMKilled)

	existing := activeIncident()
	existing.Name = name

	base := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithObjects(existing).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	logger, sink := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       base,
		Logger:       logger,
		EventStore:   &capturingEventStore{},
		EventEmitter: &capturingEmitter{},
	}

	require.NoError(t, r.createIncident(context.Background(), subject, diag, metav1.Now()),
		"an already-existing incident must absorb the diagnosis: %v", sink.messages())

	var got dorguv1.IncidentMemory
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(existing), &got))
	require.NotNil(t, got.Spec.RootCause)
	assert.Equal(t, diag.Summary, got.Spec.RootCause.Summary,
		"the diagnosis must survive the AlreadyExists race")
	assert.Equal(t, int32(2), got.Status.OccurrenceCount)
}

// TestCreateIncident_UnrecoverableNotFoundIsStillReportedLoudly keeps CF4-2's
// rule intact: when the retries really are exhausted the diagnosis is lost, and
// a loss the user paid for is never a silent one.
func TestCreateIncident_UnrecoverableNotFoundIsStillReportedLoudly(t *testing.T) {
	base := fake.NewClientBuilder().
		WithScheme(truthScheme(t)).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		Build()

	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
			return notFoundFor(client.ObjectKeyFromObject(obj))
		},
	})

	store := &capturingEventStore{}
	emitter := &capturingEmitter{}
	logger, sink := newRecordingLogger()
	r := &HealthCheckReconciler{
		Client:       c,
		Logger:       logger,
		EventStore:   store,
		EventEmitter: emitter,
	}

	diag := aiDiagnosis()
	err := r.createIncident(context.Background(), personaSubject(*diag.PersonaRef), diag, metav1.Now())
	require.Error(t, err, "a genuinely lost diagnosis must reach the caller")

	assert.True(t, sink.hasError("discard"),
		"the loss must be logged at ERROR, got: %v", sink.messages())
	require.Len(t, store.stored, 1)
	require.Len(t, emitter.emitted, 1)
	assert.Equal(t, ReasonDiagnosisDiscarded, emitter.emitted[0].Reason)
	assert.Contains(t, emitter.emitted[0].Message, "produced and paid for but is not recorded")
}

func TestRetriableIncidentWriteError(t *testing.T) {
	key := client.ObjectKey{Name: "im-1", Namespace: "default"}
	assert.True(t, retriableIncidentWriteError(notFoundFor(key)), "NotFound is the F-05 race")
	assert.True(t, retriableIncidentWriteError(newConflict("im-1")), "Conflict is the CF4-2 race")
	assert.False(t, retriableIncidentWriteError(fmt.Errorf("boom")), "everything else is real")
}

// TestPodRecoveryEvidence covers the per-pod judgements directly, including the
// terminal phases where "Ready" is never going to be true either way.
func TestPodRecoveryEvidence(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name        string
		pod         *corev1.Pod
		wantVerdict recoveryVerdict
		wantReason  string
	}{
		{
			name:        "ready and stable",
			pod:         basePod("apps", "api-1", "api"),
			wantVerdict: verdictHealthy,
		},
		{
			name: "completed run-to-finish pod",
			pod: func() *corev1.Pod {
				p := basePod("apps", "api-1", "api")
				p.Status.Phase = corev1.PodSucceeded
				return p
			}(),
			wantVerdict: verdictUnknown,
		},
		{
			name: "terminating pod is judged by its replacement",
			pod: func() *corev1.Pod {
				p := crashLoopingPod("apps", "api-1", "api")
				deleted := metav1.NewTime(now)
				p.DeletionTimestamp = &deleted
				p.Finalizers = []string{"dorgu.io/test"}
				return p
			}(),
			wantVerdict: verdictUnknown,
		},
		{
			name: "failed pod",
			pod: func() *corev1.Pod {
				p := basePod("apps", "api-1", "api")
				p.Status.Phase = corev1.PodFailed
				p.Status.Reason = "Evicted"
				return p
			}(),
			wantVerdict: verdictBroken,
			wantReason:  "phase Failed",
		},
		{
			name: "no Ready condition at all",
			pod: func() *corev1.Pod {
				p := basePod("apps", "api-1", "api")
				p.Status.Conditions = nil
				return p
			}(),
			wantVerdict: verdictBroken,
			wantReason:  "not Ready",
		},
		{
			name:        "crash-looping container",
			pod:         crashLoopingPod("apps", "api-1", "api"),
			wantVerdict: verdictBroken,
			wantReason:  "not Ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podRecoveryEvidence(tt.pod, now)
			assert.Equal(t, tt.wantVerdict, got.verdict, "reason: %s", got.Reason)
			if tt.wantReason != "" {
				assert.Contains(t, got.Reason, tt.wantReason)
			}
		})
	}
}

// TestContainerRecoveryEvidence_WaitingContainerBlocks catches the pod that is
// Ready overall while one of its containers is stuck backing off.
func TestContainerRecoveryEvidence_WaitingContainerBlocks(t *testing.T) {
	cs := corev1.ContainerStatus{
		Name:  "sidecar",
		Ready: false,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
		},
	}

	got := containerRecoveryEvidence("api-1", &cs, time.Now())

	assert.Equal(t, verdictBroken, got.verdict)
	assert.Contains(t, got.Reason, "ImagePullBackOff")
}
