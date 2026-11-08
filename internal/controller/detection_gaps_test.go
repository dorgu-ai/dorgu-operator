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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// B9 regression coverage that spans detection, diagnosis, the incident store and
// the model: F-13 for the signal types B9 adds, and the stale terminated state.

// attributingCollector wraps the real pod collector and files every signal
// against the persona "api" in its namespace, as the persona correlator would.
// Attributed groups are the ones that may reach the model, which is what these
// tests count.
type attributingCollector struct{ inner detection.SignalCollector }

func (a attributingCollector) Name() string { return a.inner.Name() }
func (a attributingCollector) Collect(ctx context.Context) ([]detection.Signal, error) {
	signals, err := a.inner.Collect(ctx)
	for i := range signals {
		signals[i].PersonaRef = &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: "api", Namespace: signals[i].Resource.Namespace,
		}
	}
	return signals, err
}

// aiReconciler is the production wiring: real pod collector behind the gate's
// deny list, rule and AI diagnosis providers over a counting model, the real
// proposer.
func aiReconciler(t *testing.T, c client.Client, g *gate.Gate, stub *stubLLMClient) *HealthCheckReconciler {
	t.Helper()
	log := noiseLogger()
	pods := detection.NewPodCollector(c, log, detection.WithNamespaceFilter(g))
	r := newNoiseReconciler(c, attributingCollector{inner: pods})
	r.Diagnosis = diagnosis.NewEngine(log,
		diagnosis.NewRuleBasedProvider(log),
		diagnosis.NewAIProvider(stub, g, log),
	)
	r.Proposer = remediation.NewProposer(c, g, log)
	return r
}

func probePersona(ns string) *dorguv1.ApplicationPersona {
	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: ns},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: "api",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{Memory: "256Mi", CPU: "500m"},
			},
		},
	}
}

// failingPods are the fault shapes the operator detects, one pod each.
func failingPods(ns string) map[string]*corev1.Pod {
	now := time.Now()
	mk := func(name string, cs corev1.ContainerStatus, conds ...corev1.PodCondition) *corev1.Pod {
		cs.Name = "main"
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}, Conditions: conds},
		}
	}
	return map[string]*corev1.Pod{
		"CrashLoopBackOff": mk("api-crash", corev1.ContainerStatus{
			RestartCount: 8,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Error", ExitCode: 1, FinishedAt: metav1.NewTime(now.Add(-30 * time.Second)),
			}},
		}),
		"OOMKilled crash loop": oomPod(ns, "api-oom"),
		"OOMKilled just restarted": mk("api-oomlive", corev1.ContainerStatus{
			RestartCount: 1,
			State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-10 * time.Second))}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-15 * time.Second)),
			}},
		}),
		"LivenessRestarts": mk("api-liveness", corev1.ContainerStatus{
			Ready: true, RestartCount: 6,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-10 * time.Second))}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Error", ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-30 * time.Second)),
			}},
		}),
		"ReadinessNotReady": mk("api-notready", corev1.ContainerStatus{
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-10 * time.Minute))}},
		}, corev1.PodCondition{
			Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-5 * time.Minute)),
		}),
	}
}

