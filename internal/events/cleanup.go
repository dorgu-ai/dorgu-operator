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

package events

import (
	"context"
	"fmt"
	"slices"
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultCleanupInterval is how often the cleaner runs.
	DefaultCleanupInterval = 30 * time.Minute

	// DefaultCleanupBatchSize is the max number of events deleted per cycle. It
	// has to clear more than arrives between cycles or the backlog never drains:
	// a five-app cluster produced roughly 2,700 DorguEvents a day, about 60 per
	// half hour, and a large cluster produces proportionally more.
	DefaultCleanupBatchSize = 200

	// DefaultMaxRecords caps how many DorguEvents are kept regardless of age.
	// A TTL bounds age, not count, and the arrival rate scales with the cluster,
	// so a 24-hour window on a large cluster is still unbounded growth in etcd:
	// 187 records in 100 minutes on five apps, with nothing to stop it (F-10).
	DefaultMaxRecords = 2000
)

// Cleaner periodically prunes DorguEvent CRDs, by age and by count.
// It implements manager.Runnable for lifecycle management.
type Cleaner struct {
	client     client.Client
	interval   time.Duration
	batchSize  int
	retention  time.Duration
	maxRecords int
	logger     logr.Logger
}

// CleanerOption configures the Cleaner.
type CleanerOption func(*Cleaner)

// WithCleanupInterval sets the cleanup interval.
func WithCleanupInterval(d time.Duration) CleanerOption {
	return func(c *Cleaner) { c.interval = d }
}

// WithBatchSize sets the deletion batch size.
func WithBatchSize(size int) CleanerOption {
	return func(c *Cleaner) { c.batchSize = size }
}

// WithRetention sets how long a DorguEvent is kept when it carries no TTL of its
// own. Records written with an explicit spec.TTL keep it, since a per-record TTL
// is a deliberate override.
func WithRetention(d time.Duration) CleanerOption {
	return func(c *Cleaner) { c.retention = d }
}

// WithMaxRecords caps the number of DorguEvents kept regardless of age; the
// oldest go first. A value of 0 or less disables the cap and leaves retention as
// the only bound.
func WithMaxRecords(n int) CleanerOption {
	return func(c *Cleaner) { c.maxRecords = n }
}

// NewCleaner creates a new Cleaner.
func NewCleaner(c client.Client, logger logr.Logger, opts ...CleanerOption) *Cleaner {
	cleaner := &Cleaner{
		client:     c,
		interval:   DefaultCleanupInterval,
		batchSize:  DefaultCleanupBatchSize,
		retention:  DefaultTTL,
		maxRecords: DefaultMaxRecords,
		logger:     logger.WithName("event-cleaner"),
	}
	for _, opt := range opts {
		opt(cleaner)
	}
	return cleaner
}

// Start begins the periodic cleanup loop. It blocks until the context is cancelled.
// Implements manager.Runnable.
func (c *Cleaner) Start(ctx context.Context) error {
	c.logger.Info("starting DorguEvent cleanup",
		"interval", c.interval,
		"retention", c.retention,
		"maxRecords", c.maxRecords,
	)

	// Prune once before waiting on the ticker. A restarted operator would
	// otherwise run a full interval carrying whatever backlog the previous
	// process left behind, which is exactly when the backlog is largest.
	if err := c.cleanup(ctx); err != nil {
		c.logger.Error(err, "startup cleanup cycle failed")
	}

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("DorguEvent cleanup stopped")
			return nil
		case <-ticker.C:
			if err := c.cleanup(ctx); err != nil {
				c.logger.Error(err, "cleanup cycle failed")
			}
		}
	}
}

// cleanup runs a single cleanup cycle. Records past their TTL go first, then the
// oldest survivors if the count is still over the cap.
func (c *Cleaner) cleanup(ctx context.Context) error {
	var list dorguv1.DorguEventList
	if err := c.client.List(ctx, &list); err != nil {
		return fmt.Errorf("listing DorguEvents: %w", err)
	}

	expired, surplus := c.selectForDeletion(list.Items, time.Now())

	doomed := make([]*dorguv1.DorguEvent, 0, len(expired)+len(surplus))
	doomed = append(doomed, expired...)
	doomed = append(doomed, surplus...)

	deferred := 0
	if len(doomed) > c.batchSize {
		deferred = len(doomed) - c.batchSize
		doomed = doomed[:c.batchSize]
	}

	deleted := 0
	for _, de := range doomed {
		// A record another cycle or another replica already removed is done, not
		// a failure.
		if err := c.client.Delete(ctx, de); err != nil && !apierrors.IsNotFound(err) {
			c.logger.Error(err, "failed to delete DorguEvent",
				"name", de.Name,
				"namespace", de.Namespace,
			)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		c.logger.Info("pruned DorguEvents",
			"deleted", deleted,
			"expired", len(expired),
			"overCap", len(surplus),
			"remaining", len(list.Items)-deleted,
		)
	}
	if deferred > 0 {
		c.logger.Info("batch size reached, the rest will be pruned next cycle",
			"batchSize", c.batchSize,
			"deferred", deferred,
		)
	}

	return nil
}

// selectForDeletion splits live records into those past their TTL and the oldest
// of the survivors that push the count over the cap. The two are kept apart so
// the log can say which bound did the pruning.
func (c *Cleaner) selectForDeletion(
	items []dorguv1.DorguEvent,
	now time.Time,
) (expired, surplus []*dorguv1.DorguEvent) {
	expired = make([]*dorguv1.DorguEvent, 0, len(items))
	kept := make([]*dorguv1.DorguEvent, 0, len(items))

	for i := range items {
		de := &items[i]
		if isExpired(de, c.retention, now) {
			expired = append(expired, de)
			continue
		}
		kept = append(kept, de)
	}

	if c.maxRecords <= 0 || len(kept) <= c.maxRecords {
		return expired, nil
	}

	slices.SortFunc(kept, func(a, b *dorguv1.DorguEvent) int {
		return a.Spec.EventTime.Compare(b.Spec.EventTime.Time)
	})
	return expired, kept[:len(kept)-c.maxRecords]
}

// isExpired returns true if the DorguEvent has exceeded its TTL. A record's own
// spec.TTL wins, because a per-record TTL is a deliberate override; otherwise
// fallbackTTL applies, so changing the operator's retention also reaches records
// written before the change.
func isExpired(de *dorguv1.DorguEvent, fallbackTTL time.Duration, now time.Time) bool {
	ttl := fallbackTTL
	if de.Spec.TTL != nil {
		ttl = de.Spec.TTL.Duration
	}

	expiry := de.Spec.EventTime.Add(ttl)
	return now.After(expiry)
}
