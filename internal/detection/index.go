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
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodNodeNameIndex is the controller-runtime cache field-index key for a Pod's
// spec.nodeName. The resource-saturation checker lists pods-by-node through it
// (client.MatchingFields / a spec.nodeName field selector); the manager must
// register the index at startup or the cache rejects the selector with
// "Index with name field:spec.nodeName does not exist".
const PodNodeNameIndex = "spec.nodeName"

// PodByNodeName extracts the index values for PodNodeNameIndex from a Pod.
// Pods not yet scheduled (empty NodeName) contribute no index entry.
func PodByNodeName(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}
	return []string{pod.Spec.NodeName}
}

// RegisterPodNodeNameIndex registers the spec.nodeName Pod index on a manager's
// field indexer. Call it before the cache starts (i.e. before mgr.Start).
func RegisterPodNodeNameIndex(ctx context.Context, fi client.FieldIndexer) error {
	return fi.IndexField(ctx, &corev1.Pod{}, PodNodeNameIndex, PodByNodeName)
}
