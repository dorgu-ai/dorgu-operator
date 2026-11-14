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

package crdratchet

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Task 2.9b: the step command CEL rule on the CRD the operator ships. The API
// server is the enforcement point; the positional verb classifier in the gate is
// the second check.

func TestCommandRule_AcceptsASingleLineKubectlInvocation(t *testing.T) {
	_, c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ctx := context.Background()

	for i, command := range []string{
		"kubectl get pods -n shop",
		"kubectl set resources deployment/web --limits=memory=1Gi -n shop",
		"kubectl rollout status deployment/web -n shop --timeout=60s",
		"kubectl get pods -l 'app=web'",
		`kubectl get pods -l "app=web"`,
		"kubectl get pod web -o jsonpath={.spec.containers[0].image}",
		"kubectl annotate pod web note=a-b_c.d/e:f=g,h",
		"kubectl get pods -n shop\t-o wide",
	} {
		err := c.Create(ctx, remediation(fmt.Sprintf("ok-%d", i), command))
		assert.NoError(t, err, "%q must be accepted", command)
	}

	// A step with no command at all is a step with no command.
	ra := remediation("no-command", "")
	require.NoError(t, c.Create(ctx, ra))
}

func TestCommandRule_RejectsEveryShellMetacharacterAndNewline(t *testing.T) {
	_, c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ctx := context.Background()

	forbidden := map[string]string{
		"semicolon":        "kubectl get pods; id",
		"pipe":             "kubectl get pods | sh",
		"ampersand":        "kubectl get pods & id",
		"double ampersand": "kubectl get pods && id",
		"dollar":           "kubectl get pods $HOME",
		"dollar paren":     "kubectl get pods $(id)",
		"backtick":         "kubectl get pods `id`",
		"open paren":       "kubectl get pods (id",
		"close paren":      "kubectl get pods id)",
		"less than":        "kubectl apply -f - < /etc/passwd",
		"greater than":     "kubectl get secrets > /tmp/out",
		"newline":          "kubectl get pods\nid",
		"trailing newline": "kubectl get pods\n",
		"carriage return":  "kubectl get pods\rid",
		"crlf":             "kubectl get pods\r\nid",
		"newline first":    "\nkubectl get pods",
		"no kubectl":       "rm -rf /",
		"kubectl no arg":   "kubectl",
		"kubectl prefix":   "kubectlx get pods",
		"leading space":    " kubectl get pods",
	}
	for name, command := range forbidden {
		t.Run(name, func(t *testing.T) {
			err := c.Create(ctx, remediation("bad-"+safeName(name), command))
			require.Error(t, err, "%q must be refused by the API server", command)
			assert.True(t, apierrors.IsInvalid(err), "%q: %v", command, err)
			assert.Contains(t, err.Error(), "step command")
		})
	}
}

func TestCommandRule_AppliesToEveryStep(t *testing.T) {
	_, c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ra := remediation("second-step", "kubectl get pods")
	ra.Spec.Steps = append(ra.Spec.Steps, dorguv1.RemediationStep{
		Order: 2, ID: "s2", Type: dorguv1.StepTypeManual, Description: "x", Command: "kubectl get pods | sh",
	})

	err := c.Create(context.Background(), ra)

	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), err.Error())
}

func TestCommandRule_ASanitizedCommandAlwaysPassesTheCRD(t *testing.T) {
	_, c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ctx := context.Background()

	// SanitizeStepCommand is what the planner runs a model's command through. Whatever
	// it lets out must be storable, or a plan would be lost at Create.
	inputs := []string{
		"kubectl get pods (x)", "kubectl get pods $(id)", "kubectl get pods; id", "kubectl get pods",
		"  kubectl get pods -n shop  ", "kubectl logs web -n shop --tail=50", "kubectl get pods\nid",
	}
	for i, in := range inputs {
		out := dorguv1.SanitizeStepCommand(in)
		if out == "" {
			continue
		}
		assert.NoError(t, c.Create(ctx, remediation(fmt.Sprintf("sanitized-%d", i), out)), "sanitized %q -> %q", in, out)
	}
}

func safeName(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range []byte(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}
