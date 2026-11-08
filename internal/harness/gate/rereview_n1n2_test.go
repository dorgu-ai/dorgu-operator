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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Re-review N1 / N2: field manager names are chosen by the client, so a stamp is
// exempt from "somebody else wrote the Deployment" only for what it OWNS, never for
// who it says it is; and a generation that moved with nothing to account for it is
// divergence even when no managedFields entry records the write.

// fieldsRevisionAnnotation is what the Deployment controller owns on the main
// resource: the rollout revision annotation, nothing else.
const fieldsRevisionAnnotation = `{"f:metadata":{"f:annotations":{".":{},"f:deployment.kubernetes.io/revision":{}}}}`

// fieldsTicketMemory is what the CLI's strategic-merge patch of the ticket's
// spec.resources.limits.memory owns: the container list entry, its merge key, and
// the one resource leaf.
var fieldsTicketMemory = fmt.Sprintf(`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"%s\"}":{".":{},"f:name":{},"f:resources":{".":{},"f:limits":{".":{},"f:memory":{}}}}}}}}}`, testPersona)

// fieldsContainerMemory is the same path but with no ".": a leaf-only claim.
var fieldsContainerMemory = fmt.Sprintf(`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"%s\"}":{"f:resources":{"f:limits":{"f:memory":{}}}}}}}}}`, testPersona)

const (
	fieldsReplicas = `{"f:spec":{"f:replicas":{}}}`
	fieldsImage    = `{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"web\"}":{".":{},"f:image":{},"f:name":{}}}}}}}`
)

func withFields(entry metav1.ManagedFieldsEntry, raw string) metav1.ManagedFieldsEntry {
	entry.FieldsV1 = &metav1.FieldsV1{Raw: []byte(raw)}
	return entry
}

func stampedBy(manager, fields string) metav1.ManagedFieldsEntry {
	return withFields(managed(manager, fixedNow.Add(time.Second), ""), fields)
}

// --- N1: the exemption is field-scoped --------------------------------------------

func TestRR_SpoofedKCM(t *testing.T) {
	memory := fmt.Sprintf(`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"%s\"}":{"f:resources":{"f:limits":{"f:memory":{}}}}}}}}}`, testPersona)
	cases := map[string]string{
		"owns the container memory limit":      memory,
		"owns the replica count":               fieldsReplicas,
		"owns the image":                       fieldsImage,
		"owns the revision and the replicas":   `{"f:metadata":{"f:annotations":{"f:deployment.kubernetes.io/revision":{}}},"f:spec":{"f:replicas":{}}}`,
		"owns another annotation":              `{"f:metadata":{"f:annotations":{"f:example.com/owner":{}}}}`,
		"owns the revision and a label":        `{"f:metadata":{"f:annotations":{"f:deployment.kubernetes.io/revision":{}},"f:labels":{"f:app":{}}}}`,
		"owns nothing readable":                `not json`,
		"owns the whole metadata node":         `{"f:metadata":{"f:annotations":{"f:deployment.kubernetes.io/revision":{}},"f:finalizers":{"v:\"x\"":{}}}}`,
		"owns an annotation by a longer name":  `{"f:metadata":{"f:annotations":{"f:deployment.kubernetes.io/revision-x":{}}}}`,
		"claims an empty fieldset but no data": ``,
	}
	for name, fields := range cases {
		entry := managed(kubeControllerManager, fixedNow.Add(time.Second), "")
		if fields != "" {
			entry = withFields(entry, fields)
		}
		assert.NotNil(t, divergedBy(t, []metav1.ManagedFieldsEntry{entry}),
			"a kube-controller-manager entry that %s is somebody else writing the Deployment", name)
	}
}

func TestRR_TheRealDeploymentControllerStampIsExempt(t *testing.T) {
	assert.Nil(t, divergedBy(t, []metav1.ManagedFieldsEntry{stampedBy(kubeControllerManager, fieldsRevisionAnnotation)}),
		"the Deployment controller owns exactly the revision annotation on the main resource")
}

func TestRR_ADorguEntryIsExemptOnlyForTheTicketsFields(t *testing.T) {
	assert.Nil(t, divergedBy(t, []metav1.ManagedFieldsEntry{stampedBy(DorguFieldManager, fieldsTicketMemory)}),
		"the CLI's own patch owns the ticket's container resource leaf")
	assert.Nil(t, divergedBy(t, []metav1.ManagedFieldsEntry{stampedBy(DorguFieldManager, fieldsContainerMemory)}))

	for name, fields := range map[string]string{
		"spec.replicas":                          fieldsReplicas,
		"the image":                              fieldsImage,
		"the ticket's field plus the image":      `{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"web\"}":{".":{},"f:name":{},"f:image":{},"f:resources":{".":{},"f:limits":{".":{},"f:memory":{}}}}}}}}}`,
		"the CPU limit the ticket did not patch": fmt.Sprintf(`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"%s\"}":{"f:resources":{"f:limits":{"f:cpu":{}}}}}}}}}`, testPersona),
		"another container's memory":             `{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"sidecar\"}":{"f:resources":{"f:limits":{"f:memory":{}}}}}}}}}`,
		"an annotation":                          fieldsRevisionAnnotation,
		"no fields at all":                       ``,
		"unreadable fields":                      `[`,
	} {
		entry := managed(DorguFieldManager, fixedNow.Add(time.Second), "")
		if fields != "" {
			entry = withFields(entry, fields)
		}
		assert.NotNil(t, divergedBy(t, []metav1.ManagedFieldsEntry{entry}),
			"a dorgu entry that owns %s is not the ticket's patch", name)
	}
}

