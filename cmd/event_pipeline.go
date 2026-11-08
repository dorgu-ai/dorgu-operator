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

package main

import (
	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/dorgu-ai/dorgu-operator/internal/events"
)

// runnableAdder is the part of a controller-runtime Manager the event pipeline
// needs, so a test can see what would be started without a cluster.
type runnableAdder interface {
	Add(manager.Runnable) error
}

// eventPipeline is the event machinery the operator may run: a watcher that
// turns Kubernetes Events into DorguEvent records, a cleaner that bounds them,
// and the store the health cycle also writes to.
//
// Nothing reads DorguEvent records yet, so the pipeline is off unless
// --enable-event-pipeline is set (harness D17). With it off, Store, Watcher and
// Cleaner are nil: nothing is started and nothing writes a record that no
// cleaner would prune. Emitter is always present. It posts the Kubernetes Event
// on an incident that `kubectl describe` shows, which needs no storage.
type eventPipeline struct {
	Emitter events.Emitter

	// Store, Watcher and Cleaner are nil unless the pipeline is enabled. Store is
	// a nil interface, not a typed nil, so callers can compare it to nil.
	Store   events.EventStore
	Watcher *events.Watcher
	Cleaner *events.Cleaner
}

// buildEventPipeline assembles the pipeline from the operator configuration.
func buildEventPipeline(
	cfg operatorConfig,
	c client.Client,
	clientset kubernetes.Interface,
	recorder record.EventRecorder,
	log logr.Logger,
) eventPipeline {
	emitter := events.NewEmitter(recorder, log)
	if !cfg.enableEventPipeline {
		return eventPipeline{Emitter: emitter}
	}

	store := events.NewEventStore(c, log, events.WithTTL(cfg.dorguEventRetention))
	return eventPipeline{
		Emitter: emitter,
		Store:   store,
		Watcher: events.NewWatcher(clientset, events.NewClassifier(), events.NewCorrelator(c), store, emitter, log),
		Cleaner: events.NewCleaner(c, log,
			events.WithRetention(cfg.dorguEventRetention),
			events.WithMaxRecords(cfg.dorguEventMaxRecords)),
	}
}

// register adds whichever runnables the pipeline has to the manager.
func (p eventPipeline) register(mgr runnableAdder) error {
	if p.Watcher != nil {
		if err := mgr.Add(p.Watcher); err != nil {
			return err
		}
	}
	if p.Cleaner != nil {
		if err := mgr.Add(p.Cleaner); err != nil {
			return err
		}
	}
	return nil
}
