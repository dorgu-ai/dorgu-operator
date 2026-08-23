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

package workload

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func deployWith(labels, annotations map[string]string, managed ...metav1.ManagedFieldsEntry) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "frontend-podinfo",
			Namespace:     "apps",
			Labels:        labels,
			Annotations:   annotations,
			ManagedFields: managed,
		},
	}
}

func applyBy(manager string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationApply}
}

func updateBy(manager string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate}
}

// withFields attaches a fieldsV1 set to an entry. The JSON is the real shape
// the API server records, copied from a live Deployment.
func withFields(e metav1.ManagedFieldsEntry, fieldsV1 string) metav1.ManagedFieldsEntry {
	e.FieldsType = "FieldsV1"
	e.FieldsV1 = &metav1.FieldsV1{Raw: []byte(fieldsV1)}
	return e
}

// containerResourcesFields is what the API server records for a manager that
// set `spec.template.spec.containers[name=app].resources.limits.memory`, which
// is exactly what `kubectl set resources` and a Dorgu heal both produce.
const containerResourcesFields = `{"f:spec":{"f:template":{"f:spec":{"f:containers":` +
	`{"k:{\"name\":\"app\"}":{"f:resources":{"f:limits":{"f:memory":{}}}}}}}}}`

// replicasOnlyFields is what an autoscaler records: `spec.replicas` and
// nothing in the pod template. Nowhere near a resource patch.
const replicasOnlyFields = `{"f:spec":{"f:replicas":{}}}`

// podAnnotationFields is what a sidecar injector records: an annotation on the
// pod template, no container resources.
const podAnnotationFields = `{"f:spec":{"f:template":{"f:metadata":{"f:annotations":` +
	`{"f:sidecar.acme.io/inject":{}}}}}}`