func TestRR_AStatusSubresourceEntryOwnsNoSpec(t *testing.T) {
	status := withFields(managed(kubeControllerManager, fixedNow.Add(time.Second), "status"), `{"f:status":{"f:replicas":{}}}`)
	assert.Nil(t, divergedBy(t, []metav1.ManagedFieldsEntry{status}))
}

func TestRR_ADorguEntryOlderThanTheTicketNeedsNoFields(t *testing.T) {
	old := withFields(managed(DorguFieldManager, fixedNow.Add(-time.Hour), ""), fieldsReplicas)
	assert.Nil(t, divergedBy(t, []metav1.ManagedFieldsEntry{old}), "a write before the ticket is in the resourceVersion the ticket recorded")
}

// --- N2: the generation bound ------------------------------------------------------

func divergedAt(t *testing.T, generation int64, entries []metav1.ManagedFieldsEntry, mutate ...func(*dorguv1.RemediationAction)) *TicketProblem {
	t.Helper()
	live := deployment("256Mi", func(d *appsv1.Deployment) { d.ResourceVersion = "99"; d.Generation = generation })
	g := newTestGate(t, withManagedFields(t, entries, persona("256Mi"), live))
	ra := ticketedAction(append([]func(*dorguv1.RemediationAction){
		func(ra *dorguv1.RemediationAction) { ra.Status.ApplyTicket.ObservedGeneration = 5 },
	}, mutate...)...)
	problem, err := g.WorkloadDivergence(context.Background(), ra)
	require.NoError(t, err)
	return problem
}

func workloadPatchedByTheCLI(ra *dorguv1.RemediationAction) {
	ra.Status.Conditions = append(ra.Status.Conditions, metav1.Condition{
		Type: ConditionWorkloadPatched, Status: metav1.ConditionTrue, Reason: "CLIAppliedResourceChange",
		LastTransitionTime: metav1.Now(),
	})
}

func TestRR_AGenerationThatMovedWithNoStampIsDiverged(t *testing.T) {
	// A JSON-patch remove leaves no fresh managedFields stamp: the only trace is
	// metadata.generation. The old entry predates the ticket.
	old := managed("kubectl-client-side-apply", fixedNow.Add(-time.Hour), "")
	assert.NotNil(t, divergedAt(t, 6, []metav1.ManagedFieldsEntry{old}),
		"generation 6 against an observed 5, with nothing patched, is an unexplained spec change")
	assert.Nil(t, divergedAt(t, 5, []metav1.ManagedFieldsEntry{old}),
		"an unchanged generation is clean")
}

func TestRR_TheCLIsPatchAccountsForExactlyOneGeneration(t *testing.T) {
	old := managed("kubectl-client-side-apply", fixedNow.Add(-time.Hour), "")
	assert.Nil(t, divergedAt(t, 6, []metav1.ManagedFieldsEntry{old}, workloadPatchedByTheCLI),
		"the recorded patch moves the generation once")
	assert.NotNil(t, divergedAt(t, 7, []metav1.ManagedFieldsEntry{old}, workloadPatchedByTheCLI),
		"a second generation is somebody else's, however the first was explained")
}

func TestRR_ADorguStampOnTheTicketsFieldsAccountsForTheCLIsPatchBeforeItIsRecorded(t *testing.T) {
	// The CLI patches between the operator writing the ticket and the CLI recording
	// WorkloadPatched: the apply-time re-check sees the generation first.
	stamp := stampedBy(DorguFieldManager, fieldsTicketMemory)
	assert.Nil(t, divergedAt(t, 6, []metav1.ManagedFieldsEntry{stamp}))
	assert.NotNil(t, divergedAt(t, 7, []metav1.ManagedFieldsEntry{stamp}))
	spoof := stampedBy(DorguFieldManager, fieldsReplicas)
	assert.NotNil(t, divergedAt(t, 6, []metav1.ManagedFieldsEntry{spoof}), "a dorgu entry on other fields accounts for nothing")
}

