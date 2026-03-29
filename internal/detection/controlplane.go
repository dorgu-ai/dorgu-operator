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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	controlPlaneCollectorName = "controlplane-health-checker"
	leaseStaleThreshold       = 60 * time.Second
)

// leaseCheck maps a kube-system lease to its signal.
type leaseCheck struct {
	leaseName  string
	signalType SignalType
	component  string
}

var leaseChecks = []leaseCheck{
	{leaseName: "kube-scheduler", signalType: SignalSchedulerUnhealthy, component: "kube-scheduler"},
	{leaseName: "kube-controller-manager", signalType: SignalControllerMgrUnhealth, component: "kube-controller-manager"},
}

// ControlPlaneCollector detects control plane health issues.
type ControlPlaneCollector struct {
	client     client.Reader
	restClient rest.Interface
	logger     logr.Logger
}

// NewControlPlaneCollector creates a collector that checks control plane health.
func NewControlPlaneCollector(c client.Reader, restClient rest.Interface, logger logr.Logger) *ControlPlaneCollector {
	return &ControlPlaneCollector{
		client:     c,
		restClient: restClient,
		logger:     logger.WithName(controlPlaneCollectorName),
	}
}

func (cp *ControlPlaneCollector) Name() string { return controlPlaneCollectorName }

func (cp *ControlPlaneCollector) Collect(ctx context.Context) ([]Signal, error) {
	now := time.Now()
	var signals []Signal

	// Check API server health endpoints
	signals = append(signals, cp.checkHealthEndpoints(ctx, now)...)

	// Check lease freshness for scheduler and controller-manager
	signals = append(signals, cp.checkLeases(ctx, now)...)

	return signals, nil
}

// checkHealthEndpoints probes /readyz and /livez on the API server.
func (cp *ControlPlaneCollector) checkHealthEndpoints(ctx context.Context, now time.Time) []Signal {
	if cp.restClient == nil {
		return nil
	}

	var signals []Signal

	for _, endpoint := range []string{"/readyz", "/livez"} {
		result := cp.restClient.Get().AbsPath(endpoint).Do(ctx)
		if err := result.Error(); err != nil {
			signals = append(signals, Signal{
				Type:     SignalAPIServerUnhealthy,
				Severity: SeverityCritical,
				Category: CategoryControlPlane,
				Source:   controlPlaneCollectorName,
				Message:  fmt.Sprintf("API server %s check failed: %v", endpoint, err),
				Resource: dorguv1.ResourceReference{
					Kind: "ComponentStatus",
					Name: "kube-apiserver",
				},
				DetectedAt: now,
				Metadata: map[string]string{
					"endpoint": endpoint,
				},
			})
		}
	}

	return signals
}

// checkLeases verifies lease freshness for scheduler and controller-manager.
func (cp *ControlPlaneCollector) checkLeases(ctx context.Context, now time.Time) []Signal {
	var signals []Signal

	for _, check := range leaseChecks {
		lease := &coordinationv1.Lease{}
		err := cp.client.Get(ctx, types.NamespacedName{
			Name:      check.leaseName,
			Namespace: "kube-system",
		}, lease)
		if err != nil {
			// Lease may not exist on managed clusters — skip gracefully
			cp.logger.V(1).Info("could not get lease", "lease", check.leaseName, "error", err)
			continue
		}

		if lease.Spec.RenewTime == nil {
			continue
		}

		staleDuration := now.Sub(lease.Spec.RenewTime.Time)
		if staleDuration > leaseStaleThreshold {
			staleSeconds := staleDuration.Seconds()
			thresholdSeconds := leaseStaleThreshold.Seconds()

			signals = append(signals, Signal{
				Type:     check.signalType,
				Severity: SeverityWarning,
				Category: CategoryControlPlane,
				Source:   controlPlaneCollectorName,
				Message:  fmt.Sprintf("%s lease is stale (%.0fs since last renewal)", check.component, staleSeconds),
				Resource: dorguv1.ResourceReference{
					Kind:      "Lease",
					Name:      check.leaseName,
					Namespace: "kube-system",
				},
				Value:      &staleSeconds,
				Threshold:  &thresholdSeconds,
				DetectedAt: lease.Spec.RenewTime.Time,
				Metadata: map[string]string{
					"component":    check.component,
					"lastRenewAge": fmt.Sprintf("%.0fs", staleSeconds),
				},
			})
		}
	}

	return signals
}
