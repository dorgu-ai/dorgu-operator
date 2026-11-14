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
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// ConditionCPUThrottlingDetection reports whether CPU throttling detection can see.
// False means it is off, with the reason: a cluster without a metrics source is not
// broken, but it must not look as if throttling is being watched when it is not.
const ConditionCPUThrottlingDetection = "CPUThrottlingDetection"

// statusLogInterval bounds how often a failure to write the status is logged.
const statusLogInterval = 5 * time.Minute

// CPUThrottlingStatusSink writes the CPU throttling collector's source state to every
// ClusterPersona: a condition for kubectl, and a structured field for `dorgu health`.
//
// It reads before it writes and writes only a difference, so a state that holds for a
// day is one write, not one per cycle, and it keeps no memory of what it wrote: a
// recreated ClusterPersona is brought up to date on the next cycle. A failed write is
// logged (rate-limited) and retried the next cycle; it never fails detection.
type CPUThrottlingStatusSink struct {
	client client.Client
	logger logr.Logger

	mu      sync.Mutex
	lastLog time.Time
	held    int

	// fieldPruned is set when a write came back without status.cpuThrottlingDetection:
	// the installed CRD predates it (Helm does not upgrade crds/) and the API server
	// drops the field. From then on only the condition, which an old CRD keeps, is
	// compared, so the state is one write and not one per ClusterPersona per cycle.
	fieldPruned atomic.Bool
}

// NewCPUThrottlingStatusSink builds the sink over a client that can list and update
// ClusterPersona status.
func NewCPUThrottlingStatusSink(c client.Client, logger logr.Logger) *CPUThrottlingStatusSink {
	return &CPUThrottlingStatusSink{client: c, logger: logger.WithName("cpu-throttling-status")}
}

var _ detection.CPUThrottlingStatusSink = (*CPUThrottlingStatusSink)(nil)

// ReportCPUThrottlingSource implements detection.CPUThrottlingStatusSink.
func (s *CPUThrottlingStatusSink) ReportCPUThrottlingSource(ctx context.Context, status detection.SourceStatus) {
	var list dorguv1.ClusterPersonaList
	if err := s.client.List(ctx, &list); err != nil {
		s.logFailure(err, "could not list ClusterPersonas to report the CPU throttling detection state")
		return
	}
	for i := range list.Items {
		name := list.Items[i].Name
		if !cpuStatusDiffers(&list.Items[i], status, s.fieldPruned.Load()) {
			continue
		}
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var fresh dorguv1.ClusterPersona
			if err := s.client.Get(ctx, client.ObjectKey{Name: name}, &fresh); err != nil {
				return err
			}
			if !cpuStatusDiffers(&fresh, status, s.fieldPruned.Load()) {
				return nil
			}
			applyCPUStatus(&fresh, status)
			if err := s.client.Status().Update(ctx, &fresh); err != nil {
				return err
			}
			// Update fills fresh from the API server's answer. A field that was set and
			// is not there was pruned.
			if fresh.Status.CPUThrottlingDetection == nil && !s.fieldPruned.Swap(true) {
				s.logger.Error(errOldCRD, "the ClusterPersona CustomResourceDefinition in this cluster has no status.cpuThrottlingDetection, "+
					"so `dorgu health` cannot show the CPU throttling detection state (and spec.metrics.prometheus.url is ignored). "+
					"Helm does not upgrade crds/: apply the CRDs from this release with kubectl apply -f charts/dorgu-operator/crds",
					"clusterPersona", name)
			}
			return nil
		})
		if err != nil {
			s.logFailure(err, "could not write the CPU throttling detection state to the ClusterPersona", "clusterPersona", name)
		}
	}
}

// desiredCPUStatus is the field the sink writes for a SourceStatus.
func desiredCPUStatus(status detection.SourceStatus) *dorguv1.CPUThrottlingDetectionStatus {
	return &dorguv1.CPUThrottlingDetectionStatus{
		Enabled: status.Active,
		Source:  status.Source,
		Reason:  status.Reason,
		Message: status.Message,
	}
}

func conditionStatusFor(status detection.SourceStatus) metav1.ConditionStatus {
	if status.Active {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// errOldCRD is the cause logged when the API server prunes the status field.
var errOldCRD = errors.New("status.cpuThrottlingDetection was dropped by the API server")

// cpuStatusDiffers reports whether the persona's field or condition is not what the
// status says it should be. ignoreField skips the field, for a CRD that cannot hold it.
func cpuStatusDiffers(cp *dorguv1.ClusterPersona, status detection.SourceStatus, ignoreField bool) bool {
	have := cp.Status.CPUThrottlingDetection
	if !ignoreField && (have == nil || *have != *desiredCPUStatus(status)) {
		return true
	}
	cond := apimeta.FindStatusCondition(cp.Status.Conditions, ConditionCPUThrottlingDetection)
	return cond == nil || cond.Status != conditionStatusFor(status) ||
		cond.Reason != status.Reason || cond.Message != status.Message
}

func applyCPUStatus(cp *dorguv1.ClusterPersona, status detection.SourceStatus) {
	cp.Status.CPUThrottlingDetection = desiredCPUStatus(status)
	upsertCondition(&cp.Status.Conditions, ConditionCPUThrottlingDetection,
		conditionStatusFor(status), status.Reason, status.Message)
}

// logFailure logs at most once per statusLogInterval and counts what it held back.
func (s *CPUThrottlingStatusSink) logFailure(err error, msg string, kv ...any) {
	s.mu.Lock()
	now := time.Now()
	if !s.lastLog.IsZero() && now.Sub(s.lastLog) < statusLogInterval {
		s.held++
		s.mu.Unlock()
		return
	}
	held := s.held
	s.lastLog, s.held = now, 0
	s.mu.Unlock()

	if held > 0 {
		kv = append(kv, "suppressedSinceLastLog", held)
	}
	s.logger.Error(err, msg, kv...)
}

// PrometheusURLFromClusterPersona returns the function the Prometheus source reads its
// base URL through: spec.metrics.prometheus.url of the first ClusterPersona (by name)
// that sets one, or "" when none does. It is read every cycle, so editing the
// ClusterPersona takes effect without a restart. A list failure is an error, not an
// empty answer: an unreadable policy is not "no Prometheus".
func PrometheusURLFromClusterPersona(c client.Reader) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		var list dorguv1.ClusterPersonaList
		if err := c.List(ctx, &list); err != nil {
			return "", fmt.Errorf("listing ClusterPersonas: %w", err)
		}
		items := slices.Clone(list.Items)
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		for i := range items {
			if m := items[i].Spec.Metrics; m != nil && m.Prometheus != nil && m.Prometheus.URL != "" {
				return m.Prometheus.URL, nil
			}
		}
		return "", nil
	}
}
