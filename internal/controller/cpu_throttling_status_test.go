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

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// C4: "detection degrades loudly but harmlessly". The collector says which source it
// reads, or why it reads none; the sink puts that on the ClusterPersona, where a
// status condition and a structured field (the one `dorgu health` reads) carry it.

// noiseWatchClient is noiseClient with the type interceptor.NewClient wants.
func noiseWatchClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(noiseScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.RemediationAction{}, &dorguv1.ClusterPersona{}).
		Build()
}

func clusterPersonaNamed(name, promURL string) *dorguv1.ClusterPersona {
	cp := &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       dorguv1.ClusterPersonaSpec{Name: name},
	}
	if promURL != "" {
		cp.Spec.Metrics = &dorguv1.ClusterMetricsSpec{Prometheus: &dorguv1.PrometheusSpec{URL: promURL}}
	}
	return cp
}

func getClusterPersona(t *testing.T, c client.Client) dorguv1.ClusterPersona {
	t.Helper()
	var cp dorguv1.ClusterPersona
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "dorgu"}, &cp))
	return cp
}

func TestPrometheusURLFromClusterPersona(t *testing.T) {
	tests := []struct {
		name string
		objs []client.Object
		want string
	}{
		{"none", nil, ""},
		{"persona without metrics", []client.Object{clusterPersonaNamed("a", "")}, ""},
		{"one url", []client.Object{clusterPersonaNamed("a", "http://prom:9090")}, "http://prom:9090"},
		{"first by name wins, deterministically", []client.Object{
			clusterPersonaNamed("b", "http://other:9090"), clusterPersonaNamed("a", "http://prom:9090"),
		}, "http://prom:9090"},
		{"a persona with no url does not hide another's", []client.Object{
			clusterPersonaNamed("a", ""), clusterPersonaNamed("b", "http://prom:9090"),
		}, "http://prom:9090"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := noiseClient(t, tt.objs...)
			got, err := PrometheusURLFromClusterPersona(c)(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPrometheusURLFromClusterPersona_ListErrorIsAnError(t *testing.T) {
	c := interceptor.NewClient(noiseWatchClient(t), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden")
		},
	})
	_, err := PrometheusURLFromClusterPersona(c)(context.Background())
	require.Error(t, err, "an unreadable policy is not 'no Prometheus'")
}

func TestCPUThrottlingStatusSink_WritesTheFieldAndTheCondition(t *testing.T) {
	c := noiseClient(t, clusterPersonaNamed("dorgu", ""))
	sink := NewCPUThrottlingStatusSink(c, noiseLogger())

	sink.ReportCPUThrottlingSource(context.Background(), detection.SourceStatus{
		Active: false, Source: detection.CPUThrottlingSourceNone, Reason: detection.ReasonNoMetricsSource,
		Message: "CPU throttling detection is off: neither metrics.k8s.io nor spec.metrics.prometheus.url is available",
	})

	cp := getClusterPersona(t, c)
	require.NotNil(t, cp.Status.CPUThrottlingDetection)
	assert.False(t, cp.Status.CPUThrottlingDetection.Enabled)
	assert.Equal(t, "none", cp.Status.CPUThrottlingDetection.Source)
	assert.Equal(t, "NoMetricsSource", cp.Status.CPUThrottlingDetection.Reason)
	assert.Contains(t, cp.Status.CPUThrottlingDetection.Message, "off")

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, ConditionCPUThrottlingDetection)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "NoMetricsSource", cond.Reason)
	assert.Contains(t, cond.Message, "off")
}

