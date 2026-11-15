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

// Package lifecycle runs the real reconcilers against an API server that enforces the chart's
// admission policies (C2 review F-6): the proposal intake and the remediation controller write
// as the operator identity, the proposer and the approver are real users, and the whole path
// from proposal to apply must go through. Until this existed, "the operator's own writes are
// never denied by its own policies" rested on a reading of the policies.
package lifecycle

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

var env *admissionenv.Env

func TestMain(m *testing.M) {
	var err error
	env, err = admissionenv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting the admission environment: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}

var counter atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, counter.Add(1)) }

func ctxFor(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func rules(resources []string, verbs ...string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: []string{"dorgu.io"}, Resources: resources, Verbs: verbs}
}

// The roles the scenarios give people: what the chart's dorgu-agent-proposer and
// dorgu-approver roles grant.
var (
	proposerRules = []rbacv1.PolicyRule{rules([]string{"remediationproposals"}, "create", "get", "list", "watch")}
	approverRules = []rbacv1.PolicyRule{
		rules([]string{"remediationactions"}, "get", "list", "approve", "reject"),
		rules([]string{"remediationactions/status"}, "get", "patch", "update"),
	}
)

// operator is the operator as the cluster sees it: the reconcilers, the preflight and the gate
// all act through it, never through the admin.
type operator struct {
	c        client.Client
	proposal *controller.ProposalReconciler
	action   *controller.RemediationController
	guard    *intake.Preflight
}

type emptyCollector struct{}

func (emptyCollector) Name() string                                        { return "empty" }
func (emptyCollector) Collect(context.Context) ([]detection.Signal, error) { return nil, nil }

// newOperator wires the reconcilers the way cmd/main.go does, over the operator's client,
// with the real preflight (policy content and RBAC checks, asked of the API server as the
// operator) as the ticket guard.
func newOperator(t *testing.T) *operator {
	t.Helper()
	cfg, err := env.UserConfig(admissionenv.OperatorUsername, []string{"system:masters"})
	require.NoError(t, err)
	cs, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	c := env.Operator
	log := logr.Discard()

	// A clock a little ahead, as the controller suite's gate has: the Deployment was written
	// a moment ago, and the gate would call that a write after the ticket.
	ahead := func() time.Time { return time.Now().Add(2 * gate.ClockSkewMargin) }
	g := gate.New(c, log, gate.WithReader(c), gate.WithClock(ahead))

	guard := intake.NewPreflight(log,
		intake.OperatorIdentityCheck(nil),
		intake.IdentityVerifiedCheck(admissionenv.OperatorUsername, intake.WhoAmIFromSelfSubjectReview(cs)),
		intake.AdmissionPoliciesCheck(c, intake.UsernameSource(intake.WhoAmIFromSelfSubjectReview(cs))),
		intake.OperatorRBACCheck(intake.AccessReviewerFromClientset(cs)),
	)
	guard.Run(context.Background())
	require.Nil(t, guard.Blocked(),
		"the policies as shipped and the operator's permissions pass the operator's own preflight")

	processor := intake.New(intake.Config{
		Client: c, Reader: c, Gate: g,
		Incidents:        &controller.ProposalIncidents{Client: c},
		OperatorUsername: admissionenv.OperatorUsername,
		Logger:           log,
	})
	guarded := intake.GuardActionWrites(c) // what every component but the intake gets
	return &operator{
		c:        c,
		proposal: &controller.ProposalReconciler{Client: c, Processor: processor},
		action: &controller.RemediationController{
			Client: guarded, Gate: g,
			Executor:         remediation.NewExecutor(guarded, g, log),
			Verifier:         remediation.NewVerifier(detection.NewEngine(log, emptyCollector{}), guarded, log),
			Rollback:         remediation.NewRollback(guarded, log),
			Logger:           log,
			OperatorUsername: admissionenv.OperatorUsername,
			TicketGuard:      guard,
		},
		guard: guard,
	}
}

func (o *operator) reconcileProposal(t *testing.T, prop *dorguv1alpha1.RemediationProposal) {
	t.Helper()
	_, err := o.proposal.Reconcile(ctxFor(t), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(prop)})
	require.NoError(t, err, "the operator's own writes are never denied by its own policies")
}

func (o *operator) reconcileAction(t *testing.T, ra *dorguv1.RemediationAction) ctrl.Result {
	t.Helper()
	res, err := o.action.Reconcile(ctxFor(t), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
	require.NoError(t, err, "the operator's own writes are never denied by its own policies")
	return res
}

// user is an authenticated person or agent with rules in ns.
type user struct {
	name string
	c    client.Client
}

func newUser(t *testing.T, ns, name string, rules []rbacv1.PolicyRule) user {
	t.Helper()
	c, err := env.UserClient(name, nil)
	require.NoError(t, err)
	require.NoError(t, env.GrantRules(ctxFor(t), ns, name, rules))
	return user{name: name, c: c}
}

// memoryProposal is a proposal to raise the persona's memory limit, written by proposer.
func memoryProposal(ns, proposer string) *dorguv1alpha1.RemediationProposal {
	return &dorguv1alpha1.RemediationProposal{
		ObjectMeta: metav1.ObjectMeta{Name: uniq("p"), Namespace: ns},
		Spec: dorguv1alpha1.RemediationProposalSpec{
			Proposer:       dorguv1.ProposerIdentity{Username: proposer},
			IdempotencyKey: uniq("k"),
			IncidentRef:    &dorguv1.IncidentReference{Name: admissionenv.WorkloadIncident, Namespace: ns},
			Target:         dorguv1alpha1.ProposalTarget{Kind: "ApplicationPersona", Namespace: ns, Name: admissionenv.AppName},
			Steps: []dorguv1.RemediationStep{{
				Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
				Description: "raise the memory limit",
				Patch:       &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"384Mi"}}}}`)},
			}},
			Rationale: "the container was OOM killed three times in ten minutes",
		},
	}
}
