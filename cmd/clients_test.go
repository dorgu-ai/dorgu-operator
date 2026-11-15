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
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
)

func raFixture(name string) *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

// C1 review F-5: the single creator is enforced by the clients, not by a scan. The
// guarded client every component gets refuses; the intake's client does not.
func TestNewClusterClients_OnlyTheIntakeClientCanCreateActions(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dorguv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	raw := fake.NewClientBuilder().WithScheme(scheme).Build()
	clients := newClusterClients(raw)
	ctx := context.Background()

	if err := clients.Guarded.Create(ctx, raFixture("by-component")); !errors.Is(err, intake.ErrActionWriteRefused) {
		t.Fatalf("a component's client created a RemediationAction: %v", err)
	}
	if err := clients.Intake.Create(ctx, raFixture("by-intake")); err != nil {
		t.Fatalf("the intake's client must create: %v", err)
	}
	var list dorguv1.RemediationActionList
	if err := raw.List(ctx, &list); err != nil || len(list.Items) != 1 || list.Items[0].Name != "by-intake" {
		t.Fatalf("only the intake's action exists, got %v (%v)", list.Items, err)
	}
}

// The pipeline is wired with both: every component built from it holds the guarded
// client, and the proposal it makes still becomes an action, created by the intake.
func TestBuildHealthPipeline_ComponentsAreGuardedAndTheIntakeStillCreates(t *testing.T) {
	proposeThroughPipeline(t, func(raw client.WithWatch) client.Reader { return raw })
}

// F-10: the intake's proposal List is served by the cached client the pipeline is
// built on, not by the API reader (a cluster-wide List per undecided proposal).
func TestBuildHealthPipeline_TheIntakeListsProposalsFromTheCacheNotTheAPIReader(t *testing.T) {
	proposeThroughPipeline(t, func(raw client.WithWatch) client.Reader {
		return interceptor.NewClient(raw, interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*dorguv1alpha1.RemediationProposalList); ok {
					return errors.New("the API reader must not list proposals")
				}
				return c.List(ctx, list, opts...)
			},
		})
	})
}

func proposeThroughPipeline(t *testing.T, apiReader func(client.WithWatch) client.Reader) {
	t.Helper()
	t.Setenv(intake.ServiceAccountEnv, "dorgu-operator")
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		dorguv1.AddToScheme, dorguv1alpha1.AddToScheme, appsv1.AddToScheme, apiextensionsv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
		Spec: dorguv1.ApplicationPersonaSpec{Name: "my-app",
			Resources: &dorguv1.ResourceConstraints{Limits: &dorguv1.ResourceValues{Memory: "256Mi", CPU: "500m"}}},
	}
	incident := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: "im-oom", Namespace: "default"},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "my-app", Namespace: "default"},
			Category:   "resource", Severity: "critical", Detection: dorguv1.DetectionInfo{Signal: "OOMKilled"},
		},
		Status: dorguv1.IncidentMemoryStatus{Phase: "Detected"},
	}
	raw := fake.NewClientBuilder().WithScheme(scheme).WithObjects(persona, incident).
		WithObjects(installedCRDs()...).
		WithStatusSubresource(&dorguv1alpha1.RemediationProposal{}, &dorguv1.RemediationAction{}).Build()
	setup := newIntakeSetup(raw, testOperatorNS, logr.Discard())
	setup.Preflight.Run(context.Background())
	p := buildHealthPipeline(newClusterClients(raw), apiReader(raw), operatorConfig{}, testOperatorNS, setup,
		logr.Discard())

	diag := diagnosis.Diagnosis{
		Summary: "OOMKilled detected", Confidence: 0.85, Category: "resource", Severity: detection.SeverityCritical,
		PersonaRef: &dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "my-app", Namespace: "default"},
		Contributing: []diagnosis.ContributingSignal{{Signal: detection.Signal{
			Type: detection.SignalOOMKilled, Severity: detection.SeverityCritical,
			Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "my-app-abc", Namespace: "default"},
			DetectedAt: time.Now(),
		}}},
		SuggestedAction: "resource-adjustment", DiagnosedAt: time.Now(),
	}
	result, err := p.Proposer.Propose(context.Background(), diag, incident)
	if err != nil || !result.Proposed {
		t.Fatalf("the planner's proposal must still become an action: proposed=%v err=%v",
			result != nil && result.Proposed, err)
	}
	var list dorguv1.RemediationActionList
	if err := raw.List(context.Background(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("want one action created by the intake, got %d (%v)", len(list.Items), err)
	}
}

// mgr.GetClient() is called once in main.go, to build the clusterClients. Any other
// call would hand a component the unguarded client.
func TestMain_CallsGetClientOnlyToBuildTheGuardedClients(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for line := range strings.SplitSeq(string(src), "\n") {
		code := line
		if i := strings.Index(code, "//"); i >= 0 {
			code = code[:i]
		}
		if strings.Contains(code, "GetClient()") {
			calls = append(calls, strings.TrimSpace(code))
		}
	}
	if len(calls) != 1 || !regexp.MustCompile(`newClusterClients\(mgr\.GetClient\(\)\)`).MatchString(calls[0]) {
		t.Fatalf("main.go must call GetClient() exactly once, as newClusterClients(mgr.GetClient()); found %q", calls)
	}
	// And nothing hands the raw client to a component under another name.
}

// installedCRDs are the two CRDs the intake's preflight reads, as a current
// installation has them.
func installedCRDs() []client.Object {
	crd := func(name, version string, spec ...string) client.Object {
		props := map[string]apiextensionsv1.JSONSchemaProps{}
		for _, p := range spec {
			props[p] = apiextensionsv1.JSONSchemaProps{Type: "object"}
		}
		return &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: props}}}},
			}}},
			Status: apiextensionsv1.CustomResourceDefinitionStatus{
				Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
					{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}},
		}
	}
	return []client.Object{
		crd(intake.ProposalCRDName, "v1alpha1", "steps"),
		crd(intake.ActionCRDName, "v1", "provenance", "steps"),
	}
}
