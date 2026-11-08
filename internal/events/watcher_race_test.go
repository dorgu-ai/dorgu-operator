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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// errorCapture is a logr sink that remembers the messages logged at ERROR.
type errorCapture struct {
	mu     sync.Mutex
	errors []string
}

func (e *errorCapture) Init(logr.RuntimeInfo)          {}
func (e *errorCapture) Enabled(int) bool               { return true }
func (e *errorCapture) Info(int, string, ...any)       {}
func (e *errorCapture) WithValues(...any) logr.LogSink { return e }
func (e *errorCapture) WithName(string) logr.LogSink   { return e }
func (e *errorCapture) Error(_ error, msg string, _ ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.errors = append(e.errors, msg)
}

// F-10: an event stored against a namespace that was deleted mid-cycle used to
// log `failed to store event ... creating DorguEvent ... not found` at ERROR.
func TestWatcher_StoreRaceIsNotAnError(t *testing.T) {
	gone := apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "crash-nopersona")
	event := &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Name: "e"},
		Reason:        "OOMKilling",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}
	tests := []struct {
		name      string
		err       error
		wantError bool
	}{
		{"namespace deleted mid-cycle", errors.Join(errors.New("creating DorguEvent"), gone), false},
		{"conflict", apierrors.NewConflict(schema.GroupResource{Resource: "dorguevents"}, "e", errors.New("modified")), false},
		{"a real failure", errors.New("etcd is on fire"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := &errorCapture{}
			w := &Watcher{
				classifier:  &mockClassifier{result: &InternalEvent{ID: "id", Severity: SeverityCritical, Category: CategoryHealth, Source: "kubelet"}},
				correlator:  &mockCorrelator{},
				store:       &mockStore{err: tt.err},
				logger:      logr.New(capture),
				startupTime: time.Now(),
			}

			w.processEvent(context.Background(), event)

			if tt.wantError {
				assert.Contains(t, capture.errors, "failed to store event")
			} else {
				assert.Empty(t, capture.errors)
			}
		})
	}
}