func TestDetectOwner(t *testing.T) {
	tests := []struct {
		name       string
		deploy     *appsv1.Deployment
		wantOwner  string
		wantDetail string
	}{
		{
			name:       "nil Deployment is unknown, which is treated as owned",
			deploy:     nil,
			wantOwner:  dorguv1.ManagedByUnknown,
			wantDetail: "",
		},
		{
			name: "Helm release annotations name the release",
			deploy: deployWith(nil, map[string]string{
				"meta.helm.sh/release-name":      "frontend",
				"meta.helm.sh/release-namespace": "apps",
			}, applyBy("helm")),
			wantOwner:  dorguv1.ManagedByHelm,
			wantDetail: `Helm release "frontend" in namespace apps`,
		},
		{
			name:       "the recommended managed-by label is enough for Helm",
			deploy:     deployWith(map[string]string{LabelManagedBy: "Helm"}, nil),
			wantOwner:  dorguv1.ManagedByHelm,
			wantDetail: "a Helm release",
		},
		{
			name:       "the helm field manager alone identifies Helm",
			deploy:     deployWith(nil, nil, applyBy("helm")),
			wantOwner:  dorguv1.ManagedByHelm,
			wantDetail: "a Helm release",
		},
		{
			name: "the ArgoCD tracking annotation names the application",
			deploy: deployWith(nil, map[string]string{
				"argocd.argoproj.io/tracking-id": "frontend:apps/Deployment:apps/frontend-podinfo",
			}),
			wantOwner:  dorguv1.ManagedByArgoCD,
			wantDetail: `ArgoCD application "frontend"`,
		},
		{
			name:       "the ArgoCD instance label names the application",
			deploy:     deployWith(map[string]string{"argocd.argoproj.io/instance": "frontend"}, nil),
			wantOwner:  dorguv1.ManagedByArgoCD,
			wantDetail: `ArgoCD application "frontend"`,
		},
		{
			name:       "the argocd field manager alone identifies ArgoCD",
			deploy:     deployWith(nil, nil, applyBy("argocd-controller")),
			wantOwner:  dorguv1.ManagedByArgoCD,
			wantDetail: "an ArgoCD application",
		},
		{
			name:       "Flux Kustomization labels win over anything Helm-shaped",
			deploy:     deployWith(map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", LabelManagedBy: "Helm"}, nil),
			wantOwner:  dorguv1.ManagedByFlux,
			wantDetail: `Flux Kustomization "apps"`,
		},
		{
			name: "a Flux HelmRelease is Flux, not Helm, because Flux reconciles it",
			deploy: deployWith(
				map[string]string{"helm.toolkit.fluxcd.io/name": "frontend", LabelManagedBy: "Helm"},
				map[string]string{"meta.helm.sh/release-name": "frontend"}),
			wantOwner:  dorguv1.ManagedByFlux,
			wantDetail: `Flux HelmRelease "frontend"`,
		},
		{
			name:       "the kustomize-controller field manager is Flux",
			deploy:     deployWith(nil, nil, applyBy("kustomize-controller")),
			wantOwner:  dorguv1.ManagedByFlux,
			wantDetail: "a Flux controller",
		},
		{
			name:       "ArgoCD wins over the Helm metadata it renders",
			deploy:     deployWith(map[string]string{"argocd.argoproj.io/instance": "frontend", LabelManagedBy: "Helm"}, nil),
			wantOwner:  dorguv1.ManagedByArgoCD,
			wantDetail: `ArgoCD application "frontend"`,
		},
		{
			name:       "a hand-written kustomize managed-by label is kustomize",
			deploy:     deployWith(map[string]string{LabelManagedBy: "kustomize"}, nil),
			wantOwner:  dorguv1.ManagedByKustomize,
			wantDetail: "a kustomize overlay",
		},
		{
			// F-08: kustomize's own managedByLabel build option writes the
			// version into the value, so the exact match this replaces never
			// fired on kustomize's own output.
			name:       "kustomize's own versioned managed-by label is kustomize",
			deploy:     deployWith(map[string]string{LabelManagedBy: "kustomize-v5.8.1"}, nil),
			wantOwner:  dorguv1.ManagedByKustomize,
			wantDetail: "a kustomize overlay",
		},
		{
			name: "kustomize origin build metadata is kustomize",
			deploy: deployWith(nil, map[string]string{
				"config.kubernetes.io/origin": "path: ../base/deploy.yaml\n",
			}, updateBy("kubectl-client-side-apply")),
			wantOwner:  dorguv1.ManagedByKustomize,
			wantDetail: "a kustomize overlay",
		},
		{
			name: "kustomize transformer build metadata is kustomize",
			deploy: deployWith(nil, map[string]string{
				"alpha.config.kubernetes.io/transformations": "- configuredIn: kustomization.yaml\n",
			}, updateBy("kubectl-client-side-apply")),
			wantOwner:  dorguv1.ManagedByKustomize,
			wantDetail: "a kustomize overlay",
		},
		{
			name: "kubectl by hand is unmanaged, which is the only patchable case",
			deploy: deployWith(nil, nil,
				updateBy("kubectl-client-side-apply"), updateBy("kubectl-set"), updateBy("kubectl-patch")),
			wantOwner:  dorguv1.ManagedByUnmanaged,
			wantDetail: "",
		},
		{
			name:       "no evidence at all is unmanaged",
			deploy:     deployWith(nil, nil),
			wantOwner:  dorguv1.ManagedByUnmanaged,
			wantDetail: "",
		},
		{
			name:       "an unrecognised server-side applier is unknown, and is named",
			deploy:     deployWith(nil, nil, applyBy("acme-platform-operator")),
			wantOwner:  dorguv1.ManagedByUnknown,
			wantDetail: `server-side applied by field manager "acme-platform-operator"`,
		},
		{
			name: "an Update by an unrecognised manager that holds no resources is not an owner",
			deploy: deployWith(nil, nil,
				withFields(updateBy("acme-platform-operator"), replicasOnlyFields)),
			wantOwner:  dorguv1.ManagedByUnmanaged,
			wantDetail: "",
		},
		{
			name: "a sidecar injector owning a pod annotation is not an owner",
			deploy: deployWith(nil, nil,
				withFields(updateBy("acme-sidecar-injector"), podAnnotationFields)),
			wantOwner:  dorguv1.ManagedByUnmanaged,
			wantDetail: "",
		},
		{
			name:       "Dorgu's own applies do not make a workload owned",
			deploy:     deployWith(nil, nil, applyBy("dorgu-operator")),
			wantOwner:  dorguv1.ManagedByUnmanaged,
			wantDetail: "",
		},
		{
			name: "Dorgu's own Update entry does not make a workload owned",
			deploy: deployWith(nil, nil,
				withFields(updateBy("dorgu"), containerResourcesFields)),
			wantOwner:  dorguv1.ManagedByUnmanaged,
			wantDetail: "",
		},
		{
			name: "a managedFields entry Dorgu cannot parse is treated as owned",
			deploy: deployWith(nil, nil,
				withFields(updateBy("acme-platform-operator"), `{"f:spec": not json}`)),
			wantOwner:  dorguv1.ManagedByUnknown,
			wantDetail: `field manager "acme-platform-operator" already owns this container's resources`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectOwner(tc.deploy)
			assert.Equal(t, tc.wantOwner, got.ManagedBy)
			assert.Equal(t, tc.wantDetail, got.Detail)
			assert.Equal(t, tc.wantOwner != dorguv1.ManagedByUnmanaged, got.IsOwned())
		})
	}
}

