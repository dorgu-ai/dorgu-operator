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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

func failing(name, reason, msg string) Check {
	return Check{Name: name, Reason: reason, Run: func(context.Context) error { return errors.New(msg) }}
}

func passing(name string) Check {
	return Check{Name: name, Reason: "Whatever", Run: func(context.Context) error { return nil }}
}

func TestPreflight_NoChecksIsReady(t *testing.T) {
	p := NewPreflight(logr.Discard())
	assert.Nil(t, p.Blocked())
	assert.Empty(t, p.Failures())
}

func TestPreflight_UntilItHasRunItIsNotReady(t *testing.T) {
	p := NewPreflight(logr.Discard(), passing("a"))
	require.NotNil(t, p.Blocked(), "fail closed until the first run")
	assert.Equal(t, ReasonPreflightPending, p.Blocked().Reason)

	p.Run(context.Background())
	assert.Nil(t, p.Blocked())
}

func TestPreflight_AFailingCheckBlocksAndNamesItself(t *testing.T) {
	p := NewPreflight(logr.Discard(), passing("a"),
		failing("b", "ProposalCRDMissing", "apply the CRD"), failing("c", "Other", "other"))

	got := p.Run(context.Background())

	require.Len(t, got, 2)
	blocked := p.Blocked()
	require.NotNil(t, blocked)
	assert.Equal(t, "b", blocked.Check)
	assert.Equal(t, "ProposalCRDMissing", blocked.Reason)
	assert.Equal(t, "apply the CRD", blocked.Message)
	assert.Len(t, p.Failures(), 2)
}

func TestPreflight_ACheckCanBeAddedLater(t *testing.T) {
	p := NewPreflight(logr.Discard())
	p.Add(failing("admission-policies", "PoliciesMissing", "C2 adds this one"))
	p.Run(context.Background())
	require.NotNil(t, p.Blocked())
	assert.Equal(t, "admission-policies", p.Blocked().Check)
}

func TestPreflight_ARecheckClearsAFailureAndSubscribersHearOfEveryRun(t *testing.T) {
	var broken atomic.Bool
	broken.Store(true)
	p := NewPreflight(logr.Discard(), Check{Name: "crd", Reason: "Missing", Run: func(context.Context) error {
		if broken.Load() {
			return errors.New("missing")
		}
		return nil
	}})
	var heard [][]Failure
	p.Subscribe(func(f []Failure) { heard = append(heard, f) })

	p.Run(context.Background())
	require.NotNil(t, p.Blocked())
	broken.Store(false)
	p.Run(context.Background())

	assert.Nil(t, p.Blocked(), "the next run sees the fix")
	require.Len(t, heard, 2)
	assert.Len(t, heard[0], 1)
	assert.Empty(t, heard[1])
}

