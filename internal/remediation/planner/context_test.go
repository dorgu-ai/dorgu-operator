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

package planner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(s))
	return s
}

func appPersona(namespace, name string) *dorguv1.ApplicationPersona {
	return &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: name,
			Type: "api",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{Memory: "256Mi", CPU: "500m"},
			},
		},
		Status: dorguv1.ApplicationPersonaStatus{
			Learned: &dorguv1.LearnedPatterns{
				ResourceBaseline: &dorguv1.ResourceBaseline{
					AvgMemory: "180Mi", PeakMemory: "240Mi",
				},
			},
		},
	}
}

func clusterPersona() *dorguv1.ClusterPersona {
	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "dorgu-cluster"},
		Spec: dorguv1.ClusterPersonaSpec{
			Name:        "dorgu-cluster",
			Environment: "production",
			Policies: &dorguv1.ClusterPolicies{
				SelfHealing: &dorguv1.SelfHealingPolicy{
					Enabled: true, Mode: "propose", TrustLevel: 2, MaxRemediationsPerHour: 5,
				},
			},
		},
	}
}

func incident(namespace, name, persona, signal string, occurred time.Time, remediationRef string) *dorguv1.IncidentMemory {
	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelPersonaName: persona},
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: persona, Namespace: namespace},
			Category:   "resource",
			Severity:   "critical",
			Detection:  dorguv1.DetectionInfo{Signal: signal},
		},
		Status: dorguv1.IncidentMemoryStatus{
			Phase:          "Resolved",
			LastOccurrence: &metav1.Time{Time: occurred},
		},
	}
	if remediationRef != "" {
		im.Spec.Resolution = &dorguv1.ResolutionInfo{
			Action:         "resource-adjustment",
			RemediationRef: &dorguv1.RemediationReference{Name: remediationRef, Namespace: namespace},
		}
	}
	return im
}

func remediation(namespace, name, persona, phase, verification string) *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelPersonaName: persona},
		},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef:  dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: persona, Namespace: namespace},
			Action:      dorguv1.RemediationActionDetail{Type: "persona-update"},
			Explanation: "increase memory",
			Confidence:  "0.85",
		},
		Status: dorguv1.RemediationActionStatus{
			Phase:              phase,
			VerificationResult: verification,
		},
	}
}

func oomDiagnosis(namespace, persona string) diagnosis.Diagnosis {
	return diagnosis.Diagnosis{
		Summary:  "OOMKilled detected",
		Category: "resource",
		Severity: detection.SeverityCritical,
		PersonaRef: &dorguv1.PersonaReference{
			Kind: "ApplicationPersona", Name: persona, Namespace: namespace,
		},
		Contributing: []diagnosis.ContributingSignal{
			{Signal: detection.Signal{Type: detection.SignalOOMKilled, Severity: detection.SeverityCritical, Message: "container OOMKilled"}},
		},
		SuggestedAction: "resource-adjustment",
	}
}

func TestBuildContext_AssemblesAllContext(t *testing.T) {
	scheme := newScheme(t)
	now := time.Now()

	objs := []runtime.Object{
		clusterPersona(),
		appPersona("default", "my-app"),
		// 3 incidents for my-app at increasing recency.
		incident("default", "im-old", "my-app", "OOMKilled", now.Add(-3*time.Hour), "ra-completed"),
		incident("default", "im-mid", "my-app", "OOMKilled", now.Add(-2*time.Hour), ""),
		incident("default", "im-new", "my-app", "CrashLoopBackOff", now.Add(-1*time.Hour), "ra-rolledback"),
		// An unrelated incident for another app — must be filtered out.
		incident("default", "im-other", "other-app", "OOMKilled", now, ""),
		// 2 past remediations with outcomes.
		remediation("default", "ra-completed", "my-app", "Completed", "Healthy"),
		remediation("default", "ra-rolledback", "my-app", "RolledBack", "Degraded"),
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

	diag := oomDiagnosis("default", "my-app")
	rc, err := BuildContext(context.Background(), c, diag, incident("default", "im-trigger", "my-app", "OOMKilled", now, ""))
	require.NoError(t, err)
	require.NotNil(t, rc)

	// App + cluster persona loaded.
	require.NotNil(t, rc.AppPersona)
	assert.Equal(t, "my-app", rc.AppPersona.Spec.Name)
	require.NotNil(t, rc.AppPersona.Status.Learned)
	require.NotNil(t, rc.ClusterPersona)
	assert.Equal(t, "production", rc.ClusterPersona.Spec.Environment)

	// Past incidents: 3 for my-app, label-filtered (other-app excluded),
	// sorted most-recent first.
	require.Len(t, rc.PastIncidents, 3)
	assert.Equal(t, "im-new", rc.PastIncidents[0].Name)
	assert.Equal(t, "im-mid", rc.PastIncidents[1].Name)
	assert.Equal(t, "im-old", rc.PastIncidents[2].Name)
	for _, im := range rc.PastIncidents {
		assert.Equal(t, "my-app", im.Spec.PersonaRef.Name)
	}

	// Past remediations carry their outcome/status.
	require.Len(t, rc.PastRemediations, 2)
	outcomes := map[string]string{}
	for _, ra := range rc.PastRemediations {
		outcomes[ra.Name] = ra.Status.Phase + "/" + ra.Status.VerificationResult
	}
	assert.Equal(t, "Completed/Healthy", outcomes["ra-completed"])
	assert.Equal(t, "RolledBack/Degraded", outcomes["ra-rolledback"])

	// Signals flattened from diagnosis.
	require.Len(t, rc.Signals, 1)
	assert.Equal(t, detection.SignalOOMKilled, rc.Signals[0].Type)
}

func TestBuildContext_CapsPastIncidents(t *testing.T) {
	scheme := newScheme(t)
	now := time.Now()

	objs := []runtime.Object{appPersona("default", "my-app")}
	for i := 0; i < MaxPastIncidents+5; i++ {
		objs = append(objs, incident("default", fmt.Sprintf("im-%02d", i), "my-app", "OOMKilled", now.Add(-time.Duration(i)*time.Minute), ""))
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

	rc, err := BuildContext(context.Background(), c, oomDiagnosis("default", "my-app"),
		incident("default", "trigger", "my-app", "OOMKilled", now, ""))
	require.NoError(t, err)
	assert.Len(t, rc.PastIncidents, MaxPastIncidents)
	// Most recent (im-00) must be first after the recency sort.
	assert.Equal(t, "im-00", rc.PastIncidents[0].Name)
}

func TestBuildContext_MissingPersonaIsError(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := BuildContext(context.Background(), c, oomDiagnosis("default", "ghost"),
		incident("default", "trigger", "ghost", "OOMKilled", time.Now(), ""))
	require.Error(t, err)
}

func TestBuildContext_NoClusterPersonaIsTolerated(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(appPersona("default", "my-app")).Build()

	rc, err := BuildContext(context.Background(), c, oomDiagnosis("default", "my-app"),
		incident("default", "trigger", "my-app", "OOMKilled", time.Now(), ""))
	require.NoError(t, err)
	assert.Nil(t, rc.ClusterPersona)
	assert.NotNil(t, rc.AppPersona)
}
