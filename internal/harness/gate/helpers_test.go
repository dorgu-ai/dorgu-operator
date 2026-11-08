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
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	testNS             = "shop"
	testPersona        = "web"
	clusterPersonaKind = "ClusterPersona"
	incidentOne        = "im-1"
	memLimit           = "spec.resources.limits.memory"
	cpuLimit           = "spec.resources.limits.cpu"
)

// fixedNow is the clock every test gate runs on.
var fixedNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, dorguv1.AddToScheme(s))
	return s
}

func fakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&dorguv1.RemediationAction{}).Build()
}

// failingListClient fails every List of a kind the predicate selects.
func failingListClient(t *testing.T, fails func(client.ObjectList) bool, objs ...client.Object) client.Client {
	t.Helper()
	return interceptor.NewClient(fakeClient(t, objs...), interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if fails(list) {
				return errors.New("the API server is unavailable")
			}
			return inner.List(ctx, list, opts...)
		},
	})
}

func isKind[T client.ObjectList](list client.ObjectList) bool {
	_, ok := list.(T)
	return ok
}

func newTestGate(t *testing.T, c client.Client, opts ...Option) *Gate {
	t.Helper()
	opts = append([]Option{WithClock(func() time.Time { return fixedNow })}, opts...)
	return New(c, logr.Discard(), opts...)
}

func jsonOf(raw string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(raw)} }

func memPatch(value string) *apiextensionsv1.JSON {
	return jsonOf(`{"spec":{"resources":{"limits":{"memory":"` + value + `"}}}}`)
}

func persona(memory string) *dorguv1.ApplicationPersona {
	p := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: testPersona, Namespace: testNS, Generation: 3},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: testPersona, Type: "api", Tier: "standard",
			Resources: &dorguv1.ResourceConstraints{Limits: &dorguv1.ResourceValues{Memory: memory, CPU: "250m"}},
		},
	}
	return p
}

// deployment is a live Deployment for the persona with the given memory limit.
func deployment(memory string, mutate ...func(*appsv1.Deployment)) *appsv1.Deployment {
	labels := map[string]string{"app": testPersona}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: testPersona, Namespace: testNS, Labels: labels, UID: "uid-1", ResourceVersion: "42",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: testPersona,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
					},
				}}},
			},
		},
	}
	for _, m := range mutate {
		m(d)
	}
	return d
}

func target() dorguv1.PersonaReference {
	return dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: testPersona, Namespace: testNS}
}

// personaUpdateStep is a one-step plan raising memory to the given value.
func personaUpdateStep(patch *apiextensionsv1.JSON) dorguv1.RemediationStep {
	return dorguv1.RemediationStep{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
		Description: "raise memory", Rationale: "oom", Patch: patch,
	}
}

func planCandidate(steps ...dorguv1.RemediationStep) Candidate {
	return Candidate{
		Target: target(), Namespace: testNS, Name: "ra-new", Source: dorguv1.PlanSourceAIAnthropic,
		IncidentRef: dorguv1.IncidentReference{Name: incidentOne, Namespace: testNS},
		Steps:       steps,
	}
}

func legacyCandidate(patch *apiextensionsv1.JSON) Candidate {
	return Candidate{
		Target: target(), Namespace: testNS, Name: "ra-new", Source: dorguv1.PlanSourceRuleBased,
		IncidentRef: dorguv1.IncidentReference{Name: incidentOne, Namespace: testNS},
		Action:      dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypePersonaUpdate, Patch: patch},
	}
}

// sibling is another RemediationAction targeting the test persona.
func sibling(name, phase string, mutate ...func(*dorguv1.RemediationAction)) *dorguv1.RemediationAction {
	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS, CreationTimestamp: metav1.NewTime(fixedNow.Add(-10 * time.Minute)),
		},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef:  target(),
			IncidentRef: dorguv1.IncidentReference{Name: "im-other", Namespace: testNS},
			Action:      dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification},
		},
		Status: dorguv1.RemediationActionStatus{Phase: phase},
	}
	for _, m := range mutate {
		m(ra)
	}
	return ra
}

func clusterPersona(mutate func(*dorguv1.SelfHealingPolicy)) *dorguv1.ClusterPersona {
	policy := &dorguv1.SelfHealingPolicy{}
	if mutate != nil {
		mutate(policy)
	}
	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: dorguv1.ClusterPersonaSpec{
			Name: "cluster", Policies: &dorguv1.ClusterPolicies{SelfHealing: policy},
		},
	}
}

// run runs the gate and fails the test on an infrastructure error.
//
// At ApprovalRecheck a candidate with no prior observation gets the one a clean
// proposal against deployment() would have recorded, so the tests that are about
// something else do not trip the "no proposal record" refusal. A test about the
// prior observation itself calls g.Run directly.
func run(t *testing.T, g *Gate, cand Candidate, phase Phase) *Decision {
	t.Helper()
	if phase == PhaseApprovalRecheck && cand.prior == nil && cand.priorProblem == "" {
		cand.prior = priorObservation("uid-1")
	}
	d, err := g.Run(context.Background(), cand, phase)
	require.NoError(t, err)
	return d
}

// verdict returns the first verdict for a guardrail, or nil.
func verdict(d *Decision, guardrail string) *dorguv1.GateVerdict {
	for i := range d.Verdicts {
		if d.Verdicts[i].Guardrail == guardrail {
			return &d.Verdicts[i]
		}
	}
	return nil
}

func verdictKind(d *Decision, guardrail string) string {
	if v := verdict(d, guardrail); v != nil {
		return v.Verdict
	}
	return "absent"
}

// stepPatchValue is the memory limit a step patches, or "".
func stepPatchValue(t *testing.T, step dorguv1.RemediationStep) string {
	t.Helper()
	if step.Patch == nil {
		return ""
	}
	return PatchLeafValues(step.Patch.Raw)[memLimit]
}

func metav1Time(t time.Time) time.Time { return t }

func metav1Now() metav1.Time { return metav1.NewTime(fixedNow) }

// interceptorGetFailure is a client whose ApplicationPersona Get fails.
func interceptorGetFailure(t *testing.T) client.Client {
	t.Helper()
	return interceptor.NewClient(fakeClient(t), interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*dorguv1.ApplicationPersona); ok {
				return errors.New("the API server is unavailable")
			}
			return inner.Get(ctx, key, obj, opts...)
		},
	})
}

type (
	clientObject     = client.Object
	clientObjectList = client.ObjectList
)

func mustQuantity(s string) resource.Quantity { return resource.MustParse(s) }

// priorObservation is what the operator recorded when it proposed against a
// workload with the given UID.
func priorObservation(uid string) *dorguv1.GateObserved {
	now := metav1Now()
	return &dorguv1.GateObserved{WorkloadUID: uid, ObservedAt: &now}
}