func TestPreflight_StartRechecksOnItsInterval(t *testing.T) {
	var broken atomic.Bool
	broken.Store(true)
	p := NewPreflight(logr.Discard(), Check{Name: "crd", Reason: "Missing", Run: func(context.Context) error {
		if broken.Load() {
			return errors.New("missing")
		}
		return nil
	}})
	p.SetInterval(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	require.Eventually(t, func() bool { return p.Blocked() != nil }, time.Second, 5*time.Millisecond)
	broken.Store(false)
	require.Eventually(t, func() bool { return p.Blocked() == nil }, time.Second, 5*time.Millisecond, "the periodic recheck clears it")
	cancel()
	require.NoError(t, <-done)
	assert.False(t, p.NeedLeaderElection(), "every replica reports its own readiness")
}

func TestPreflight_ACheckThatPanicsOrReturnsAnEmptyErrorStillFails(t *testing.T) {
	p := NewPreflight(logr.Discard(), Check{Name: "x", Reason: "Bad", Run: func(context.Context) error { return errors.New("") }})
	p.Run(context.Background())
	require.NotNil(t, p.Blocked())
	assert.NotEmpty(t, p.Blocked().Message, "an error with no text is still said")
}

// F-8: a missing or malformed identity never defaults.
func TestOperatorUsername_IsStrict(t *testing.T) {
	tests := []struct {
		name, sa, ns string
		want         string
		wantErr      bool
	}{
		{"ok", "dorgu-operator", "dorgu-system", "system:serviceaccount:dorgu-system:dorgu-operator", false},
		{"kustomize prefixed name", "dorgu-controller-manager", "dorgu-operator-system", "system:serviceaccount:dorgu-operator-system:dorgu-controller-manager", false},
		{"no service account env", "", "dorgu-system", "", true},
		{"blank service account env", "   ", "dorgu-system", "", true},
		{"no namespace", "dorgu-operator", "", "", true},
		{"colon in the name", "a:b", "dorgu-system", "", true},
		{"uppercase", "Dorgu", "dorgu-system", "", true},
		{"spaces inside", "dorgu operator", "dorgu-system", "", true},
		{"a path", "../x", "dorgu-system", "", true},
		{"bad namespace", "dorgu-operator", "Dorgu System", "", true},
		{"too long", strings.Repeat("a", 300), "dorgu-system", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(ServiceAccountEnv, tt.sa)
			got, err := OperatorUsername(tt.ns)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, got, "never a made-up username")
				assert.ErrorIs(t, err, ErrOperatorIdentity)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestProcessor_WithNoOperatorUsernameIsNotReady(t *testing.T) {
	w := newWorld(t, Config{OperatorUsername: "-"}, baseObjects()...)
	w.p.username = ""
	require.NotNil(t, w.p.Blocked(), "no username is a blocked intake, not a default")
	assert.Equal(t, ReasonOperatorIdentityUnknown, w.p.Blocked().Reason)
}

// F-8 and F-9: a blocked intake converts nothing, and decides nothing.
func TestProcess_ABlockedIntakeConvertsNothing(t *testing.T) {
	pf := NewPreflight(logr.Discard(), failing("operator-identity", ReasonOperatorIdentityUnknown, "set OPERATOR_SERVICE_ACCOUNT"))
	pf.Run(context.Background())
	w := newWorld(t, Config{Preflight: pf}, baseObjects()...)
	w.create(t, proposal("p", "alice", "k"))

	out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"}, nil)

	require.Error(t, err)
	var notReady *NotReadyError
	require.ErrorAs(t, err, &notReady)
	assert.Equal(t, ReasonOperatorIdentityUnknown, notReady.Failure.Reason)
	assert.Nil(t, out)
	assert.Empty(t, w.get(t, "p").Status.Phase, "the proposal was not even marked received")
	assert.Empty(t, actionNames(t, w.c), "no action")
}

func actionNames(t *testing.T, c client.Client) []string {
	t.Helper()
	var names []string
	var list dorguv1.RemediationActionList
	require.NoError(t, c.List(context.Background(), &list))
	for _, ra := range list.Items {
		names = append(names, ra.Name)
	}
	return names
}

func TestProcess_ABlockedIntakeResumesWhenItClears(t *testing.T) {
	var broken atomic.Bool
	broken.Store(true)
	pf := NewPreflight(logr.Discard(), Check{Name: "crd", Reason: "Missing", Run: func(context.Context) error {
		if broken.Load() {
			return errors.New("missing")
		}
		return nil
	}})
	pf.Run(context.Background())
	w := newWorld(t, Config{Preflight: pf}, baseObjects()...)
	w.create(t, proposal("p", "alice", "k"))
	_, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"}, nil)
	require.Error(t, err)

	broken.Store(false)
	pf.Run(context.Background())

	out := w.process(t, "p", nil)
	assert.Equal(t, v1alpha1.PhaseConverted, out.Phase, out.Message)
}

// F-9: the CRD checks.
func crdScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, apiextensionsv1.AddToScheme(s))
	return s
}

func crd(name, version string, established bool, schema *apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
	c := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "dorgu.io",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: schema},
			}},
		},
	}
	if established {
		c.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
			{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}
	}
	return c
}

