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

package llm

import (
	"sync"
	"time"
)

// DefaultCacheTTL is how long an answer is reused while a group's signal
// fingerprint stays the same. The health cycle runs every minute, so without a
// cache an incident that stays open costs a call per cycle (KPI Q3, finding H-3).
// A change in the signals re-asks at once; this is only the bound on how stale an
// unchanged explanation may get.
const DefaultCacheTTL = 30 * time.Minute

// FailureTTL is how long a failed or invalid answer suppresses a retry. It is
// deliberately shorter than a success: a provider outage should be retried, but
// not once a minute for every open incident.
const FailureTTL = 5 * time.Minute

// DefaultCacheEntries bounds a Cache. Entries are one per open incident
// fingerprint, so this is far above any real cluster and exists to cap memory.
const DefaultCacheEntries = 1024

type cacheEntry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a TTL cache keyed by a signal fingerprint. It is safe for concurrent
// use. A zero or negative ttl disables caching: Get never hits.
type Cache[V any] struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	items      map[string]cacheEntry[V]
}

// NewCache creates a cache whose entries live for ttl, holding at most
// maxEntries (DefaultCacheEntries when not positive).
func NewCache[V any](ttl time.Duration, maxEntries int) *Cache[V] {
	if maxEntries <= 0 {
		maxEntries = DefaultCacheEntries
	}
	return &Cache[V]{
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
		items:      map[string]cacheEntry[V]{},
	}
}

// SetClock replaces the time source, for tests.
func (c *Cache[V]) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// TTL returns the lifetime of a success entry.
func (c *Cache[V]) TTL() time.Duration { return c.ttl }

// Enabled reports whether the cache stores anything.
func (c *Cache[V]) Enabled() bool { return c.ttl > 0 }

// Get returns the live entry for key.
func (c *Cache[V]) Get(key string) (V, bool) {
	var zero V
	if !c.Enabled() {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.items[key]
	if !ok {
		return zero, false
	}
	if !c.now().Before(e.expires) {
		delete(c.items, key)
		return zero, false
	}
	return e.value, true
}

// Set stores value for the cache's ttl.
func (c *Cache[V]) Set(key string, value V) { c.SetFor(key, value, c.ttl) }

// SetFor stores value for ttl, capped at the cache's own ttl so a failure entry
// can never outlive a success. A disabled cache stores nothing.
func (c *Cache[V]) SetFor(key string, value V, ttl time.Duration) {
	if !c.Enabled() {
		return
	}
	if ttl <= 0 || ttl > c.ttl {
		ttl = c.ttl
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if _, exists := c.items[key]; !exists && len(c.items) >= c.maxEntries {
		c.evictLocked(now)
	}
	c.items[key] = cacheEntry[V]{value: value, expires: now.Add(ttl)}
}

// evictLocked makes room: expired entries first, then the one closest to expiry.
func (c *Cache[V]) evictLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for k, e := range c.items {
		if !now.Before(e.expires) {
			delete(c.items, k)
			continue
		}
		if oldestKey == "" || e.expires.Before(oldest) {
			oldestKey, oldest = k, e.expires
		}
	}
	if len(c.items) >= c.maxEntries && oldestKey != "" {
		delete(c.items, oldestKey)
	}
}

// Len returns the number of stored entries, expired ones included.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
