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
			name:       "a kustomize managed-by label is kustomize",
			deploy:     deployWith(map[string]string{LabelManagedBy: "kustomize"}, nil),
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
			name:       "an Update by an unrecognised manager does not claim ownership",
			deploy:     deployWith(nil, nil, updateBy("acme-platform-operator")),
			wantOwner:  dorguv1.ManagedByUnmanaged,
			wantDetail: "",
		},
		{
			name:       "Dorgu's own applies do not make a workload owned",
			deploy:     deployWith(nil, nil, applyBy("dorgu-operator")),
			wantOwner:  dorguv1.ManagedByUnmanaged,
			wantDetail: "",
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

// TestDetectOwner_UnknownIsOwned pins the default the whole safety story rests
// on: anything Dorgu cannot positively call unmanaged is treated as owned.
func TestDetectOwner_UnknownIsOwned(t *testing.T) {
	assert.True(t, Ownership{ManagedBy: dorguv1.ManagedByUnknown}.IsOwned())
	assert.True(t, (&dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByUnknown}).IsOwned())
	assert.True(t, (*dorguv1.WorkloadRef)(nil).IsOwned(), "an absent record is owned")
	assert.False(t, (&dorguv1.WorkloadRef{ManagedBy: dorguv1.ManagedByUnmanaged}).IsOwned())
}
