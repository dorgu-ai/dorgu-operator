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

// cpuFieldRecheckInterval is how long the sink goes on ignoring status.cpuThrottlingDetection
// after the API server pruned it. When it elapses the field is written once more: if it
// sticks the CRD has been upgraded and the sink compares the field again, with no restart.
const cpuFieldRecheckInterval = 5 * time.Minute

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

	// fieldPrunedAt is when (unix nanoseconds) a write last came back without
	// status.cpuThrottlingDetection, or 0: the installed CRD predates it (Helm does not
	// upgrade crds/) and the API server drops the field. For cpuFieldRecheckInterval
	// after that only the condition, which an old CRD keeps, is compared, so the state is
	// one write and not one per ClusterPersona per cycle. Then the field is tried again,
	// so applying the new CRDs needs no restart.
	fieldPrunedAt atomic.Int64

	now func() time.Time
}

// NewCPUThrottlingStatusSink builds the sink over a client that can list and update
// ClusterPersona status.
func NewCPUThrottlingStatusSink(c client.Client, logger logr.Logger) *CPUThrottlingStatusSink {
	return &CPUThrottlingStatusSink{client: c, logger: logger.WithName("cpu-throttling-status"), now: time.Now}
}

// ignoringField is true while the field is known to be pruned and the re-check is not due.
func (s *CPUThrottlingStatusSink) ignoringField() bool {
	at := s.fieldPrunedAt.Load()
	return at != 0 && s.now().Sub(time.Unix(0, at)) < cpuFieldRecheckInterval
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
		if !cpuStatusDiffers(&list.Items[i], status, s.ignoringField()) {
			continue
		}
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var fresh dorguv1.ClusterPersona
			if err := s.client.Get(ctx, client.ObjectKey{Name: name}, &fresh); err != nil {
				return err
			}
			ignoring := s.ignoringField()
			if !cpuStatusDiffers(&fresh, status, ignoring) {
				return nil
			}
			applyCPUStatus(&fresh, status)
			if err := s.client.Status().Update(ctx, &fresh); err != nil {
				return err
			}
			// Update fills fresh from the API server's answer. A field that was set and
			// is not there was pruned; one that is there means the CRD can hold it.
			s.noteField(fresh.Status.CPUThrottlingDetection != nil, ignoring, name)
			return nil
		})
		if err != nil {
			s.logFailure(err, "could not write the CPU throttling detection state to the ClusterPersona", "clusterPersona", name)
		}
	}
}

// noteField records what a status write showed about the CRD. The first time the field
// comes back pruned, and again once per cpuFieldRecheckInterval while it keeps coming
// back pruned, the log says what to do: it is the operator's only way to say that
// `dorgu health` is blind to this state.
func (s *CPUThrottlingStatusSink) noteField(kept, wasIgnoring bool, persona string) {
	if kept {
		s.fieldPrunedAt.Store(0)
		return
	}
	if wasIgnoring {
		return
	}
	s.fieldPrunedAt.Store(s.now().UnixNano())
	s.logger.Error(errOldCRD, "the ClusterPersona CustomResourceDefinition in this cluster has no status.cpuThrottlingDetection, "+
		"so `dorgu health` cannot show the CPU throttling detection state (and spec.metrics.prometheus.url is ignored). "+
		"Helm does not upgrade crds/: apply the CRDs from this release with kubectl apply -f charts/dorgu-operator/crds. "+
		"The operator re-checks every 5 minutes, so no restart is needed once they are applied",
		"clusterPersona", persona)
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
