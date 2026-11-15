package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// C4b N-4: the "old CRD" flag was set once per process and never cleared, so after an
// admin applied the new CRDs (as the log line says) status.cpuThrottlingDetection stayed
// empty until the operator restarted. The sink now re-checks on an interval.

func TestC4bN4_TheOldCRDFlagIsRecheckedAndClearsOnceTheCRDIsUpgraded(t *testing.T) {
	updates, prune := 0, true
	base := noiseWatchClient(t, clusterPersonaNamed("dorgu", ""))
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if cp, ok := obj.(*dorguv1.ClusterPersona); ok {
				updates++
				if prune {
					cp.Status.CPUThrottlingDetection = nil // an old CRD's pruning
				}
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	logger, sink := newRecordingLogger()
	s := NewCPUThrottlingStatusSink(c, logger)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	status := detection.SourceStatus{Active: true, Source: detection.CPUThrottlingSourceMetricsServer,
		Reason: detection.ReasonMetricsServerActive, Message: "reads metrics.k8s.io"}
	report := func() { s.ReportCPUThrottlingSource(context.Background(), status) }
	crdLines := func() int {
		n := 0
		for _, m := range sink.messages() {
			if strings.HasPrefix(m, "ERROR ") && strings.Contains(m, "CustomResourceDefinition") {
				n++
			}
		}
		return n
	}

	report()
	require.Equal(t, 1, updates)
	require.Equal(t, 1, crdLines())

	now = now.Add(time.Minute)
	report()
	report()
	assert.Equal(t, 1, updates, "inside the interval an old CRD costs no writes")

	now = now.Add(cpuFieldRecheckInterval)
	report() // still the old CRD: one more attempt, one more line, and back to ignoring
	assert.Equal(t, 2, updates)
	assert.Equal(t, 2, crdLines(), "the line comes back once per interval while the CRD is old: %v", sink.messages())
	report()
	assert.Equal(t, 2, updates)

	prune = false // the admin applies the new CRDs
	now = now.Add(cpuFieldRecheckInterval)
	report()
	assert.Equal(t, 3, updates, "the re-check writes the field")
	cp := getClusterPersona(t, c)
	require.NotNil(t, cp.Status.CPUThrottlingDetection, "status.cpuThrottlingDetection is there without a restart")
	assert.Equal(t, detection.ReasonMetricsServerActive, cp.Status.CPUThrottlingDetection.Reason)
	assert.Equal(t, 2, crdLines(), "and nothing more is logged")

	report()
	assert.Equal(t, 3, updates, "from then on the field is compared again, so an unchanged state is no write")
	status.Reason = detection.ReasonMetricsSourceLost
	status.Active = false
	report()
	assert.Equal(t, 4, updates, "and a change is written")
}

func TestC4bN4_TheLogLineSaysHowLongTheFlagLasts(t *testing.T) {
	base := noiseWatchClient(t, clusterPersonaNamed("dorgu", ""))
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if cp, ok := obj.(*dorguv1.ClusterPersona); ok {
				cp.Status.CPUThrottlingDetection = nil
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	logger, sink := newRecordingLogger()
	NewCPUThrottlingStatusSink(c, logger).ReportCPUThrottlingSource(context.Background(),
		detection.SourceStatus{Active: true, Source: detection.CPUThrottlingSourceMetricsServer, Reason: detection.ReasonMetricsServerActive})
	joined := strings.Join(sink.messages(), "\n")
	assert.Contains(t, joined, "no restart is needed", "the admin is told the operator notices the upgrade by itself")
}
