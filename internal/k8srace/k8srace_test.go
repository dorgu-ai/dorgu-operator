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

package k8srace

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	incidentGR = schema.GroupResource{Group: "dorgu.io", Resource: "incidentmemories"}
	personaGR  = schema.GroupResource{Group: "dorgu.io", Resource: "applicationpersonas"}
)

func conflictErr(name string) error {
	return apierrors.NewConflict(incidentGR, name, errors.New("modified"))
}

func TestPredicates(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		conflict, gone bool
	}{
		{"nil", nil, false, false},
		{"conflict", conflictErr("x"), true, false},
		{"wrapped conflict", errors.Join(errors.New("updating labels"), conflictErr("x")), true, false},
		{"named object not found", apierrors.NewNotFound(personaGR, "api"), false, true},
		{"wrapped named object not found", errors.Join(errors.New("creating"), apierrors.NewNotFound(personaGR, "api")), false, true},
		{"namespace terminating", &apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403,
			Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{Type: corev1.NamespaceTerminatingCause}}},
		}}, false, true},
		{"ordinary forbidden (lost RBAC)", apierrors.NewForbidden(personaGR, "api", errors.New("rbac")), false, false},
		{"missing resource type (CRD not installed)", &apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: 404,
			Message: "the server could not find the requested resource",
		}}, false, false},
		{"not found from a non-status error", errors.New("not found"), false, false},
		{"internal error", apierrors.NewInternalError(errors.New("x")), false, false},
		{"plain error", errors.New("boom"), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.conflict, IsConflict(tt.err), "IsConflict")
			assert.Equal(t, tt.gone, IsGone(tt.err), "IsGone")
			assert.Equal(t, tt.conflict || tt.gone, IsRace(tt.err), "IsRace")
		})
	}
}
