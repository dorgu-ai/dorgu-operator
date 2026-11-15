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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

func crdOnlyClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func failureReasons(fs []intake.Failure) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Reason)
	}
	return out
}

// F-8 and F-9 at the wiring: the setup the operator starts with is closed for each of
// its three reasons, and open when all three hold.
func TestNewIntakeSetup_IsClosedForEachReasonAndOpenWhenAllHold(t *testing.T) {
	t.Setenv(intake.ServiceAccountEnv, "dorgu-operator")

	open := newIntakeSetup(crdOnlyClient(t, installedCRDs()...), testOperatorNS, logr.Discard())
	if got := open.Preflight.Run(context.Background()); len(got) != 0 {
		t.Fatalf("a complete installation must pass, got %v", failureReasons(got))
	}
	if open.Username != "system:serviceaccount:dorgu-system:dorgu-operator" {
		t.Fatalf("username = %q", open.Username)
	}

	noCRDs := newIntakeSetup(crdOnlyClient(t), testOperatorNS, logr.Discard())
	got := failureReasons(noCRDs.Preflight.Run(context.Background()))
	if len(got) != 2 || got[0] != intake.ReasonProposalCRDMissing || got[1] != intake.ReasonActionProvenanceMissing {
		t.Fatalf("with no CRDs: %v", got)
	}

	t.Setenv(intake.ServiceAccountEnv, "")
	noIdentity := newIntakeSetup(crdOnlyClient(t, installedCRDs()...), testOperatorNS, logr.Discard())
	if noIdentity.Username != "" {
		t.Fatalf("username must not be defaulted, got %q", noIdentity.Username)
	}
	got = failureReasons(noIdentity.Preflight.Run(context.Background()))
	if len(got) != 1 || got[0] != intake.ReasonOperatorIdentityUnknown {
		t.Fatalf("with no identity: %v", got)
	}
}

func TestLateRegistration_RegistersOnceWhenTheProposalCRDAppears(t *testing.T) {
	calls := 0
	l := &lateRegistration{log: logr.Discard(), register: func() error { calls++; return nil }}

	l.onChecks([]intake.Failure{{Reason: intake.ReasonProposalCRDMissing}})
	if calls != 0 {
		t.Fatal("registered while the proposal CRD is missing: the cache would never sync and the manager would exit")
	}
	l.onChecks([]intake.Failure{{Reason: intake.ReasonActionProvenanceMissing}})
	if calls != 1 {
		t.Fatalf("a failure that is not the proposal CRD must not hold the controller back, calls = %d", calls)
	}
	l.onChecks(nil)
	l.onChecks(nil)
	if calls != 1 {
		t.Fatalf("registered %d times, want exactly one", calls)
	}
}

func TestLateRegistration_ARegistrationThatFailsIsTriedAgain(t *testing.T) {
	calls := 0
	l := &lateRegistration{log: logr.Discard(), register: func() error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	}}
	l.onChecks(nil)
	l.onChecks(nil)
	l.onChecks(nil)
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one failure, one success, then done)", calls)
	}
}

// N-9: a well-formed but wrong OPERATOR_SERVICE_ACCOUNT is caught against what the
// API server says, and closes the intake with its own reason.
func TestNewIntakeSetup_VerifyIdentityFailsClosedOnAMismatch(t *testing.T) {
	t.Setenv(intake.ServiceAccountEnv, "dorgu-operator")
	who := func(name string) intake.WhoAmI {
		return func(context.Context) (string, error) { return name, nil }
	}

	right := newIntakeSetup(crdOnlyClient(t, installedCRDs()...), testOperatorNS, logr.Discard())
	right.verifyIdentity(who("system:serviceaccount:dorgu-system:dorgu-operator"))
	if got := right.Preflight.Run(context.Background()); len(got) != 0 {
		t.Fatalf("a matching identity must pass, got %v", failureReasons(got))
	}

	wrong := newIntakeSetup(crdOnlyClient(t, installedCRDs()...), testOperatorNS, logr.Discard())
	wrong.verifyIdentity(who("system:serviceaccount:dorgu-system:default"))
	got := failureReasons(wrong.Preflight.Run(context.Background()))
	if len(got) != 1 || got[0] != intake.ReasonOperatorIdentityMismatch {
		t.Fatalf("a wrong identity: %v", got)
	}

	t.Setenv(intake.ServiceAccountEnv, "")
	none := newIntakeSetup(crdOnlyClient(t, installedCRDs()...), testOperatorNS, logr.Discard())
	none.verifyIdentity(who("anyone"))
	got = failureReasons(none.Preflight.Run(context.Background()))
	if len(got) != 1 || got[0] != intake.ReasonOperatorIdentityUnknown {
		t.Fatalf("with no configured identity there is nothing to verify, and it is already closed: %v", got)
	}
}

// R-6: the clients the preflight asks the API server through carry a request timeout of
// their own, on a copy of the rest config (the manager's keeps its own, which a long
// watch needs), so a server that accepts the connection and goes quiet fails a call
// instead of holding the preflight loop for ever.
func TestPreflightRESTConfig_BoundsRequestsOnACopy(t *testing.T) {
	base := &rest.Config{Host: "https://kubernetes.invalid", Timeout: 0}

	got := preflightRESTConfig(base)

	if got.Timeout != preflightTimeout {
		t.Fatalf("timeout = %v, want %v", got.Timeout, preflightTimeout)
	}
	if base.Timeout != 0 {
		t.Fatal("the manager's own config must not be changed")
	}
	if got.Host != base.Host {
		t.Fatalf("host = %q, want %q", got.Host, base.Host)
	}

	// A config that is already stricter keeps its bound.
	strict := &rest.Config{Timeout: time.Second}
	if preflightRESTConfig(strict).Timeout != time.Second {
		t.Fatal("a stricter timeout must be kept")
	}
}
