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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// R8 of the #54 adversarial review.

func boolPtr(b bool) *bool { return &b }

func controlledBy(kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "example.com/v1", Kind: kind, Name: name, UID: "uid-owner", Controller: boolPtr(true),
	}
}

func TestDetectOwner_ControllerOwnerReferenceMeansOwned(t *testing.T) {
	deploy := deployWith(nil, nil)
	deploy.OwnerReferences = []metav1.OwnerReference{controlledBy("WebApp", "shop-web")}

	owner := DetectOwner(deploy)

	assert.True(t, owner.IsOwned(), "a Deployment a custom resource controls is reconciled by that controller")
	assert.NotEqual(t, dorguv1.ManagedByUnmanaged, owner.ManagedBy)
	assert.Contains(t, owner.Detail, "WebApp")
}

func TestDetectOwner_NonControllerOwnerReferenceIsNotOwnership(t *testing.T) {
	deploy := deployWith(nil, nil)
	deploy.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "example.com/v1", Kind: "Group", Name: "g", UID: "u", Controller: boolPtr(false),
	}, {
		APIVersion: "example.com/v1", Kind: "Group", Name: "h", UID: "v",
	}}

	assert.False(t, DetectOwner(deploy).IsOwned())
}

func TestDetectOwner_ServerSideKubectlApplyIsAReconciler(t *testing.T) {
	// `kubectl apply --server-side` from CI is an Apply-operation entry under the
	// manager name "kubectl". It re-asserts the manifest on every run.
	owner := DetectOwner(deployWith(nil, nil, applyBy("kubectl")))

	assert.True(t, owner.IsOwned())
}

func TestDetectOwner_ABenignPrefixIsNotABenignManager(t *testing.T) {
	// Only Dorgu's own exact managers are benign. A manager that merely starts with
	// "dorgu" or "kubectl" is somebody else's.
	for _, entry := range []metav1.ManagedFieldsEntry{
		applyBy("dorgu-evil-pipeline"),
		applyBy("kubectl-pipeline"),
		withFields(updateBy("dorguops"), containerResourcesFields),
	} {
		assert.True(t, DetectOwner(deployWith(nil, nil, entry)).IsOwned(), "manager %q", entry.Manager)
	}
}

func TestDetectOwner_DorguOwnManagersAndHandsOnKubectlStayUnmanaged(t *testing.T) {
	for _, entry := range []metav1.ManagedFieldsEntry{
		applyBy("dorgu"), applyBy("dorgu-operator"), updateBy("dorgu"), updateBy("dorgu-operator"),
		updateBy("kubectl-client-side-apply"), updateBy("kubectl-set"), updateBy("kubectl-patch"),
		updateBy("kubectl-edit"), updateBy("kube-controller-manager"),
	} {
		assert.False(t, DetectOwner(deployWith(nil, nil, entry)).IsOwned(), "manager %q (%s)", entry.Manager, entry.Operation)
	}
}