func TestRR_ANonTrueWorkloadPatchedAccountsForNothing(t *testing.T) {
	old := managed("kubectl-client-side-apply", fixedNow.Add(-time.Hour), "")
	notPatched := func(ra *dorguv1.RemediationAction) {
		ra.Status.Conditions = append(ra.Status.Conditions, metav1.Condition{
			Type: ConditionWorkloadPatched, Status: metav1.ConditionFalse, Reason: "Declined", LastTransitionTime: metav1.Now(),
		})
	}
	assert.NotNil(t, divergedAt(t, 6, []metav1.ManagedFieldsEntry{old}, notPatched))
}

func TestRR_TheGenerationBoundAlsoHoldsAtApply(t *testing.T) {
	// CheckTicket must refuse to apply a persona patch over a workload whose spec
	// moved with no resourceVersion explanation either.
	live := deployment("256Mi", func(d *appsv1.Deployment) { d.Generation = 9 })
	g := newTestGate(t, fakeClient(t, persona("256Mi"), live))
	problem, err := g.WorkloadDivergence(context.Background(), ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.ObservedGeneration = 5
	}))
	require.NoError(t, err)
	require.NotNil(t, problem)
	assert.Equal(t, GuardrailLiveWorkload, problem.Guardrail)
}

// --- N1: heal credit needs the live resources to be the ticket's -------------------

func healEvidence(t *testing.T, live *appsv1.Deployment) *HealEvidence {
	t.Helper()
	g := newTestGate(t, fakeClient(t, persona("256Mi"), live))
	got, err := g.HealEvidence(context.Background(), ticketedAction())
	require.NoError(t, err)
	return got
}

func TestRR_HealEvidenceReadsGenerationAndResources(t *testing.T) {
	// The ticket's patch is 384Mi; "0.375Gi" is the same quantity.
	got := healEvidence(t, deployment("384Mi", func(d *appsv1.Deployment) { d.Generation = 3 }))
	assert.Equal(t, int64(3), got.Generation)
	assert.True(t, got.ResourcesMatchTicket, got.Mismatch)

	same := healEvidence(t, deployment("0.375Gi"))
	assert.True(t, same.ResourcesMatchTicket, "quantities are compared as quantities")

	other := healEvidence(t, deployment("256Mi"))
	assert.False(t, other.ResourcesMatchTicket)
	assert.Contains(t, other.Mismatch, "spec.resources.limits.memory")
}

func TestRR_HealEvidenceFailsClosed(t *testing.T) {
	noContainer := deployment("384Mi", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "a"}, {Name: "b"}}
	})
	assert.False(t, healEvidence(t, noContainer).ResourcesMatchTicket, "the ticket's container is not there")

	unset := deployment("384Mi", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	})
	assert.False(t, healEvidence(t, unset).ResourcesMatchTicket, "a limit that is gone is not the ticket's value")

	requests := deployment("384Mi", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	})
	g := newTestGate(t, fakeClient(t, persona("256Mi"), requests))
	ra := ticketedAction(func(ra *dorguv1.RemediationAction) {
		ra.Status.ApplyTicket.Patch = jsonOf(`{"spec":{"resources":{"requests":{"cpu":"2"}}}}`)
	})
	got, err := g.HealEvidence(context.Background(), ra)
	require.NoError(t, err)
	assert.False(t, got.ResourcesMatchTicket)
	assert.Contains(t, got.Mismatch, "spec.resources.requests.cpu")

	_, err = (*Gate)(nil).HealEvidence(context.Background(), ticketedAction())
	require.Error(t, err)
	_, err = newTestGate(t, fakeClient(t, persona("256Mi"))).HealEvidence(context.Background(), ticketedAction())
	require.Error(t, err, "no Deployment")
	_, err = newTestGate(t, fakeClient(t, persona("256Mi"), deployment("384Mi", func(d *appsv1.Deployment) { d.UID = "other" }))).
		HealEvidence(context.Background(), ticketedAction())
	require.Error(t, err, "another Deployment's resources say nothing about this heal")
}

func TestRR_ASoleContainerStandsInForTheTicketsContainerName(t *testing.T) {
	renamed := deployment("384Mi", func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Name = "main" })
	assert.True(t, healEvidence(t, renamed).ResourcesMatchTicket)
}

// --- the field-set reader ---------------------------------------------------------

func TestRR_OwnedPathsReadsFieldsV1(t *testing.T) {
	paths, ok := ownedPaths(&metav1.FieldsV1{Raw: []byte(fieldsTicketMemory)})
	require.True(t, ok)
	assert.Contains(t, paths, fieldPath{"f:spec", "f:template", "f:spec", "f:containers", `k:{"name":"web"}`, "f:resources", "f:limits", "f:memory"})
	assert.Contains(t, paths, fieldPath{"f:spec", "f:template", "f:spec", "f:containers", `k:{"name":"web"}`, "f:name"})

	_, ok = ownedPaths(nil)
	assert.False(t, ok)
	_, ok = ownedPaths(&metav1.FieldsV1{})
	assert.False(t, ok)
	_, ok = ownedPaths(&metav1.FieldsV1{Raw: []byte(`{"f:a":"not an object"}`)})
	assert.False(t, ok)
}
