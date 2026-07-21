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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPodByNodeName(t *testing.T) {
	tests := []struct {
		name string
		obj  client.Object
		want []string
	}{
		{
			name: "scheduled pod indexes its node",
			obj:  &corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-a"}},
			want: []string{"node-a"},
		},
		{
			name: "unscheduled pod contributes nothing",
			obj:  &corev1.Pod{Spec: corev1.PodSpec{NodeName: ""}},
			want: nil,
		},
		{
			name: "non-pod object contributes nothing",
			obj:  &corev1.Node{},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PodByNodeName(tt.obj))
		})
	}
}

// TestPodNodeNameIndex_EnablesNodeScopedList proves the registered index name +
// index func let a spec.nodeName field selector return only the pods on the
// requested node — the query the resource-saturation checker relies on. Without
// the index the cache rejects the selector with
// "Index with name field:spec.nodeName does not exist".
func TestPodNodeNameIndex_EnablesNodeScopedList(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	podOnA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}, Spec: corev1.PodSpec{NodeName: "node-a"}}
	podOnB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default"}, Spec: corev1.PodSpec{NodeName: "node-b"}}
	unscheduled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(podOnA, podOnB, unscheduled).
		WithIndex(&corev1.Pod{}, PodNodeNameIndex, PodByNodeName).
		Build()

	var list corev1.PodList
	require.NoError(t, c.List(context.Background(), &list, &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(PodNodeNameIndex, "node-a"),
	}))

	require.Len(t, list.Items, 1, "only the pod on node-a should match")
	assert.Equal(t, "a", list.Items[0].Name)
}
