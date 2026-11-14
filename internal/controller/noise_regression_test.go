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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

// Regression tests for B7: the noise the dogfood cluster would otherwise raise.

func noiseScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, dorguv1.AddToScheme(s))
	require.NoError(t, dorguv1alpha1.AddToScheme(s))
	return s
}

func noiseClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(noiseScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}).
		Build()
}

func noiseLogger() logr.Logger { return zap.New(zap.UseDevMode(true)) }

// oomPod is a pod whose container was OOM-killed and is now crash-looping: the
// shape one real OOM leaves behind, which yields an OOMKilled and a
// CrashLoopBackOff signal for the same pod.
func oomPod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Reason:     "OOMKilled",
						FinishedAt: metav1.NewTime(time.Now().Add(-10 * time.Second)),
					},
				},
			}},
		},
	}
}

func excludingClusterPersona(namespaces ...string) *dorguv1.ClusterPersona {
	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "dorgu-cluster"},
		Spec: dorguv1.ClusterPersonaSpec{
			Name: "dorgu-cluster",
			Policies: &dorguv1.ClusterPolicies{
				SelfHealing: &dorguv1.SelfHealingPolicy{ExcludeNamespaces: namespaces},
			},
		},
	}
}

func listIncidents(t *testing.T, c client.Client) []dorguv1.IncidentMemory {
	t.Helper()
	var list dorguv1.IncidentMemoryList
	require.NoError(t, c.List(context.Background(), &list))
	return list.Items
}

// podCollectorFor builds the pod collector the way cmd/main.go does.
func podCollectorFor(c client.Client, g *gate.Gate) *detection.PodCollector {
	return detection.NewPodCollector(c, noiseLogger(), detection.WithNamespaceFilter(g))
}

func newNoiseReconciler(c client.Client, collectors ...detection.SignalCollector) *HealthCheckReconciler {
	log := noiseLogger()
	return &HealthCheckReconciler{
		Client:       c,
		Detection:    detection.NewEngine(log, collectors...),
		Diagnosis:    diagnosis.NewEngine(log, diagnosis.NewRuleBasedProvider(log)),
		EventStore:   &noopEventStore{},
		EventEmitter: &noopEmitter{},
		Logger:       log,
	}
}

// F-13: pod detection honours the same deny-list as the gate. Today it hardcodes
// kube-system, so an OOM in cert-manager, or in any namespace a ClusterPersona
// excludes, raises an incident.
func TestPodDetection_ExcludedNamespacesRaiseNoIncident(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		wantRaise bool
	}{
		{"excluded by ClusterPersona", "cert-manager", false},
		{"excluded, entry has capitals and a space", "argocd", false},
		{"protected: the operator's own namespace", "dorgu-system", false},
		{"protected: an installer-configured namespace", "gateway", false},
		{"protected: kube-system", "kube-system", false},
		{"not excluded", "shop", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := noiseClient(t,
				excludingClusterPersona("cert-manager", " ArgoCD "),
				oomPod(tt.namespace, "worker-abc"),
			)
			g := gate.New(c, noiseLogger(), gate.WithProtectedNamespaces("dorgu-system", "gateway"))

			newNoiseReconciler(c, podCollectorFor(c, g)).reconcile(context.Background())

			incidents := listIncidents(t, c)
			if tt.wantRaise {
				assert.NotEmpty(t, incidents, "an OOM in %q must still raise an incident", tt.namespace)
			} else {
				assert.Empty(t, incidents, "an OOM in the excluded namespace %q must raise no incident", tt.namespace)
			}
		})
	}
}

// F-9: one OOM is one incident. The OOM leaves an OOMKilled and a
// CrashLoopBackOff signal on the same pod, and used to raise an incident for each.
func TestOneOOMRaisesOneIncident(t *testing.T) {
	c := noiseClient(t, oomPod("shop", "worker-abc"))
	g := gate.New(c, noiseLogger())

	newNoiseReconciler(c, podCollectorFor(c, g)).reconcile(context.Background())

	incidents := listIncidents(t, c)
	require.Len(t, incidents, 1, "one OOM must raise one incident, got %s", incidentSignals(incidents))
	assert.Equal(t, "OOMKilled", incidents[0].Spec.Detection.Signal,
		"the incident is the root cause (the OOM), not the crash-loop symptom")
}

// F-9: the RemediationAction names the incident it addresses, and there is only
// one to name.
func TestRemediationActionLinksToTheOneIncident(t *testing.T) {
	const namespace, persona = "shop", "api"
	personaRef := &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: persona, Namespace: namespace}
	pod := dorguv1.ResourceReference{Kind: "Pod", Name: "api-abc", Namespace: namespace}
	now := time.Now()

	c := noiseClient(t, &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: persona, Namespace: namespace},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: persona,
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{Memory: "256Mi", CPU: "500m"},
			},
		},
	})
	log := noiseLogger()
	g := gate.New(c, log)
	r := newNoiseReconciler(c, &stubCollector{
		name: "stub",
		signals: []detection.Signal{
			{
				Type: detection.SignalCrashLoopBackOff, Severity: detection.SeverityCritical,
				Category: detection.CategoryHealth, Resource: pod, PersonaRef: personaRef, DetectedAt: now,
			},
			{
				Type: detection.SignalOOMKilled, Severity: detection.SeverityCritical,
				Category: detection.CategoryHealth, Resource: pod, PersonaRef: personaRef, DetectedAt: now,
			},
		},
	})
	r.Proposer = remediation.NewProposer(c, g, log)

	r.reconcile(context.Background())

	incidents := listIncidents(t, c)
	require.Len(t, incidents, 1, "got %s", incidentSignals(incidents))

	var actions dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &actions))
	require.Len(t, actions.Items, 1, "one OOM, one remediation")
	assert.Equal(t, incidents[0].Name, actions.Items[0].Spec.IncidentRef.Name,
		"the remediation must name the incident it addresses")
}

func incidentSignals(incidents []dorguv1.IncidentMemory) []string {
	out := make([]string, 0, len(incidents))
	for i := range incidents {
		out = append(out, incidents[i].Spec.Category+"/"+incidents[i].Spec.Detection.Signal)
	}
	return out
}