func specWith(props ...string) *apiextensionsv1.JSONSchemaProps {
	spec := apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{}}
	for _, p := range props {
		spec.Properties[p] = apiextensionsv1.JSONSchemaProps{Type: "object"}
	}
	return &apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": spec}}
}

func TestProposalCRDCheck(t *testing.T) {
	good := crd(ProposalCRDName, "v1alpha1", true, specWith("steps"))
	tests := []struct {
		name    string
		objs    []client.Object
		wantErr string
	}{
		{"installed", []client.Object{good}, ""},
		{"missing", nil, "is not installed"},
		{"not established yet", []client.Object{crd(ProposalCRDName, "v1alpha1", false, specWith())}, "not established"},
		{"serves no v1alpha1", []client.Object{crd(ProposalCRDName, "v1beta1", true, specWith())}, "v1alpha1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(crdScheme(t)).WithObjects(tt.objs...).Build()
			err := ProposalCRDCheck(c).Run(context.Background())
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
			assert.Contains(t, err.Error(), "dorgu.io_remediationproposals.yaml", "it says which file to apply")
		})
	}
	assert.Equal(t, ReasonProposalCRDMissing, ProposalCRDCheck(nil).Reason)
}

func TestActionProvenanceCheck(t *testing.T) {
	tests := []struct {
		name    string
		objs    []client.Object
		wantErr string
	}{
		{"with provenance", []client.Object{crd(ActionCRDName, "v1", true, specWith("provenance", "steps"))}, ""},
		{"missing CRD", nil, "is not installed"},
		{"an old CRD that prunes provenance", []client.Object{crd(ActionCRDName, "v1", true, specWith("steps"))}, "spec.provenance"},
		{"no schema at all", []client.Object{crd(ActionCRDName, "v1", true, nil)}, "spec.provenance"},
		{"serves no v1", []client.Object{crd(ActionCRDName, "v2", true, specWith("provenance"))}, "v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(crdScheme(t)).WithObjects(tt.objs...).Build()
			err := ActionProvenanceCheck(c).Run(context.Background())
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
			assert.Contains(t, err.Error(), "dorgu.io_remediationactions.yaml")
		})
	}
	assert.Equal(t, ReasonActionProvenanceMissing, ActionProvenanceCheck(nil).Reason)
}

func TestCRDChecks_AReadErrorIsAFailureNotAPass(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(crdScheme(t)).Build()
	for name, err := range map[string]error{
		"forbidden": apierrors.NewForbidden(schema.GroupResource{Resource: "customresourcedefinitions"}, "x", errors.New("no")),
		"timeout":   errors.New("etcd timeout"),
	} {
		c := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return err
			},
		})
		for _, check := range []Check{ProposalCRDCheck(c), ActionProvenanceCheck(c)} {
			got := check.Run(context.Background())
			require.Error(t, got, name)
			assert.Contains(t, got.Error(), "could not read", name)
		}
	}
}

// F-9: the preflight is a periodic read of the CRD. The create itself checks too: an
// API server running a schema without spec.provenance prunes it silently, and the
// action it kept would read as one the operator proposed itself.
func TestProcess_AnActionThatLostItsProvenanceIsRemovedAndNotAccepted(t *testing.T) {
	w := newWorld(t, Config{}, baseObjects()...)
	w.p.client = interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if ra, ok := obj.(*dorguv1.RemediationAction); ok {
				ra.Spec.Provenance = nil // what a stale CRD does to the object it returns
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	w.create(t, proposal("p", "alice", "k"))

	out, err := w.p.Process(context.Background(), types.NamespacedName{Namespace: testNS, Name: "p"}, nil)

	require.Error(t, err)
	var notReady *NotReadyError
	require.ErrorAs(t, err, &notReady)
	assert.Equal(t, ReasonActionProvenanceMissing, notReady.Failure.Reason)
	assert.Contains(t, err.Error(), actionCRDFile)
	assert.Nil(t, out)
	assert.Empty(t, actionNames(t, w.c), "the provenance-less action was deleted")
	assert.NotNil(t, w.p.Blocked(), "and the intake closes until the preflight passes again")
}
