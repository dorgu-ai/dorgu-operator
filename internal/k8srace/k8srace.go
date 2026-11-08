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

// Package k8srace names the Kubernetes API errors that are expected races, not
// faults: an optimistic-lock conflict, and an object (or its namespace) that was
// deleted underneath a reader. A busy cluster produces them every minute, so
// callers log them at debug and retry or move on, and keep ERROR for failures
// that need a person.
package k8srace

import (
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// IsConflict reports an optimistic-lock conflict ("the object has been
// modified; please apply your changes to the latest version"). Two writers
// raced and one lost; the loser re-reads and tries again.
func IsConflict(err error) bool {
	return err != nil && apierrors.IsConflict(err)
}

// IsGone reports that the object, or the namespace it lives in, went away
// underneath us: a named object that is NotFound, or a namespace that is
// terminating. Both are what deleting a persona or a namespace mid-cycle looks
// like, and neither is a fault.
//
// A NotFound that names no object ("the server could not find the requested
// resource") is not benign: it means a resource type is missing, such as a CRD
// that was never installed, and that must stay loud.
func IsGone(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
		return true
	}
	if !apierrors.IsNotFound(err) {
		return false
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	details := status.Status().Details
	return details != nil && details.Name != ""
}

// IsRace is either of the two: an expected race, not a fault.
func IsRace(err error) bool {
	return IsConflict(err) || IsGone(err)
}
