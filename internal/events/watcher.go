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

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// DefaultStartupWindow filters out events older than this on startup.
	DefaultStartupWindow = 10 * time.Minute

	// DefaultResyncPeriod is how often the informer re-lists.
	DefaultResyncPeriod = 5 * time.Minute
)

// Watcher watches K8s Events and processes them through the pipeline:
// classify → correlate → store → emit.
// It implements manager.Runnable for lifecycle management.
type Watcher struct {
	clientset    kubernetes.Interface
	classifier   Classifier
	correlator   Correlator
	store        EventStore
	emitter      Emitter
	logger       logr.Logger
	resyncPeriod time.Duration
	startupTime  time.Time
}

// WatcherOption configures the Watcher.
type WatcherOption func(*Watcher)

// WithResyncPeriod sets the informer resync period.
func WithResyncPeriod(d time.Duration) WatcherOption {
	return func(w *Watcher) { w.resyncPeriod = d }
}

// NewWatcher creates a new event Watcher.
func NewWatcher(
	clientset kubernetes.Interface,
	classifier Classifier,
	correlator Correlator,
	store EventStore,
	emitter Emitter,
	logger logr.Logger,
	opts ...WatcherOption,
) *Watcher {
	w := &Watcher{
		clientset:    clientset,
		classifier:   classifier,
		correlator:   correlator,
		store:        store,
		emitter:      emitter,
		logger:       logger.WithName("event-watcher"),
		resyncPeriod: DefaultResyncPeriod,
		startupTime:  time.Now(),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Start begins watching K8s Events. It blocks until the context is cancelled.
// Implements manager.Runnable.
func (w *Watcher) Start(ctx context.Context) error {
	w.startupTime = time.Now()
	w.logger.Info("starting event watcher")

	factory := informers.NewSharedInformerFactory(w.clientset, w.resyncPeriod)
	informer := factory.Core().V1().Events().Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			event, ok := obj.(*corev1.Event)
			if !ok {
				return
			}
			w.processEvent(ctx, event)
		},
		UpdateFunc: func(_, newObj interface{}) {
			event, ok := newObj.(*corev1.Event)
			if !ok {
				return
			}
			w.processEvent(ctx, event)
		},
	})
	if err != nil {
		return err
	}

	factory.Start(ctx.Done())

	// Wait for cache sync.
	factory.WaitForCacheSync(ctx.Done())
	w.logger.Info("event watcher cache synced")

	// Block until context is cancelled.
	<-ctx.Done()
	w.logger.Info("event watcher stopped")
	return nil
}

// processEvent runs the event through the pipeline: classify → correlate → store → emit.
func (w *Watcher) processEvent(ctx context.Context, event *corev1.Event) {
	// Skip old events on startup to avoid replaying history.
	if w.isOldEvent(event) {
		return
	}

	// Step 1: Classify.
	internal := w.classifier.Classify(event)
	if internal == nil {
		return // Event was discarded by classifier.
	}

	// Step 2: Correlate to Persona.
	if err := w.correlator.Correlate(ctx, internal); err != nil {
		w.logger.Error(err, "failed to correlate event",
			"event", event.Name,
			"reason", event.Reason,
		)
		// Continue processing even if correlation fails.
	}

	// Step 3: Store.
	if err := w.store.Store(ctx, internal); err != nil {
		w.logger.Error(err, "failed to store event",
			"event", event.Name,
			"reason", event.Reason,
		)
		return
	}

	// Step 4: Emit K8s Event from operator.
	if w.emitter != nil {
		if err := w.emitter.Emit(ctx, internal); err != nil {
			w.logger.Error(err, "failed to emit event",
				"event", event.Name,
				"reason", event.Reason,
			)
		}
	}

	w.logger.V(1).Info("processed event",
		"reason", event.Reason,
		"severity", internal.Severity,
		"category", internal.Category,
		"involvedObject", internal.InvolvedObject.Name,
	)
}

// isOldEvent returns true if the event occurred before the startup window.
func (w *Watcher) isOldEvent(event *corev1.Event) bool {
	eventTime := event.LastTimestamp.Time
	if eventTime.IsZero() {
		eventTime = event.CreationTimestamp.Time
	}
	if eventTime.IsZero() {
		return false // Can't determine age; process it.
	}

	cutoff := w.startupTime.Add(-DefaultStartupWindow)
	return eventTime.Before(cutoff)
}
