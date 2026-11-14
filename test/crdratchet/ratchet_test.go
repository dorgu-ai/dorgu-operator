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

// Package crdratchet proves CRD validation ratcheting (task 2.9b): a
// RemediationAction stored under the v0.13 step-command rule, which let shell
// metacharacters through, stays updatable after the CRD is upgraded to the
// tightened rule, while no new object and no edited command can use them.
package crdratchet

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const crdName = "remediationactions.dorgu.io"

// oldStyleCommands pass the v0.13.1 rule (it only checks the kubectl prefix) and
// fail the tightened one.
var oldStyleCommands = map[string]string{
	"pipe":         "kubectl get pods -n shop | grep web",
	"chain":        "kubectl get pods; kubectl get nodes",
	"substitution": "kubectl delete pod $(kubectl get pods -o name)",
	"backtick":     "kubectl get pods `whoami`",
	"redirect":     "kubectl get secrets -A > /tmp/x",
	"newline":      "kubectl get pods\nrm -rf /",
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, apiextensionsv1.AddToScheme(s))
	require.NoError(t, dorguv1.AddToScheme(s))
	return s
}

func remediation(name, command string) *dorguv1.RemediationAction {
	return &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{Name: "inc", Namespace: "default"},
			PersonaRef:  dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "web", Namespace: "default"},
			Action:      dorguv1.RemediationActionDetail{Type: dorguv1.ActionTypeNotification},
			Explanation: "look at it",
			Confidence:  "0.85",
			Steps: []dorguv1.RemediationStep{{
				Order: 1, ID: "s1", Type: dorguv1.StepTypeManual, Description: "run this", Command: command,
			}},
		},
	}
}

func startEnv(t *testing.T, crdDir string) client.Client {
	t.Helper()
	env := &envtest.Environment{CRDDirectoryPaths: []string{crdDir}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	require.NoError(t, err, "starting envtest (run through `make test` so KUBEBUILDER_ASSETS is set)")
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: scheme(t)})
	require.NoError(t, err)
	return c
}

// upgradeCRD replaces the served schema with the one in config/crd/bases, the way
// `helm upgrade` or `kubectl apply` does, and waits until the new rule is enforced.
func upgradeCRD(t *testing.T, c client.Client) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "dorgu.io_remediationactions.yaml"))
	require.NoError(t, err)
	var next apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &next))

	ctx := context.Background()
	var live apiextensionsv1.CustomResourceDefinition
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: crdName}, &live))
	live.Spec = next.Spec
	require.NoError(t, c.Update(ctx, &live))

	require.Eventually(t, func() bool {
		probe := remediation("probe-"+time.Now().Format("150405.000000"), "kubectl get pods | cat")
		err := c.Create(ctx, probe)
		if err == nil {
			_ = c.Delete(ctx, probe)
			return false
		}
		return apierrors.IsInvalid(err)
	}, 30*time.Second, 250*time.Millisecond, "the tightened rule never took effect")
}

func TestRatchet_AnOldStyleActionStaysUpdatableAfterTheCRDIsTightened(t *testing.T) {
	c := startEnv(t, filepath.Join("testdata"))
	ctx := context.Background()

	for name, command := range oldStyleCommands {
		require.NoError(t, c.Create(ctx, remediation("old-"+name, command)), "control: the v0.13.1 CRD stores %s", name)
	}

	upgradeCRD(t, c)

	t.Run("a new object with the same command is refused", func(t *testing.T) {
		for name, command := range oldStyleCommands {
			err := c.Create(ctx, remediation("new-"+name, command))
			require.Error(t, err, name)
			assert.True(t, apierrors.IsInvalid(err), "%s: %v", name, err)
			assert.Contains(t, err.Error(), "step command", name)
		}
	})

	t.Run("a new, clean object is accepted", func(t *testing.T) {
		require.NoError(t, c.Create(ctx, remediation("new-clean", "kubectl get pods -n shop")))
	})

	for name := range oldStyleCommands {
		key := client.ObjectKey{Name: "old-" + name, Namespace: "default"}

		t.Run("status of "+name+" can still be written", func(t *testing.T) {
			var ra dorguv1.RemediationAction
			require.NoError(t, c.Get(ctx, key, &ra))
			ra.Status.Phase = "Approved"
			ra.Status.ApprovedBy = "alice@example.com"
			require.NoError(t, c.Status().Update(ctx, &ra), "ratcheting must leave the stored command alone")
		})

		t.Run("status patch of "+name+" can still be written", func(t *testing.T) {
			var ra dorguv1.RemediationAction
			require.NoError(t, c.Get(ctx, key, &ra))
			patch := client.MergeFrom(ra.DeepCopy())
			ra.Status.Phase = "Rejected"
			require.NoError(t, c.Status().Patch(ctx, &ra, patch))
		})

		t.Run("metadata of "+name+" can still be written", func(t *testing.T) {
			var ra dorguv1.RemediationAction
			require.NoError(t, c.Get(ctx, key, &ra))
			ra.Annotations = map[string]string{"example.com/note": "kept"}
			ra.Finalizers = nil
			require.NoError(t, c.Update(ctx, &ra))
		})

		t.Run("another spec field of "+name+" can still be edited", func(t *testing.T) {
			var ra dorguv1.RemediationAction
			require.NoError(t, c.Get(ctx, key, &ra))
			ra.Spec.Explanation = "edited, command untouched"
			require.NoError(t, c.Update(ctx, &ra), "an unchanged command is ratcheted")
		})

		t.Run("the command of "+name+" cannot be edited into another unsafe one", func(t *testing.T) {
			var ra dorguv1.RemediationAction
			require.NoError(t, c.Get(ctx, key, &ra))
			ra.Spec.Steps[0].Command = "kubectl get nodes | sh"
			err := c.Update(ctx, &ra)
			require.Error(t, err)
			assert.True(t, apierrors.IsInvalid(err), err.Error())
		})

		t.Run("the command of "+name+" can be edited into a safe one", func(t *testing.T) {
			var ra dorguv1.RemediationAction
			require.NoError(t, c.Get(ctx, key, &ra))
			ra.Spec.Steps[0].Command = "kubectl get nodes"
			require.NoError(t, c.Update(ctx, &ra))
		})
	}

	t.Run("a merge patch of the status subresource through the raw API", func(t *testing.T) {
		var ra dorguv1.RemediationAction
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "old-pipe", Namespace: "default"}, &ra))
		require.NoError(t, c.Status().Patch(ctx, &ra, client.RawPatch(types.MergePatchType,
			[]byte(`{"status":{"phase":"Failed"}}`))))
	})
}
