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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	clienttesting "k8s.io/client-go/testing"
)

func fakeMetricsClient(withGroup bool, objs ...runtime.Object) *metricsfake.Clientset {
	cs := metricsfake.NewSimpleClientset(objs...)
	if withGroup {
		cs.Fake.Resources = []*metav1.APIResourceList{{GroupVersion: "metrics.k8s.io/v1beta1"}}
	}
	return cs
}

func TestMetricsAPISource_Available(t *testing.T) {
	ok, err := NewMetricsAPISource(fakeMetricsClient(true)).Available(context.Background())
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = NewMetricsAPISource(fakeMetricsClient(false)).Available(context.Background())
	require.NoError(t, err)
	assert.False(t, ok, "no metrics.k8s.io group is an absence, not an error")
}

func TestMetricsAPISource_DiscoveryErrorIsAnError(t *testing.T) {
	cs := fakeMetricsClient(true)
	cs.Fake.PrependReactor("get", "group", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("discovery timeout")
	})
	ok, err := NewMetricsAPISource(cs).Available(context.Background())
	require.Error(t, err, "if discovery cannot say, the caller must not read it as absent")
	assert.False(t, ok)
}

func TestMetricsAPISource_PodMetrics(t *testing.T) {
	pm := &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop"},
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "app", Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("900m")},
		}},
	}
	items, err := NewMetricsAPISource(fakeMetricsClient(true, pm)).PodMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "api-1", items[0].Name)
}

func TestMetricsAPISource_ListError(t *testing.T) {
	cs := fakeMetricsClient(true)
	cs.Fake.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("503 service unavailable")
	})
	_, err := NewMetricsAPISource(cs).PodMetrics(context.Background())
	require.Error(t, err)
}
