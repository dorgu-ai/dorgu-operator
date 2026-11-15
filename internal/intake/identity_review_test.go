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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func whoAmI(name string, err error) WhoAmI {
	return func(context.Context) (string, error) { return name, err }
}

// N-9: OPERATOR_SERVICE_ACCOUNT is a claim the deployment makes; the API server
// says who the operator is. A well-formed but wrong name fails closed.
func TestIdentityVerifiedCheck(t *testing.T) {
	const mine = "system:serviceaccount:dorgu-system:dorgu-operator"
	tests := []struct {
		name       string
		who        WhoAmI
		wantReason string
		wantText   string
	}{
		{"the API server agrees", whoAmI(mine, nil), "", ""},
		{"a different service account", whoAmI("system:serviceaccount:dorgu-system:default", nil),
			ReasonOperatorIdentityMismatch, "system:serviceaccount:dorgu-system:default"},
		{"a human user", whoAmI("kubernetes-admin", nil), ReasonOperatorIdentityMismatch, "kubernetes-admin"},
		{"the review cannot be made", whoAmI("", errors.New("connection refused")),
			ReasonOperatorIdentityUnverified, "connection refused"},
		{"the review answers with no name", whoAmI("", nil), ReasonOperatorIdentityUnverified, "no username"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pf := NewPreflight(logr.Discard(), IdentityVerifiedCheck(mine, tt.who))
			failures := pf.Run(context.Background())
			if tt.wantReason == "" {
				assert.Empty(t, failures)
				assert.Nil(t, pf.Blocked())
				return
			}
			require.Len(t, failures, 1)
			assert.Equal(t, tt.wantReason, failures[0].Reason)
			assert.Contains(t, failures[0].Message, mine, "the message says what the operator was configured as")
			assert.Contains(t, failures[0].Message, tt.wantText)
			assert.NotNil(t, pf.Blocked(), "the intake is closed")
		})
	}
}

func TestIdentityVerifiedCheck_AMismatchClosesTheIntakeAndAFixReopensIt(t *testing.T) {
	actual := "system:serviceaccount:dorgu-system:other"
	pf := NewPreflight(logr.Discard(), IdentityVerifiedCheck(TestOperatorUsername,
		func(context.Context) (string, error) { return actual, nil }))
	w := newWorld(t, Config{Preflight: pf}, baseObjects()...)
	w.create(t, proposal("p-id", "alice", "k"))
	pf.Run(context.Background())

	_, err := w.p.Process(context.Background(), keyOf("p-id"), nil)
	var notReady *NotReadyError
	require.ErrorAs(t, err, &notReady)
	assert.Equal(t, ReasonOperatorIdentityMismatch, notReady.Failure.Reason)
	assert.Empty(t, w.get(t, "p-id").Status.Phase, "no conversion, not even a status")

	actual = TestOperatorUsername
	pf.Run(context.Background())
	out := w.process(t, "p-id", nil)
	assert.Equal(t, "Converted", string(out.Phase))
}

func TestWhoAmIFromSelfSubjectReview(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.SelfSubjectReview{Status: authnv1.SelfSubjectReviewStatus{
			UserInfo: authnv1.UserInfo{Username: TestOperatorUsername}}}, nil
	})

	got, err := WhoAmIFromSelfSubjectReview(cs)(context.Background())

	require.NoError(t, err)
	assert.Equal(t, TestOperatorUsername, got)

	failing := fake.NewClientset()
	failing.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	_, err = WhoAmIFromSelfSubjectReview(failing)(context.Background())
	require.Error(t, err)
}

func keyOf(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: testNS, Name: name}
}