func TestCPUThrottlingStatusSink_ActiveSources(t *testing.T) {
	tests := []struct {
		name   string
		status detection.SourceStatus
		source string
	}{
		{"metrics server", detection.SourceStatus{Active: true, Source: detection.CPUThrottlingSourceMetricsServer, Reason: detection.ReasonMetricsServerActive}, "metrics.k8s.io"},
		{"prometheus", detection.SourceStatus{Active: true, Source: detection.CPUThrottlingSourcePrometheus, Reason: detection.ReasonPrometheusActive}, "prometheus"},
		{"fallback is announced", detection.SourceStatus{Active: true, Source: detection.CPUThrottlingSourceMetricsServer, Reason: detection.ReasonPrometheusUnreachable, Message: "using metrics.k8s.io"}, "metrics.k8s.io"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := noiseClient(t, clusterPersonaNamed("dorgu", ""))
			NewCPUThrottlingStatusSink(c, noiseLogger()).ReportCPUThrottlingSource(context.Background(), tt.status)
			cp := getClusterPersona(t, c)
			require.NotNil(t, cp.Status.CPUThrottlingDetection)
			assert.True(t, cp.Status.CPUThrottlingDetection.Enabled)
			assert.Equal(t, tt.source, cp.Status.CPUThrottlingDetection.Source)
			assert.Equal(t, tt.status.Reason, cp.Status.CPUThrottlingDetection.Reason)
			cond := apimeta.FindStatusCondition(cp.Status.Conditions, ConditionCPUThrottlingDetection)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
		})
	}
}

func TestCPUThrottlingStatusSink_WritesOnlyOnChange(t *testing.T) {
	writes := 0
	base := noiseWatchClient(t, clusterPersonaNamed("dorgu", ""))
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	sink := NewCPUThrottlingStatusSink(c, noiseLogger())
	off := detection.SourceStatus{Source: detection.CPUThrottlingSourceNone, Reason: detection.ReasonNoMetricsSource, Message: "off"}

	for range 50 {
		sink.ReportCPUThrottlingSource(context.Background(), off)
	}
	assert.Equal(t, 1, writes, "a state that has not changed is not rewritten every cycle")

	sink.ReportCPUThrottlingSource(context.Background(), detection.SourceStatus{
		Active: true, Source: detection.CPUThrottlingSourceMetricsServer, Reason: detection.ReasonMetricsServerActive})
	assert.Equal(t, 2, writes, "a change is")
}

func TestCPUThrottlingStatusSink_NoClusterPersonaIsHarmless(t *testing.T) {
	c := noiseClient(t)
	sink := NewCPUThrottlingStatusSink(c, noiseLogger())
	assert.NotPanics(t, func() {
		sink.ReportCPUThrottlingSource(context.Background(), detection.SourceStatus{Reason: detection.ReasonNoMetricsSource})
	})
}

func TestCPUThrottlingStatusSink_FailedWriteIsRetriedNextCycle(t *testing.T) {
	fail := true
	writes := 0
	base := noiseWatchClient(t, clusterPersonaNamed("dorgu", ""))
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if fail {
				return errors.New("conflict storm")
			}
			writes++
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	sink := NewCPUThrottlingStatusSink(c, noiseLogger())
	st := detection.SourceStatus{Reason: detection.ReasonNoMetricsSource, Source: detection.CPUThrottlingSourceNone, Message: "off"}

	assert.NotPanics(t, func() { sink.ReportCPUThrottlingSource(context.Background(), st) })
	fail = false
	sink.ReportCPUThrottlingSource(context.Background(), st)
	assert.Equal(t, 1, writes, "a write that failed must not be remembered as done")
	assert.NotNil(t, getClusterPersona(t, base).Status.CPUThrottlingDetection)
}

func TestCPUThrottlingStatusSink_FollowsARecreatedClusterPersona(t *testing.T) {
	base := noiseWatchClient(t, clusterPersonaNamed("dorgu", ""))
	sink := NewCPUThrottlingStatusSink(base, noiseLogger())
	st := detection.SourceStatus{Reason: detection.ReasonNoMetricsSource, Source: detection.CPUThrottlingSourceNone, Message: "off"}
	sink.ReportCPUThrottlingSource(context.Background(), st)

	cp := getClusterPersona(t, base)
	require.NoError(t, base.Delete(context.Background(), &cp))
	require.NoError(t, base.Create(context.Background(), clusterPersonaNamed("dorgu", "")))

	sink.ReportCPUThrottlingSource(context.Background(), st)
	assert.NotNil(t, getClusterPersona(t, base).Status.CPUThrottlingDetection,
		"the sink reads the object, not its own memory of what it wrote")
}