// F-13, for every signal type a pod can raise: a pod that is really failing in a
// deny-listed or excluded namespace raises no incident, no proposal and makes no
// model call. B7 fixed F-13 for OOMKilled; D0.8 could not prove it (a clean pod
// delete raises no signal), so this pins it with pods that genuinely fail, and
// for the signals B9 adds. The same pods in an ordinary namespace do raise one,
// so the assertion cannot pass by detecting nothing.
func TestF13_FailingPodsInExcludedNamespacesRaiseNothingAndCallNoModel(t *testing.T) {
	namespaces := []struct {
		name      string
		namespace string
		wantRaise bool
	}{
		{"excluded by the ClusterPersona", "cert-manager", false},
		{"the operator's own namespace", "dorgu-system", false},
		{"installer-protected", "gateway", false},
		{"kube-system", "kube-system", false},
		{"control: an ordinary namespace", "shop", true},
	}
	for _, ns := range namespaces {
		for fault := range failingPods(ns.namespace) {
			t.Run(ns.name+"/"+fault, func(t *testing.T) {
				pod := failingPods(ns.namespace)[fault]
				c := noiseClient(t, excludingClusterPersona("cert-manager"), probePersona(ns.namespace), pod)
				g := gate.New(c, noiseLogger(), gate.WithProtectedNamespaces("dorgu-system", "gateway"))
				stub := &stubLLMClient{}
				r := aiReconciler(t, c, g, stub)

				r.reconcile(context.Background())
				r.reconcile(context.Background())

				incidents := listIncidents(t, c)
				var actions dorguv1.RemediationActionList
				require.NoError(t, c.List(context.Background(), &actions))
				if ns.wantRaise {
					assert.NotEmpty(t, incidents, "%s in %s must raise an incident", fault, ns.namespace)
					assert.Positive(t, stub.calls, "the control must reach the model, or the zero below proves nothing")
					return
				}
				assert.Empty(t, incidents, "%s in %s must raise no incident, got %s", fault, ns.namespace, incidentSignals(incidents))
				assert.Empty(t, actions.Items, "no proposal for an excluded namespace")
				assert.Zero(t, stub.calls, "no model call for an excluded namespace")
			})
		}
	}
}

// D0.8 finding 3: the container ran fine after its OOM kill, but the pod kept
// lastState.terminated=OOMKilled until it was replaced, so the signal, the
// incident and a model call came back every cycle (~1,440 a day).
func TestStaleOOMKilled_StopsRaisingSignalsAndModelCalls(t *testing.T) {
	now := time.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "shop"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now.Add(-15 * time.Minute))},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", Ready: true, RestartCount: 1,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-15 * time.Minute))}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-16 * time.Minute)),
				}},
			}},
		},
	}
	c := noiseClient(t, probePersona("shop"), pod)
	g := gate.New(c, noiseLogger())
	stub := &stubLLMClient{}
	r := aiReconciler(t, c, g, stub)

	for range 3 {
		r.reconcile(context.Background())
	}

	assert.Empty(t, listIncidents(t, c), "a container Ready for 15 minutes since its OOM kill is not an incident")
	assert.Zero(t, stub.calls, "and costs no model calls")
}

// The same pod, seen the cycle after the kill, is still detected: staleness must
// not erase a kill nobody has looked at yet.
func TestStaleOOMKilled_JustRestartedIsStillDetectedThenGoesQuiet(t *testing.T) {
	now := time.Now()
	status := func(startedAgo, killedAgo time.Duration) corev1.PodStatus {
		return corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now.Add(-startedAgo))},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", Ready: true, RestartCount: 1,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-startedAgo))}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-killedAgo)),
				}},
			}},
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "shop"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		Status:     status(20*time.Second, 25*time.Second),
	}
	c := noiseClient(t, probePersona("shop"), pod)
	g := gate.New(c, noiseLogger())
	stub := &stubLLMClient{}
	r := aiReconciler(t, c, g, stub)

	r.reconcile(context.Background())
	require.Len(t, listIncidents(t, c), 1, "the fresh kill is detected")
	callsAfterDetection := stub.calls
	require.Positive(t, callsAfterDetection)

	// Ten minutes on, the same pod status as the API server would report it.
	var cur corev1.Pod
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pod), &cur))
	cur.Status = status(10*time.Minute, 10*time.Minute+5*time.Second)
	require.NoError(t, c.Status().Update(context.Background(), &cur))

	for range 3 {
		r.reconcile(context.Background())
	}
	assert.Equal(t, callsAfterDetection, stub.calls, "a stale terminated state must not keep the model calls coming")
	im := listIncidents(t, c)[0]
	assert.EqualValues(t, 1, im.Status.OccurrenceCount, "nor keep refreshing the incident's occurrence count")

	// With the signal gone the incident is free to resolve once it is stable.
	im.Spec.Detection.LastSeen = metav1.NewTime(now.Add(-10 * time.Minute))
	require.NoError(t, c.Update(context.Background(), &im))
	r.reconcile(context.Background())
	got := listIncidents(t, c)[0]
	assert.Equal(t, PhaseResolved, got.Status.Phase, "the incident must not stay open on a stale signal")
}
