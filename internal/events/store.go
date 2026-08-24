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
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultCacheSize is the default number of events in the in-memory cache.
	DefaultCacheSize = 500

	// DefaultDedupWindow is the default deduplication window.
	DefaultDedupWindow = 5 * time.Minute

	// DefaultTTL is the default event TTL.
	DefaultTTL = 24 * time.Hour

	// MaxK8sNameLength is the maximum length for K8s resource names.
	MaxK8sNameLength = 253
)

// EventStore provides hybrid persistence: in-memory LRU cache + DorguEvent CRD.
type EventStore interface {
	// Store persists an event (both in-memory and as CRD).
	Store(ctx context.Context, event *InternalEvent) error

	// Query returns events matching the filter.
	Query(ctx context.Context, filter EventFilter) ([]InternalEvent, error)

	// Count returns the number of events matching the filter.
	Count(ctx context.Context, filter EventFilter) (int, error)
}

// HybridEventStore implements EventStore with in-memory cache and CRD persistence.
type HybridEventStore struct {
	client      client.Client
	logger      logr.Logger
	cacheSize   int
	dedupWindow time.Duration
	ttl         time.Duration

	mu       sync.RWMutex
	cache    []InternalEvent
	dedupMap map[string]time.Time // key: dedup key → last seen time
}

// StoreOption configures the HybridEventStore.
type StoreOption func(*HybridEventStore)

// WithCacheSize sets the cache size.
func WithCacheSize(size int) StoreOption {
	return func(s *HybridEventStore) { s.cacheSize = size }
}

// WithDedupWindow sets the deduplication window.
func WithDedupWindow(d time.Duration) StoreOption {
	return func(s *HybridEventStore) { s.dedupWindow = d }
}

// WithTTL sets the retention stamped on every DorguEvent the store writes, so
// the record states the retention the operator is actually running with rather
// than a compiled-in default the cleaner may not be using.
func WithTTL(d time.Duration) StoreOption {
	return func(s *HybridEventStore) { s.ttl = d }
}

// NewEventStore creates a new HybridEventStore.
func NewEventStore(c client.Client, logger logr.Logger, opts ...StoreOption) *HybridEventStore {
	s := &HybridEventStore{
		client:      c,
		logger:      logger,
		cacheSize:   DefaultCacheSize,
		dedupWindow: DefaultDedupWindow,
		ttl:         DefaultTTL,
		cache:       make([]InternalEvent, 0, DefaultCacheSize),
		dedupMap:    make(map[string]time.Time),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Store persists an event. Returns nil if the event is deduplicated.
func (s *HybridEventStore) Store(ctx context.Context, event *InternalEvent) error {
	if event == nil {
		return nil
	}

	// Atomic deduplication check-and-set.
	dedupKey := s.dedupKey(event)
	if s.checkAndMarkSeen(dedupKey) {
		s.logger.V(1).Info("event deduplicated", "id", event.ID, "key", dedupKey)
		return nil
	}

	// Write to in-memory cache.
	s.addToCache(*event)

	// Persist as DorguEvent CRD. The error is returned, not logged: every caller
	// already logs it, and logging here as well printed each failure twice, at
	// two layers, which is half of what made F-06's 171 ERROR lines unreadable.
	if err := s.persistCRD(ctx, event); err != nil {
		return fmt.Errorf("persisting DorguEvent CRD: %w", err)
	}

	return nil
}

// Query returns events matching the filter. Reads from cache first, falls back to K8s API.
func (s *HybridEventStore) Query(ctx context.Context, filter EventFilter) ([]InternalEvent, error) {
	// Try cache first.
	results := s.queryCache(filter)
	if len(results) > 0 || s.cacheCoversFilter(filter) {
		return applyLimit(results, filter.Limit), nil
	}

	// Fall back to K8s API.
	return s.queryAPI(ctx, filter)
}

// Count returns the number of events matching the filter.
func (s *HybridEventStore) Count(ctx context.Context, filter EventFilter) (int, error) {
	events, err := s.Query(ctx, filter)
	if err != nil {
		return 0, err
	}
	return len(events), nil
}

// dedupKey identifies what a record is about. The reason is part of that
// identity: without it, two unrelated things happening to one object inside one
// category were the same event, so whichever arrived second was suppressed. On
// an IncidentMemory that meant an ordinary health event could swallow the
// DorguDiagnosisDiscarded record the docs tell users to alert on (F-06).
func (s *HybridEventStore) dedupKey(event *InternalEvent) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		event.Source,
		event.InvolvedObject.Namespace,
		event.InvolvedObject.Name,
		event.Category,
		event.Reason,
	)
}

