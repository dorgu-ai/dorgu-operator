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
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

// logCapture is a logr sink that remembers what was logged at ERROR, and what
// at V(1), so a test can assert on the level and not just on the text.
type logCapture struct {
	mu     sync.Mutex
	errors []string
	debug  []string
}

func (l *logCapture) Init(logr.RuntimeInfo)          {}
func (l *logCapture) Enabled(int) bool               { return true }
func (l *logCapture) WithValues(...any) logr.LogSink { return l }
func (l *logCapture) WithName(string) logr.LogSink   { return l }

func (l *logCapture) Info(level int, msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if level >= 1 {
		l.debug = append(l.debug, msg)
	}
}

func (l *logCapture) Error(_ error, msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, msg)
}

func newLogCapture() (*logCapture, logr.Logger) {
	c := &logCapture{}
	return c, logr.New(c)
}

var (
	incidentGR = schema.GroupResource{Group: "dorgu.io", Resource: "incidentmemories"}
	personaGR  = schema.GroupResource{Group: "dorgu.io", Resource: "applicationpersonas"}
)

func conflictErr(name string) error {
	return apierrors.NewConflict(incidentGR, name, errors.New("modified"))
}

func incidentFixture(persona string) *dorguv1.IncidentMemory {
	return &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: "im-1", Namespace: "shop"},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef:  dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: persona, Namespace: "shop"},
			Attribution: AttributionPersona,
			Category:    "resource",
			Severity:    "critical",
			Detection:   dorguv1.DetectionInfo{Signal: "OOMKilled"},
		},
		Status: dorguv1.IncidentMemoryStatus{Phase: PhaseDetected, OccurrenceCount: 1},
	}
}

func reconcileIncident(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (ctrl.Result, *logCapture, error) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(noiseScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&dorguv1.IncidentMemory{}, &dorguv1.ApplicationPersona{}).
		WithInterceptorFuncs(funcs).Build()
	capture, log := newLogCapture()
	r := &IncidentController{Client: c, Logger: log}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: "shop", Name: "im-1"},
	})
	return res, capture, err
}

// Z1 F-1: an optimistic-lock conflict writing an IncidentMemory's labels is an
// expected race. It is retried quietly, not returned as a reconcile error (which
// controller-runtime logs at ERROR and counts in
// controller_runtime_reconcile_errors_total).
func TestIncidentController_LabelConflictRequeuesQuietly(t *testing.T) {
	im := incidentFixture("api")
	im.Labels = nil // forces the label write
	res, logs, err := reconcileIncident(t, interceptor.Funcs{
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			return conflictErr(obj.GetName())
		},
	}, im)

	require.NoError(t, err, "a conflict is a race, not a reconcile error")
	assert.True(t, res.RequeueAfter > 0, "the incident must be looked at again")
	assert.Empty(t, logs.errors)
}

// F-10: the same for a status write that keeps conflicting.
func TestIncidentController_StatusConflictRequeuesQuietly(t *testing.T) {
	im := incidentFixture("api")
	res, logs, err := reconcileIncident(t, interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
			return conflictErr(obj.GetName())
		},
	}, im)

	require.NoError(t, err)
	assert.True(t, res.RequeueAfter > 0)
	assert.Empty(t, logs.errors)
}

// F-10: an incident whose persona was deleted under it (the QA cleanup) is a
// NotFound race, logged at V(1). It used to log eight ERROR lines in a second.
func TestIncidentController_PersonaGoneIsNotAnError(t *testing.T) {
	_, logs, err := reconcileIncident(t, interceptor.Funcs{}, incidentFixture("deleted-persona"))

	require.NoError(t, err)
	assert.Empty(t, logs.errors, "a persona deleted under its incidents is expected, not an ERROR")
	assert.Contains(t, logs.debug, "persona gone, nothing to sync")
}

// A real failure keeps its ERROR: quiet is for races only.
func TestIncidentController_RealErrorsStayLoud(t *testing.T) {
	boom := errors.New("etcd is on fire")
	t.Run("label write", func(t *testing.T) {
		im := incidentFixture("api")
		im.Labels = nil
		_, _, err := reconcileIncident(t, interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return boom },
		}, im)
		require.Error(t, err)
	})
	t.Run("persona sync", func(t *testing.T) {
		persona := &dorguv1.ApplicationPersona{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}}
		_, logs, _ := reconcileIncident(t, interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
		}, incidentFixture("api"), persona)
		assert.Contains(t, logs.errors, "failed to sync persona status")
	})
}

// F-10: the ApplicationPersona reconciler's status write races the incident
// controller's. A conflict there requeues; it is not a reconcile error.
func TestApplicationPersona_StatusConflictRequeuesQuietly(t *testing.T) {
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec:       dorguv1.ApplicationPersonaSpec{Name: "api"},
	}
	c := fake.NewClientBuilder().WithScheme(noiseScheme(t)).
		WithObjects(persona).
		WithStatusSubresource(&dorguv1.ApplicationPersona{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(personaGR, obj.GetName(), errors.New("modified"))
			},
		}).Build()
	r := &ApplicationPersonaReconciler{Client: c, Scheme: c.Scheme()}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: "shop", Name: "api"},
	})

	require.NoError(t, err)
	assert.True(t, res.RequeueAfter > 0)
}

// F-10: deleting a namespace mid-cycle made the health cycle log
// `failed to process diagnosis ... namespaces "x" not found` at ERROR and count
// the diagnosis as discarded. The namespace is gone; there is nothing to record.
func TestHealthCycle_NamespaceVanishingIsNotAnError(t *testing.T) {
	nsGone := apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "crash-nopersona")
	c := fake.NewClientBuilder().WithScheme(noiseScheme(t)).
		WithStatusSubresource(&dorguv1.IncidentMemory{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return nsGone },
		}).Build()
	capture, log := newLogCapture()
	r := newNoiseReconciler(c, &stubCollector{name: "stub", signals: []detection.Signal{{
		Type: detection.SignalOOMKilled, Severity: detection.SeverityCritical, Category: detection.CategoryHealth,
		Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "p", Namespace: "crash-nopersona"},
		DetectedAt: metav1.Now().Time,
	}}})
	r.Logger = log
	r.Diagnosis = diagnosis.NewEngine(log, diagnosis.NewRuleBasedProvider(log))

	r.reconcile(context.Background())

	assert.Empty(t, capture.errors, "a vanished namespace is a race, not an ERROR")
}
