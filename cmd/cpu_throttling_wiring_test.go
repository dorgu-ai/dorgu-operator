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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// C4: the wiring main() uses, asserted without a cluster: the collector reads the
// Prometheus URL from the ClusterPersona, honours --enable-metrics-server, and puts its
// state on the ClusterPersona status.

func wiringClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, dorguv1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&dorguv1.ClusterPersona{}).Build()
}

func wiringClusterPersona(prometheusURL string) *dorguv1.ClusterPersona {
	cp := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "dorgu"},
		Spec:       dorguv1.ClusterPersonaSpec{Name: "dorgu"},
	}
	if prometheusURL != "" {
		cp.Spec.Metrics = &dorguv1.ClusterMetricsSpec{Prometheus: &dorguv1.PrometheusSpec{URL: prometheusURL}}
	}
	return cp
}

// wiringMetricsClient is a fake metrics-server client that serves metrics.k8s.io.
// The generated fake has no NewClientset in this version of k8s.io/metrics.
func wiringMetricsClient(served bool) *metricsfake.Clientset {
	mc := metricsfake.NewSimpleClientset() //nolint:staticcheck // SA1019: the only generated constructor
	if served {
		mc.Resources = []*metav1.APIResourceList{{GroupVersion: "metrics.k8s.io/v1beta1"}}
	}
	return mc
}

func wiringPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "app",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
		}}},
	}
}

func statusOf(t *testing.T, c client.Client) *dorguv1.CPUThrottlingDetectionStatus {
	t.Helper()
	var cp dorguv1.ClusterPersona
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "dorgu"}, &cp))
	return cp.Status.CPUThrottlingDetection
}

func TestNewCPUThrottlingCollector_NoSourceReportsOff(t *testing.T) {
	c := wiringClient(t, wiringClusterPersona(""), wiringPod())
	cfg := operatorConfig{enableMetricsServer: true}
	col := newCPUThrottlingCollector(c, nil, wiringMetricsClient(false), cfg, logr.Discard())

	signals, err := col.Collect(context.Background())
	require.NoError(t, err)
	assert.Empty(t, signals)
	st := statusOf(t, c)
	require.NotNil(t, st)
	assert.False(t, st.Enabled)
	assert.Equal(t, "NoMetricsSource", st.Reason)
}

func TestNewCPUThrottlingCollector_MetricsServerPresent(t *testing.T) {
	c := wiringClient(t, wiringClusterPersona(""), wiringPod())
	cfg := operatorConfig{enableMetricsServer: true}
	col := newCPUThrottlingCollector(c, nil, wiringMetricsClient(true), cfg, logr.Discard())

	_, err := col.Collect(context.Background())
	require.NoError(t, err)
	st := statusOf(t, c)
	require.NotNil(t, st)
	assert.True(t, st.Enabled)
	assert.Equal(t, "metrics.k8s.io", st.Source)
}

func TestNewCPUThrottlingCollector_FlagOffDisablesMetricsServer(t *testing.T) {
	c := wiringClient(t, wiringClusterPersona(""), wiringPod())
	cfg := operatorConfig{enableMetricsServer: false}
	col := newCPUThrottlingCollector(c, nil, wiringMetricsClient(true), cfg, logr.Discard())

	_, err := col.Collect(context.Background())
	require.NoError(t, err)
	st := statusOf(t, c)
	require.NotNil(t, st)
	assert.False(t, st.Enabled)
	assert.Equal(t, "MetricsServerDisabled", st.Reason)
}

func TestNewCPUThrottlingCollector_PrometheusFromTheClusterPersonaWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"namespace":"shop","pod":"api-1","container":"app"},"value":[1,"0.6"]}]}}`))
	}))
	t.Cleanup(srv.Close)

	c := wiringClient(t, wiringClusterPersona(srv.URL), wiringPod())
	cfg := operatorConfig{enableMetricsServer: true, healthCheckInterval: time.Minute}
	col := newCPUThrottlingCollector(c, nil, wiringMetricsClient(true), cfg, logr.Discard())

	signals, err := col.Collect(context.Background())
	require.NoError(t, err)
	require.Len(t, signals, 1)
	assert.Equal(t, "prometheus", signals[0].Metadata["source"])
	st := statusOf(t, c)
	require.NotNil(t, st)
	assert.Equal(t, "prometheus", st.Source)
}

func TestNewCPUThrottlingCollector_NilMetricsClientIsOff(t *testing.T) {
	c := wiringClient(t, wiringPod())
	col := newCPUThrottlingCollector(c, nil, nil, operatorConfig{enableMetricsServer: true}, logr.Discard())
	signals, err := col.Collect(context.Background())
	require.NoError(t, err, "a metrics client that could not be built is a missing source, reported as such")
	assert.Empty(t, signals)
}
