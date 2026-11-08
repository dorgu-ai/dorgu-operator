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

package gate

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	// DefaultHealthCheckAfter is how long the operator waits after applying a
	// change before verifying it. It is the operator's value; an author's
	// request for a different one is ignored.
	DefaultHealthCheckAfter = 10 * time.Minute

	// DefaultVerifyRetries is the rollback retry budget the operator assigns.
	DefaultVerifyRetries = 1

	// kindApplicationPersona is the only persona kind a remediation writes to.
	kindApplicationPersona = "ApplicationPersona"

	// defaultKubeSystem is always protected.
	defaultKubeSystem = "kube-system"
)

// Gate runs the pipeline. Build one with New and share it: it holds no state
// between runs beyond its configuration.
type Gate struct {
	client client.Client
	logger logr.Logger

	// protected are the namespaces the deny-list always refuses, on top of
	// kube-system and any ClusterPersona excludeNamespaces.
	protected []string

	// operatorNamespaceUnknown is set when the operator could not determine its
	// own namespace. The deny-list cannot then protect it, so the gate fails
	// closed (R12).
	operatorNamespaceUnknown bool

	now    func() time.Time
	stages []Stage
}

// Option configures a Gate.
type Option func(*Gate)

// WithProtectedNamespaces adds namespaces the deny-list always refuses: the
// operator's own namespace and any the installer configured. Empty names are
// ignored, so an unknown operator namespace can be passed through as-is.
func WithProtectedNamespaces(namespaces ...string) Option {
	return func(g *Gate) {
		for _, ns := range namespaces {
			ns = NormalizeNamespace(ns)
			if ns != "" && !slices.Contains(g.protected, ns) {
				g.protected = append(g.protected, ns)
			}
		}
	}
}

// WithOperatorNamespaceUnknown tells the gate the operator could not determine
// its own namespace, so the deny-list cannot protect it. The gate then refuses
// every proposal, rule-based or AI, rather than risk planning a change in the
// namespace Dorgu itself runs in.
func WithOperatorNamespaceUnknown() Option {
	return func(g *Gate) { g.operatorNamespaceUnknown = true }
}

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) Option {
	return func(g *Gate) { g.now = now }
}

// New builds a Gate with the standard pipeline.
func New(c client.Client, logger logr.Logger, opts ...Option) *Gate {
	g := &Gate{
		client:    c,
		logger:    logger.WithName("gate"),
		protected: []string{defaultKubeSystem},
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(g)
	}
	g.stages = []Stage{
		validateStage{g},
		groundStage{g},
		ownershipStage{g},
		guardrailsStage{g},
		modePolicyStage{g},
		dedupCooldownStage{g},
	}
	return g
}

// Stages returns the pipeline's stage names in order.
func (g *Gate) Stages() []string {
	names := make([]string, 0, len(g.stages))
	for _, s := range g.stages {
		names = append(names, s.Name())
	}
	return names
}

// Run puts a candidate through the pipeline.
//
// It stops at the first stage that refuses, so a refused plan reports the
// earliest reason and nothing later reads the cluster on its behalf. An error
// means the gate could not decide (a read failed); callers must treat that as
// "do not proceed", never as a pass.
func (g *Gate) Run(ctx context.Context, cand Candidate, phase Phase) (*Decision, error) {
	if g == nil {
		return nil, fmt.Errorf("no gate is configured, so nothing can be cleared")
	}

	d := newDecision(&cand, phase, metav1.NewTime(g.now()))
	for _, stage := range g.stages {
		if err := stage.Run(ctx, &cand, d); err != nil {
			return nil, fmt.Errorf("gate stage %s: %w", stage.Name(), err)
		}
		if d.Result == ResultRefuse {
			break
		}
	}
	return d, nil
}

// newDecision starts a decision from a deep copy of the candidate's plan, so no
// stage can modify the caller's data.
func newDecision(cand *Candidate, phase Phase, now metav1.Time) *Decision {
	d := &Decision{
		Phase:       phase,
		Result:      ResultPass,
		EvaluatedAt: now,
		Action:      *cand.Action.DeepCopy(),
		Verify: dorguv1.RemediationRollbackSpec{
			Enabled:          true,
			HealthCheckAfter: &metav1.Duration{Duration: DefaultHealthCheckAfter},
			MaxRetries:       DefaultVerifyRetries,
		},
	}
	if len(cand.Steps) > 0 {
		d.Steps = make([]dorguv1.RemediationStep, len(cand.Steps))
		for i := range cand.Steps {
			d.Steps[i] = *cand.Steps[i].DeepCopy()
		}
	}
	return d
}

// personaKey identifies the persona a candidate targets, defaulting an empty
// namespace the way every other lookup does.
func personaKey(cand *Candidate) client.ObjectKey {
	ns := cand.Target.Namespace
	if ns == "" {
		ns = cand.Namespace
	}
	return client.ObjectKey{Namespace: ns, Name: cand.Target.Name}
}