// checkAndMarkSeen atomically checks if a key is a duplicate and marks it as seen.
// Returns true if the event is a duplicate (should be suppressed).
func (s *HybridEventStore) checkAndMarkSeen(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if lastSeen, exists := s.dedupMap[key]; exists {
		if time.Since(lastSeen) < s.dedupWindow {
			return true
		}
	}

	s.dedupMap[key] = time.Now()

	// Clean up old dedup entries periodically.
	if len(s.dedupMap) > s.cacheSize*2 {
		now := time.Now()
		for k, t := range s.dedupMap {
			if now.Sub(t) > s.dedupWindow {
				delete(s.dedupMap, k)
			}
		}
	}

	return false
}

func (s *HybridEventStore) addToCache(event InternalEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cache = append(s.cache, event)

	// Evict oldest entries when cache exceeds size.
	if len(s.cache) > s.cacheSize {
		s.cache = s.cache[len(s.cache)-s.cacheSize:]
	}
}

func (s *HybridEventStore) queryCache(filter EventFilter) []InternalEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []InternalEvent
	for _, event := range s.cache {
		if matchesFilter(event, filter) {
			results = append(results, event)
		}
	}
	return results
}

// cacheCoversFilter returns true if the cache has enough data to answer the query
// without falling back to the API. Currently conservative — only returns true
// if the cache is not empty.
func (s *HybridEventStore) cacheCoversFilter(filter EventFilter) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if filter.Since == nil {
		return false
	}

	// If the cache is empty, can't cover anything.
	if len(s.cache) == 0 {
		return false
	}

	// If the oldest event in cache is newer than the filter's Since,
	// we might be missing older events — need to go to API.
	oldest := s.cache[0].EventTime
	return !oldest.After(*filter.Since)
}

func (s *HybridEventStore) queryAPI(ctx context.Context, filter EventFilter) ([]InternalEvent, error) {
	var list dorguv1.DorguEventList
	opts := []client.ListOption{}

	if filter.Namespace != "" {
		opts = append(opts, client.InNamespace(filter.Namespace))
	}

	if err := s.client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing DorguEvents: %w", err)
	}

	var results []InternalEvent
	for _, de := range list.Items {
		event := crdToInternalEvent(de)
		if matchesFilter(event, filter) {
			results = append(results, event)
		}
	}

	return applyLimit(results, filter.Limit), nil
}

