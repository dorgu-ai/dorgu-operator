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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func liveRef() *dorguv1.WorkloadRef {
	return &dorguv1.WorkloadRef{
		Kind:            "Deployment",
		Name:            "frontend-podinfo",
		Namespace:       "apps",
		Container:       "podinfo",
		ManagedBy:       dorguv1.ManagedByHelm,
		ManagedByDetail: `Helm release "frontend" in namespace apps`,
		ObservedImage:   "ghcr.io/stefanprodan/podinfo:6.99.0-does-not-exist",
		ObservedResources: &dorguv1.ObservedResources{
			Limits:   &dorguv1.ResourceValues{Memory: "32Mi"},
			Requests: &dorguv1.ResourceValues{CPU: "25m", Memory: "16Mi"},
		},
	}
}

// TestWriteLiveWorkload_StatesLiveNumbersAndAbsences is the grounding contract
// as the model sees it: the running values, and explicitly which keys are not
// set so a fix cannot introduce one by accident.
func TestWriteLiveWorkload_StatesLiveNumbersAndAbsences(t *testing.T) {
	var sb strings.Builder
	writeLiveWorkload(&sb, &WorkloadContext{
		Ref:           liveRef(),
		Replicas:      2,
		ReadyReplicas: 0,
		PriorImages:   []string{"ghcr.io/stefanprodan/podinfo:6.14.1"},
	})
	msg := sb.String()

	assert.Contains(t, msg, "## Live workload (ground truth")
	assert.Contains(t, msg, "Deployment: apps/frontend-podinfo | container: podinfo")
	assert.Contains(t, msg, "Image (running now): ghcr.io/stefanprodan/podinfo:6.99.0-does-not-exist")
	assert.Contains(t, msg, "Replicas: desired=2 ready=0")
	assert.Contains(t, msg, "Live limits: cpu=NOT SET (do not introduce) memory=32Mi")
	assert.Contains(t, msg, "Live requests: cpu=25m memory=16Mi")
	assert.Contains(t, msg, "ghcr.io/stefanprodan/podinfo:6.14.1")
	assert.Contains(t, msg, `managedBy: helm (Helm release "frontend" in namespace apps)`)
	assert.Contains(t, msg, "This workload is OWNED")
}

func TestWriteLiveWorkload_UnmanagedInvitesADirectCommand(t *testing.T) {
	ref := liveRef()
	ref.ManagedBy = dorguv1.ManagedByUnmanaged
	ref.ManagedByDetail = ""

	var sb strings.Builder
	writeLiveWorkload(&sb, &WorkloadContext{Ref: ref})
	msg := sb.String()

	assert.Contains(t, msg, "managedBy: unmanaged")
	assert.Contains(t, msg, "a direct kubectl command is the right")
	assert.NotContains(t, msg, "This workload is OWNED")
}

// TestWriteLiveWorkload_UnreadableSaysSo keeps the operator honest when it
// cannot resolve a Deployment: it must not quietly let persona numbers stand in.
func TestWriteLiveWorkload_UnreadableSaysSo(t *testing.T) {
	var sb strings.Builder
	writeLiveWorkload(&sb, nil)
	msg := sb.String()

	assert.Contains(t, msg, "no Deployment could be resolved")
	assert.Contains(t, msg, "you may not state any current")
}

func TestWriteLiveWorkload_NoResourcesAtAll(t *testing.T) {
	ref := liveRef()
	ref.ObservedResources = nil

	var sb strings.Builder
	writeLiveWorkload(&sb, &WorkloadContext{Ref: ref})

	assert.Contains(t, sb.String(), "the container sets NO requests and NO limits")
}

// TestBuildPlanUserMessage_LabelsThePersonaAsStale is F-03/F-04: the persona's
// numbers must never be presented to the model as the current state.
func TestBuildPlanUserMessage_LabelsThePersonaAsStale(t *testing.T) {
	rc := RemediationContext{
		Diagnosis:  oomDiagnosis("my-app"),
		AppPersona: appPersona(),
		Workload:   &WorkloadContext{Ref: liveRef()},
	}

	msg := buildPlanUserMessage(rc)

	assert.Contains(t, msg, "## Application persona (imported snapshot of INTENT, may be stale)")
	assert.Contains(t, msg, "Persona-recorded limits (NOT current)")
	assert.NotContains(t, msg, "Current limits:")
	assert.Less(t, strings.Index(msg, "## Live workload"), strings.Index(msg, "## Application persona"),
		"the ground truth is presented before the snapshot it supersedes")
}

func TestPlanSystemPrompt_CarriesTheGroundingAndOwnershipRules(t *testing.T) {
	for _, want := range []string{
		"GROUNDING",
		"only source of truth for what is running",
		"twice what the live workload has",
		"Only change resource keys the live workload ALREADY sets",
		"Never assert a version, tag or release you have not read",
		"OWNERSHIP",
		"NEVER suggest a command that writes to it",
		"managedBy \"unknown\" is treated as owned",
		"persona-update steps are unaffected",
	} {
		assert.Contains(t, planSystemPrompt, want)
	}
}

func TestNewWorkloadContext(t *testing.T) {
	replicas := int32(3)
	deploy := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{AnnotationImportedImage: "ghcr.io/stefanprodan/podinfo:6.14.1"},
		},
		Status: dorguv1.ApplicationPersonaStatus{
			Deployments: &dorguv1.DeploymentTracking{
				Current:        "ghcr.io/stefanprodan/podinfo:6.99.0-does-not-exist",
				LastSuccessful: "ghcr.io/stefanprodan/podinfo:6.14.1",
			},
		},
	}

	wc := NewWorkloadContext(liveRef(), deploy, persona)
	require.NotNil(t, wc)
	assert.Equal(t, int32(3), wc.Replicas)
	assert.Equal(t, int32(1), wc.ReadyReplicas)
	assert.Equal(t, []string{"ghcr.io/stefanprodan/podinfo:6.14.1"}, wc.PriorImages,
		"the running tag is excluded and duplicates collapse, leaving the verified prior tag")
}

func TestNewWorkloadContext_UnresolvedRefYieldsNoContext(t *testing.T) {
	assert.Nil(t, NewWorkloadContext(nil, nil, nil))
	assert.Nil(t, NewWorkloadContext(&dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByUnknown}, nil, nil),
		"a placeholder record is not a workload we can quote")
}
