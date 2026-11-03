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

package remediation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Ported from dorgu-platform-v1 internal/view/step_command_test.go, together
// with the parser it pins; keep the operator, CLI and platform copies in step.
//
// The verb used to be found by scanning for the first token that matched a
// known verb name, so a flag value that happened to be a verb name decided the
// answer: `kubectl -n logs delete deployment/foo` classified as read-only,
// because "logs" was reached before "delete", and ownership shaping then left a
// delete in a plan for a Helm-owned workload. Every case below is pinned so
// that cannot come back.

func TestReadsOnlyClassifiesTheVerbPositionally(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		// The bypass, in the forms that reached it.
		{"a namespace named after a read-only verb cannot hide a delete",
			"kubectl -n logs delete deployment/foo", false},
		{"a namespace named get cannot hide a patch",
			"kubectl --namespace get patch deploy/checkout", false},
		{"an output value named describe cannot hide a delete",
			"kubectl -o describe delete ns/prod", false},
		{"a context named top cannot hide a drain",
			"kubectl --context top drain node-a", false},

		// The same shapes, honestly read-only.
		{"a namespace named after a verb still allows a real read",
			"kubectl -n logs logs deploy/checkout", true},
		{"a global flag before a read verb", "kubectl -n apps get pods", true},
		{"a global flag before a write verb", "kubectl -n apps patch deploy/x", false},

		// Ordinary forms.
		{"verb first, read", "kubectl get pods -n apps", true},
		{"verb first, write", "kubectl delete pod/x -n apps", false},
		{"logs", "kubectl logs deploy/checkout -n apps", true},
		{"events", "kubectl events -n apps --for pod/x", true},
		{"top", "kubectl top nodes", true},
		{"describe", "kubectl describe deploy/checkout -n apps", true},

		// Flags that carry their own value.
		{"equals form on a value-taking flag", "kubectl --namespace=logs delete deploy/x", false},
		{"equals form before a read verb", "kubectl --namespace=apps get pods", true},
		{"boolean global flag", "kubectl --insecure-skip-tls-verify get pods", true},
		{"boolean global flag before a write", "kubectl --insecure-skip-tls-verify delete pod/x", false},

		// Refused because they cannot be read with certainty.
		{"an unrecognised flag is refused rather than guessed at",
			"kubectl --made-up-flag get pods", false},
		{"an unrecognised equals flag is refused",
			"kubectl --made-up-flag=x get pods", false},
		{"a trailing flag after the verb does not change the verb", "kubectl get pods -n", true},
		{"a value-taking flag with no value and no verb is refused", "kubectl -n", false},
		{"an unknown verb is refused", "kubectl frobnicate deploy/x", false},
		{"no verb at all", "kubectl --insecure-skip-tls-verify", false},
		{"not kubectl", "helm upgrade checkout ./chart", false},
		{"bare kubectl", "kubectl", false},
		{"empty", "", false},

		// rollout splits on its subcommand, and a bare rollout names none.
		{"rollout status reads", "kubectl rollout status deploy/checkout -n apps", true},
		{"rollout history reads", "kubectl rollout history deploy/checkout", true},
		{"rollout restart writes", "kubectl rollout restart deploy/checkout", false},
		{"rollout undo writes", "kubectl rollout undo deploy/checkout", false},
		{"rollout pause writes", "kubectl rollout pause deploy/checkout", false},
		{"a bare rollout cannot be classified", "kubectl rollout", false},
		{"rollout with only flags cannot be classified", "kubectl rollout --revision=2", false},

		// auth splits on its subcommand too: reconcile writes RBAC.
		{"auth can-i reads", "kubectl auth can-i get pods -n apps", true},
		{"auth whoami reads", "kubectl auth whoami", true},
		{"auth reconcile writes RBAC", "kubectl auth reconcile -f rbac.yaml", false},
		{"auth with a flag before reconcile still writes", "kubectl auth --dry-run=none reconcile -f rbac.yaml", false},
		{"an unknown auth subcommand is refused", "kubectl auth frobnicate", false},
		{"a bare auth names no subcommand", "kubectl auth", false},

		// A flag before the subcommand could be hiding one in its value, the
		// same shape as the original verb bypass, so it is refused.
		{"a namespace named status cannot hide a rollout restart",
			"kubectl rollout -n status restart deploy/checkout", false},
		{"a namespace named can-i cannot hide an auth reconcile",
			"kubectl auth -n can-i reconcile -f rbac.yaml", false},
		{"a global flag before rollout status", "kubectl -n apps rollout status deploy/x", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, readsOnly(tt.command), "readsOnly")

			// writesWorkload is the inverse for anything that is a kubectl
			// invocation. A non-kubectl command is not a workload write here:
			// SanitizeStepCommand already drops it.
			wantWrite := !tt.want
			if !isKubectlInvocation(tt.command) {
				wantWrite = false
			}
			assert.Equal(t, wantWrite, writesWorkload(tt.command), "writesWorkload")
		})
	}
}
