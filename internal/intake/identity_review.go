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

package intake

import (
	"context"
	"errors"
	"fmt"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The operator asks the API server who it is. Every authenticated user may; this
// grants it explicitly for a cluster whose roles say otherwise.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=selfsubjectreviews,verbs=create

const (
	// ReasonOperatorIdentityMismatch: the API server says the operator is someone
	// other than OPERATOR_SERVICE_ACCOUNT and POD_NAMESPACE claim.
	ReasonOperatorIdentityMismatch = "OperatorIdentityMismatch"

	// ReasonOperatorIdentityUnverified: the operator could not ask the API server
	// who it is, so the claim is unchecked.
	ReasonOperatorIdentityUnverified = "OperatorIdentityUnverified"
)

// WhoAmI returns the username the API server authenticates the operator as.
type WhoAmI func(ctx context.Context) (string, error)

// WhoAmIFromSelfSubjectReview asks the API server who the operator is, with a
// SelfSubjectReview (authentication.k8s.io/v1, Kubernetes 1.28 and later).
// Every authenticated user may create one; the operator's ClusterRole grants it
// as well, for a cluster whose roles say otherwise.
func WhoAmIFromSelfSubjectReview(cs kubernetes.Interface) WhoAmI {
	return func(ctx context.Context) (string, error) {
		review, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
		if err != nil {
			return "", fmt.Errorf("asking the API server who the operator is (SelfSubjectReview): %w", err)
		}
		return review.Status.UserInfo.Username, nil
	}
}

// reasonedError is a check failure that names its own reason, for a check that can
// fail in more than one way.
type reasonedError struct {
	reason string
	err    error
}

func (e *reasonedError) Error() string         { return e.err.Error() }
func (e *reasonedError) Unwrap() error         { return e.err }
func (e *reasonedError) FailureReason() string { return e.reason }

// failureReason is the reason a check's error asks to be reported with, or the
// check's own.
func failureReason(err error, fallback string) string {
	var r interface{ FailureReason() string }
	if errors.As(err, &r) && r.FailureReason() != "" {
		return r.FailureReason()
	}
	return fallback
}

// IdentityVerifiedCheck is the preflight check that the identity the operator
// believes it has (configured, so a claim) is the one the API server gives it.
// Without it a well-formed but wrong OPERATOR_SERVICE_ACCOUNT would be recorded as
// the planner's username on every proposal and action. It fails closed: on a
// mismatch, and when the API server cannot be asked.
func IdentityVerifiedCheck(expected string, whoAmI WhoAmI) Check {
	return Check{
		Name:   "operator-identity-verified",
		Reason: ReasonOperatorIdentityUnverified,
		Run: func(ctx context.Context) error {
			actual, err := whoAmI(ctx)
			switch {
			case err != nil:
				return &reasonedError{ReasonOperatorIdentityUnverified, fmt.Errorf(
					"the operator is configured as %q but could not check that with the API server: %w", expected, err)}
			case actual == "":
				return &reasonedError{ReasonOperatorIdentityUnverified, fmt.Errorf(
					"the operator is configured as %q but the API server's SelfSubjectReview returned no username", expected)}
			case actual != expected:
				return &reasonedError{ReasonOperatorIdentityMismatch, fmt.Errorf(
					"the operator is configured as %q (%s and POD_NAMESPACE) but the API server authenticates it as %q: "+
						"fix the deployment so the operator runs as the ServiceAccount it names", expected, ServiceAccountEnv, actual)}
			}
			return nil
		},
	}
}