// TestForeignUpdateManagerOwningResourcesIsNotUnmanaged is F-03.
//
// The rule this replaces counted Apply-operation managers only, on the reasoning
// that an Update entry says nothing about whether a future patch will conflict.
// Server-side apply does not work that way. Reproduced against a real API
// server (kube-apiserver 1.36.2): with only `kubectl-set:Update` holding
// `.spec.template.spec.containers[name=probe].resources.limits.memory`, a
// foreign `kubectl apply --server-side` failed with
//
//	Apply failed with 1 conflict: conflict with "kubectl-set" using apps/v1:
//	  .spec.template.spec.containers[name="probe"].resources.limits.memory
//
// So a foreign manager holding those fields by Update is a real conflict risk
// and must not read as freely patchable.
func TestForeignUpdateManagerOwningResourcesIsNotUnmanaged(t *testing.T) {
	deploy := deployWith(nil, nil,
		updateBy("kube-controller-manager"),
		withFields(updateBy("acme-platform-operator"), containerResourcesFields),
	)

	got := DetectOwner(deploy)

	assert.Equal(t, dorguv1.ManagedByUnknown, got.ManagedBy,
		"an Update-operation manager holding the target fields is not freely patchable")
	assert.True(t, got.IsOwned())
	assert.Equal(t, `field manager "acme-platform-operator" already owns this container's resources`, got.Detail,
		"the refusal has to name the manager it is refusing on behalf of")
}

// TestKubectlUpdateManagersStayUnmanaged is the other half of F-03, and the
// line the fix above must not cross.
//
// `kubectl set resources` leaves a `kubectl-set:Update` entry on exactly the
// fields a remediation writes, and that entry genuinely will break a later
// `helm upgrade`. It is still not ownership: it is a human with kubectl, which
// is what unmanaged means. Refusing here would refuse the docs' own walkthrough
// and leave the pre-existing conflict in place; healing takes those fields over
// and the CLI then releases them, so the conflict is cleared rather than added
// to.
func TestKubectlUpdateManagersStayUnmanaged(t *testing.T) {
	deploy := deployWith(nil, nil,
		withFields(updateBy("kubectl-client-side-apply"), containerResourcesFields),
		withFields(updateBy("kubectl-set"), containerResourcesFields),
		withFields(updateBy("kubectl-patch"), containerResourcesFields),
		updateBy("kube-controller-manager"),
	)

	got := DetectOwner(deploy)

	assert.Equal(t, dorguv1.ManagedByUnmanaged, got.ManagedBy)
	assert.False(t, got.IsOwned())
}

// TestPlainKustomizeOverlayReadsUnmanaged is F-08, pinned as the honest answer
// rather than the advertised one.
//
// A plain `kubectl apply -k` produces a Deployment with no label, no
// annotation, and the same `kubectl-client-side-apply` field manager as
// `kubectl apply -f`. Verified against kustomize v5.8.1 as shipped in kubectl:
// there is nothing to detect. This test exists so that fact is recorded in the
// codebase instead of contradicted by the docs.
func TestPlainKustomizeOverlayReadsUnmanaged(t *testing.T) {
	// Byte-for-byte what `kubectl apply -k` on an ordinary namePrefix overlay
	// leaves behind.
	deploy := deployWith(nil,
		map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
		withFields(updateBy("kubectl-client-side-apply"), containerResourcesFields),
	)

	got := DetectOwner(deploy)

	assert.Equal(t, dorguv1.ManagedByUnmanaged, got.ManagedBy,
		"kustomize emits no default marker, so Dorgu cannot and must not claim to detect it")
	assert.False(t, got.IsOwned())
}

func TestOwnsContainerResources(t *testing.T) {
	tests := []struct {
		name     string
		fieldsV1 string
		want     bool
	}{
		{"no fieldsV1 at all owns nothing", "", false},
		{"a container's resources block", containerResourcesFields, true},
		{"replicas only", replicasOnlyFields, false},
		{"a pod template annotation only", podAnnotationFields, false},
		{"metadata only", `{"f:metadata":{"f:labels":{"f:app":{}}}}`, false},
		{
			name:     "a sibling container's resources still count",
			fieldsV1: `{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"sidecar\"}":{"f:resources":{}}}}}}}`,
			want:     true,
		},
		{
			name:     "a container claimed without its resources does not count",
			fieldsV1: `{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"app\"}":{"f:image":{}}}}}}}`,
			want:     false,
		},
		{"unparseable fieldsV1 is treated as owning", `{"f:spec": not json}`, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := updateBy("acme")
			if tc.fieldsV1 != "" {
				entry = withFields(entry, tc.fieldsV1)
			}
			assert.Equal(t, tc.want, ownsContainerResources(entry))
		})
	}
}

// TestDetectOwner_UnknownIsOwned pins the default the whole safety story rests
// on: anything Dorgu cannot positively call unmanaged is treated as owned.
func TestDetectOwner_UnknownIsOwned(t *testing.T) {
	assert.True(t, Ownership{ManagedBy: dorguv1.ManagedByUnknown}.IsOwned())
	assert.True(t, (&dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByUnknown}).IsOwned())
	assert.True(t, (*dorguv1.WorkloadRef)(nil).IsOwned(), "an absent record is owned")
	assert.False(t, (&dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByUnmanaged}).IsOwned())
}
