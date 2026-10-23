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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func denyListVerdict(t *testing.T, checker *SafetyCheckerImpl, namespace string) *SafetyViolation {
	t.Helper()
	result, err := checker.Check(context.Background(), newTestAction(namespace))
	require.NoError(t, err)
	for i := range result.Violations {
		if result.Violations[i].Rule == RuleDenyList {
			return &result.Violations[i]
		}
	}
	return nil
}

func TestSafetyChecker_DenyList_ProtectedNamespaces(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	checker := NewSafetyChecker(c, testLogger(), WithProtectedNamespaces("dorgu-system", "payments", ""))

	tests := []struct {
		namespace string
		denied    bool
	}{
		{"kube-system", true},
		{"dorgu-system", true},
		{"payments", true},
		{"default", false},
		// The old hardcoded name is no longer special: the operator's real
		// namespace is what is protected.
		{"dorgu-operator-system", false},
	}
	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			v := denyListVerdict(t, checker, tt.namespace)
			if tt.denied {
				require.NotNil(t, v, "expected %s to be refused", tt.namespace)
				assert.Contains(t, v.Message, tt.namespace)
				return
			}
			assert.Nil(t, v, "expected %s to be allowed", tt.namespace)
		})
	}
}

func TestSafetyChecker_DenyList_KubeSystemAlwaysProtected(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	checker := NewSafetyChecker(c, testLogger())

	assert.NotNil(t, denyListVerdict(t, checker, "kube-system"))
}

func TestOperatorNamespace_FromPodNamespaceEnv(t *testing.T) {
	t.Setenv(OperatorNamespaceEnv, "dorgu-system")

	assert.Equal(t, "dorgu-system", OperatorNamespace())
}

func TestOperatorNamespace_FallsBackToServiceAccountFile(t *testing.T) {
	t.Setenv(OperatorNamespaceEnv, "")
	path := filepath.Join(t.TempDir(), "namespace")
	require.NoError(t, os.WriteFile(path, []byte("ops\n"), 0o600))
	restore := serviceAccountNamespaceFile
	serviceAccountNamespaceFile = path
	t.Cleanup(func() { serviceAccountNamespaceFile = restore })

	assert.Equal(t, "ops", OperatorNamespace())
}

func TestOperatorNamespace_UnknownIsEmpty(t *testing.T) {
	t.Setenv(OperatorNamespaceEnv, "")
	restore := serviceAccountNamespaceFile
	serviceAccountNamespaceFile = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { serviceAccountNamespaceFile = restore })

	assert.Empty(t, OperatorNamespace())
}
