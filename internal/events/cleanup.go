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
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultCleanupInterval is how often the cleaner runs.
	DefaultCleanupInterval = 30 * time.Minute

	// DefaultCleanupBatchSize is the max number of events deleted per cycle.
	DefaultCleanupBatchSize = 50
)

// Cleaner periodically removes expired DorguEvent CRDs.
// It implements manager.Runnable for lifecycle management.
type Cleaner struct {
	client    client.Client
	interval  time.Duration
	batchSize int
	logger    logr.Logger
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

// NewCleaner creates a new Cleaner.
func NewCleaner(c client.Client, logger logr.Logger, opts ...CleanerOption) *Cleaner {
	cleaner := &Cleaner{
		client:    c,
		interval:  DefaultCleanupInterval,
		batchSize: DefaultCleanupBatchSize,
		logger:    logger.WithName("event-cleaner"),
	}
	for _, opt := range opts {
		opt(cleaner)
	}
	return cleaner
}

// Start begins the periodic cleanup loop. It blocks until the context is cancelled.
// Implements manager.Runnable.
func (c *Cleaner) Start(ctx context.Context) error {
	c.logger.Info("starting DorguEvent cleanup", "interval", c.interval)

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

// cleanup runs a single cleanup cycle, deleting expired DorguEvent CRDs.
func (c *Cleaner) cleanup(ctx context.Context) error {
	var list dorguv1.DorguEventList
	if err := c.client.List(ctx, &list); err != nil {
		return err
	}

	now := time.Now()
	deleted := 0

	for i := range list.Items {
		if deleted >= c.batchSize {
			c.logger.Info("batch size reached, remaining expired events will be cleaned next cycle",
				"batchSize", c.batchSize,
			)
			break
		}

		de := &list.Items[i]
		if isExpired(de, now) {
			if err := c.client.Delete(ctx, de); err != nil {
				c.logger.Error(err, "failed to delete expired DorguEvent",
					"name", de.Name,
					"namespace", de.Namespace,
				)
				continue
			}
			deleted++
		}
	}

	if deleted > 0 {
		c.logger.Info("cleaned up expired DorguEvents", "count", deleted)
	}

	return nil
}

// isExpired returns true if the DorguEvent has exceeded its TTL.
func isExpired(de *dorguv1.DorguEvent, now time.Time) bool {
	ttl := DefaultTTL
	if de.Spec.TTL != nil {
		ttl = de.Spec.TTL.Duration
	}

	expiry := de.Spec.EventTime.Time.Add(ttl)
	return now.After(expiry)
}
