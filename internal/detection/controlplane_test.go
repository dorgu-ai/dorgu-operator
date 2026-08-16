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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestControlPlaneCollector_Name(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true))
	c := NewControlPlaneCollector(fake.NewClientBuilder().Build(), nil, logger)
	assert.Equal(t, "controlplane-health-checker", c.Name())
}

func TestControlPlaneCollector_FreshLeases(t *testing.T) {
	now := time.Now()
	renewTime := metav1.NewMicroTime(now.Add(-10 * time.Second)) // 10s ago = fresh

	schedulerLease := makeLease("kube-scheduler", &renewTime)
	cmLease := makeLease("kube-controller-manager", &renewTime)

	signals := collectCPSignals(t, schedulerLease, cmLease)
	assert.Empty(t, signals, "fresh leases should not produce signals")
}

func TestControlPlaneCollector_StaleSchedulerLease(t *testing.T) {
	now := time.Now()
	staleTime := metav1.NewMicroTime(now.Add(-120 * time.Second)) // 120s ago = stale
	freshTime := metav1.NewMicroTime(now.Add(-10 * time.Second))

	schedulerLease := makeLease("kube-scheduler", &staleTime)
	cmLease := makeLease("kube-controller-manager", &freshTime)

	signals := collectCPSignals(t, schedulerLease, cmLease)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalSchedulerUnhealthy, signals[0].Type)
	assert.Equal(t, SeverityWarning, signals[0].Severity)
	assert.Equal(t, CategoryControlPlane, signals[0].Category)
	assert.Contains(t, signals[0].Message, "kube-scheduler")
}

func TestControlPlaneCollector_StaleControllerManagerLease(t *testing.T) {
	now := time.Now()
	staleTime := metav1.NewMicroTime(now.Add(-120 * time.Second))
	freshTime := metav1.NewMicroTime(now.Add(-10 * time.Second))

	schedulerLease := makeLease("kube-scheduler", &freshTime)
	cmLease := makeLease("kube-controller-manager", &staleTime)

	signals := collectCPSignals(t, schedulerLease, cmLease)
	require.Len(t, signals, 1)
	assert.Equal(t, SignalControllerMgrUnhealth, signals[0].Type)
	assert.Contains(t, signals[0].Message, "kube-controller-manager")
}

func TestControlPlaneCollector_BothStale(t *testing.T) {
	now := time.Now()
	staleTime := metav1.NewMicroTime(now.Add(-120 * time.Second))

	schedulerLease := makeLease("kube-scheduler", &staleTime)
	cmLease := makeLease("kube-controller-manager", &staleTime)

	signals := collectCPSignals(t, schedulerLease, cmLease)
	assert.Len(t, signals, 2)
}

func TestControlPlaneCollector_NoLeases(t *testing.T) {
	// Managed cluster scenario — leases may not exist
	signals := collectCPSignals(t)
	assert.Empty(t, signals, "missing leases should be skipped gracefully")
}

func TestControlPlaneCollector_LeaseWithNilRenewTime(t *testing.T) {
	lease := makeLease("kube-scheduler", nil)

	signals := collectCPSignals(t, lease)
	assert.Empty(t, signals, "lease with nil renewTime should be skipped")
}

func TestControlPlaneCollector_StaleLeaseHasValueAndThreshold(t *testing.T) {
	now := time.Now()
	staleTime := metav1.NewMicroTime(now.Add(-120 * time.Second))

	lease := makeLease("kube-scheduler", &staleTime)

	signals := collectCPSignals(t, lease)
	require.Len(t, signals, 1)
	require.NotNil(t, signals[0].Value)
	require.NotNil(t, signals[0].Threshold)
	assert.Greater(t, *signals[0].Value, 60.0) // stale duration > 60s
	assert.InDelta(t, 60.0, *signals[0].Threshold, 1.0)
}

func TestControlPlaneCollector_NoRestClient(t *testing.T) {
	// When restClient is nil, health endpoint checks should be skipped
	now := time.Now()
	freshTime := metav1.NewMicroTime(now.Add(-10 * time.Second))
	lease := makeLease("kube-scheduler", &freshTime)

	signals := collectCPSignals(t, lease)
	assert.Empty(t, signals)
}

// --- helpers ---

func makeLease(name string, renewTime *metav1.MicroTime) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kube-system",
		},
	}
	if renewTime != nil {
		lease.Spec.RenewTime = renewTime
	}
	return lease
}

func collectCPSignals(t *testing.T, leases ...*coordinationv1.Lease) []Signal {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, coordinationv1.AddToScheme(scheme))

	objs := make([]runtime.Object, 0, len(leases))
	for _, l := range leases {
		objs = append(objs, l)
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	logger := zap.New(zap.UseDevMode(true))
	// nil restClient — health endpoint checks are tested separately
	collector := NewControlPlaneCollector(fakeClient, nil, logger)

	signals, err := collector.Collect(context.Background())
	require.NoError(t, err)
	return signals
}
