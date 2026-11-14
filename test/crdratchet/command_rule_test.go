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
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Task 2.9b: the step command CEL rule on the CRD the operator ships. The API
// server is the enforcement point; the positional verb classifier in the gate is
// the second check.

func TestCommandRule_AcceptsASingleLineKubectlInvocation(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ctx := context.Background()

	for i, command := range []string{
		"kubectl get pods -n shop",
		"kubectl set resources deployment/web --limits=memory=1Gi -n shop",
		"kubectl rollout status deployment/web -n shop --timeout=60s",
		"kubectl get pods -l 'app=web'",
		`kubectl get pods -l "app=web"`,
		"kubectl get pod web -o jsonpath={.spec.containers[0].image}",
		"kubectl annotate pod web note=a-b_c.d/e:f=g,h",
	} {
		err := c.Create(ctx, remediation(fmt.Sprintf("ok-%d", i), command))
		assert.NoError(t, err, "%q must be accepted", command)
	}

	// A step with no command at all is a step with no command.
	ra := remediation("no-command", "")
	require.NoError(t, c.Create(ctx, ra))
}

func TestCommandRule_RejectsEveryShellMetacharacterAndNewline(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
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
		"tab":              "kubectl get pods\t-n shop",
		"escape":           "kubectl get pods \x1b[2K",
		"nul":              "kubectl get pods \x00",
		"vertical tab":     "kubectl get pods\x0b-n shop",
		"form feed":        "kubectl get pods\x0c-n shop",
		"delete":           "kubectl get pods\x7f",
		"next line":        "kubectl get pods\u0085-n shop",
		"line separator":   "kubectl get pods\u2028-n shop",
		"paragraph sep":    "kubectl get pods\u2029-n shop",
		"bidi override":    "kubectl get pods -n \u202eevil\u202c shop",
		"bidi isolate":     "kubectl get pods -n \u2067evil\u2069 shop",
		"zero width space": "kubectl get pods -n sh\u200bop",
		"byte order mark":  "kubectl get pods\ufeff",
		"tag character":    "kubectl get pods \U000e0041",
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
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ra := remediation("second-step", "kubectl get pods")
	ra.Spec.Steps = append(ra.Spec.Steps, dorguv1.RemediationStep{
		Order: 2, ID: "s2", Type: dorguv1.StepTypeManual, Description: "x", Command: "kubectl get pods | sh",
	})

	err := c.Create(context.Background(), ra)

	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), err.Error())
}

func TestCommandRule_ASanitizedCommandAlwaysPassesTheCRD(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
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

// commandCodePoints is every code point the rule is compared on: the C3 review's
// U+0000 to U+2100 and three more, every Unicode format character (category Cf,
// N-3: bidi overrides, zero-width characters, the byte order mark, the tag
// characters, wherever they are in Unicode), and the neighbours of those ranges that
// are not Cf, which must stay accepted.
func commandCodePoints() []rune {
	var points []rune
	for r := rune(0); r <= 0x2100; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		points = append(points, r)
	}
	points = append(points, 0xfeff, 0xfffd, 0x1f600)
	for _, rng := range unicode.Cf.R16 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			points = append(points, r)
		}
	}
	for _, rng := range unicode.Cf.R32 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			points = append(points, r)
		}
	}
	points = append(points, 0x200a, 0x2065, 0xfff8, 0xfffc, 0xe0000, 0xe0080, 0xe000, 0xf8ff, 0x10ffff)
	return slices.Compact(slices.Sorted(slices.Values(points)))
}

// F-7: the rule on the API server and SanitizeStepCommand must agree on every code
// point, so a command the planner lets out is always storable and nothing the API
// server stores is a command the planner would have dropped. The list is the one the
// C3 review probed: U+0000 to U+2100, U+FEFF, U+FFFD and U+1F600. Dry-run creates
// run the full validation without storing anything.
func TestCommandRule_AgreesWithSanitizeStepCommandOnEveryCodePoint(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ctx := context.Background()

	points := commandCodePoints()

	accepted := 0
	for _, r := range points {
		command := "kubectl get pods" + string(r) + "x"
		err := c.Create(ctx, remediation("codepoint", command), client.DryRunAll)
		serverAccepts := err == nil
		if err != nil && !apierrors.IsInvalid(err) {
			t.Fatalf("U+%04X: unexpected error %v", r, err)
		}
		goAccepts := dorguv1.SanitizeStepCommand(command) == command
		if serverAccepts != goAccepts {
			t.Errorf("U+%04X: API server accepts=%v, SanitizeStepCommand accepts=%v", r, serverAccepts, goAccepts)
		}
		if serverAccepts {
			accepted++
			if (r <= 0x1f) || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r) {
				t.Errorf("the API server accepted control, line-break or format character U+%04X", r)
			}
		}
	}
	assert.Greater(t, accepted, 8000, "ordinary characters must still be accepted")
}

// N-3: the rule gained \p{Cf}. A plan at both bounds, 64 steps each with a command
// of the maximum length, must still fit the API server's CEL cost budgets: the
// estimated one when the CRD is installed (startEnv would fail) and the runtime one
// when the object is validated.
func TestCommandRule_FitsTheCELCostBudgetAtTheLargestPlan(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	command := "kubectl get pods " + strings.Repeat("a", dorguv1.MaxStepCommandLength-len("kubectl get pods "))
	require.Len(t, command, dorguv1.MaxStepCommandLength)

	ra := remediation("largest-plan", command)
	ra.Spec.Steps = nil
	for i := 1; i <= dorguv1.MaxPlanSteps; i++ {
		ra.Spec.Steps = append(ra.Spec.Steps, dorguv1.RemediationStep{
			Order: int32(i), ID: fmt.Sprintf("s%d", i), Type: dorguv1.StepTypeManual, Description: "x", Command: command,
		})
	}
	require.NoError(t, c.Create(context.Background(), ra))

	// A format character in the last command of that same plan is refused.
	bad := ra.DeepCopy()
	bad.ObjectMeta = metav1.ObjectMeta{Name: "largest-plan-bad", Namespace: "default"}
	bad.Spec.Steps[len(bad.Spec.Steps)-1].Command = "kubectl get pods " + string(rune(0x202e))
	err := c.Create(context.Background(), bad)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), err.Error())
}