func (s *HybridEventStore) persistCRD(ctx context.Context, event *InternalEvent) error {
	name := generateCRDName(event)
	namespace := event.InvolvedObject.Namespace
	if namespace == "" {
		namespace = "default"
	}

	ttl := metav1.Duration{Duration: s.ttl}

	de := &dorguv1.DorguEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: dorguv1.DorguEventSpec{
			Severity: string(event.Severity),
			Category: string(event.Category),
			Source:   event.Source,
			Message:  event.Message,
			InvolvedObject: dorguv1.ResourceReference{
				Kind:      event.InvolvedObject.Kind,
				Name:      event.InvolvedObject.Name,
				Namespace: event.InvolvedObject.Namespace,
			},
			PersonaRef:  event.PersonaRef,
			EventTime:   metav1.Time{Time: event.EventTime},
			K8sEventRef: event.K8sEventUID,
			TTL:         &ttl,
		},
	}

	if err := s.client.Create(ctx, de); err != nil {
		// AlreadyExists is the expected steady state, not a failure. Names are
		// content-addressed over the involved object, category, reason and event
		// time, so an object already under this name is this same record from an
		// earlier delivery: the write being reported has already succeeded. The
		// informer re-lists everything it holds on each resync, and the resync
		// period is the same 5 minutes as the dedup window, so a settled event
		// comes back round just as its dedup entry expires. Returning that as an
		// error cost 171 ERROR lines in 20 minutes and, worse, aborted the
		// pipeline before the emit step (F-06).
		if apierrors.IsAlreadyExists(err) {
			s.logger.V(1).Info("DorguEvent already recorded, skipping duplicate write",
				"name", name, "namespace", namespace)
			return nil
		}
		return fmt.Errorf("creating DorguEvent %s/%s: %w", namespace, name, err)
	}

	s.logger.V(1).Info("persisted DorguEvent", "name", name, "namespace", namespace)
	return nil
}

// generateCRDName creates a deterministic CRD name for deduplication.
// Format: de-{namespace}-{kind}-{name}-{hash(category+reason+eventTime)}
//
// The reason is in the hash because event times have second granularity: two
// different things happening to one object in one category within the same
// second produced the same name, and the second one was lost.
func generateCRDName(event *InternalEvent) string {
	hashInput := fmt.Sprintf("%s/%s/%s",
		event.Category, event.Reason, event.EventTime.Format(time.RFC3339Nano))
	hash := sha256.Sum256([]byte(hashInput))
	hashStr := fmt.Sprintf("%x", hash[:6])

	name := fmt.Sprintf("de-%s-%s-%s-%s",
		event.InvolvedObject.Namespace,
		sanitizeK8sName(event.InvolvedObject.Kind),
		sanitizeK8sName(event.InvolvedObject.Name),
		hashStr,
	)

	if len(name) > MaxK8sNameLength {
		name = name[:MaxK8sNameLength]
	}
	return name
}

func sanitizeK8sName(s string) string {
	result := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32) // lowercase
		} else {
			result = append(result, '-')
		}
	}
	return string(result)
}

func matchesFilter(event InternalEvent, filter EventFilter) bool {
	if filter.Namespace != "" && event.InvolvedObject.Namespace != filter.Namespace {
		return false
	}
	if filter.Severity != "" && event.Severity != filter.Severity {
		return false
	}
	if filter.Category != "" && event.Category != filter.Category {
		return false
	}
	if filter.Since != nil && event.EventTime.Before(*filter.Since) {
		return false
	}
	if filter.PersonaRef != nil && event.PersonaRef != nil {
		if event.PersonaRef.Kind != filter.PersonaRef.Kind ||
			event.PersonaRef.Name != filter.PersonaRef.Name {
			return false
		}
	}
	return true
}

func crdToInternalEvent(de dorguv1.DorguEvent) InternalEvent {
	return InternalEvent{
		ID:       de.Name,
		Severity: Severity(de.Spec.Severity),
		Category: Category(de.Spec.Category),
		Source:   de.Spec.Source,
		Message:  de.Spec.Message,
		InvolvedObject: dorguv1.ResourceReference{
			Kind:      de.Spec.InvolvedObject.Kind,
			Name:      de.Spec.InvolvedObject.Name,
			Namespace: de.Spec.InvolvedObject.Namespace,
		},
		PersonaRef:  de.Spec.PersonaRef,
		EventTime:   de.Spec.EventTime.Time,
		K8sEventUID: de.Spec.K8sEventRef,
	}
}

func applyLimit(events []InternalEvent, limit int) []InternalEvent {
	if limit <= 0 || limit >= len(events) {
		return events
	}
	return events[:limit]
}
