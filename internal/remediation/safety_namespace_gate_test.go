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
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
)

// The safety checker is the one deny-list. The diagnosis provider asks it
// through diagnosis.NamespaceGate instead of keeping a copy.
var _ diagnosis.NamespaceGate = (*SafetyCheckerImpl)(nil)

func clusterPersonaExcluding(namespaces ...string) *dorguv1.ClusterPersona {
	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: dorguv1.ClusterPersonaSpec{
			Name: "test-cluster",
			Policies: &dorguv1.ClusterPolicies{
				SelfHealing: &dorguv1.SelfHealingPolicy{ExcludeNamespaces: namespaces},
			},
		},
	}
}

func TestSafetyChecker_NamespaceProtected_AgreesWithNamespaceDenied(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(clusterPersonaExcluding("staging")).Build()
	checker := NewSafetyChecker(c, testLogger(), WithProtectedNamespaces("dorgu-system", "payments"))

	tests := []struct {
		namespace string
		protected bool
	}{
		{"kube-system", true},
		{"dorgu-system", true},
		{"payments", true},
		{"staging", true},
		{"default", false},
		{"apps", false},
		// Fail closed: a namespace that cannot be determined is not safe to send.
		{"", true},
	}
	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			assert.Equal(t, tt.protected, checker.NamespaceProtected(context.Background(), tt.namespace))
			assert.Equal(t, tt.protected, checker.NamespaceDenied(context.Background(), tt.namespace) != nil,
				"the planner gate and the diagnosis gate must never disagree")
		})
	}
}

// An unreadable ClusterPersona means excludeNamespaces is unknown. The built-in
// protected namespaces still hold, and everything else is refused rather than
// guessed at.
func TestSafetyChecker_NamespaceProtected_FailsClosedWhenDenyListUnreadable(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errors.New("apiserver unavailable")
			},
		}).Build()
	checker := NewSafetyChecker(c, testLogger(), WithProtectedNamespaces("dorgu-system"))

	assert.True(t, checker.NamespaceProtected(context.Background(), "kube-system"))
	assert.True(t, checker.NamespaceProtected(context.Background(), "apps"), "unreadable deny-list must fail closed")
	violation := checker.NamespaceDenied(context.Background(), "apps")
	require.NotNil(t, violation)
	assert.Equal(t, RuleDenyList, violation.Rule)
}

type countingLLM struct{ calls int }

func (c *countingLLM) EnhanceDiagnosis(context.Context, llm.DiagnosisRequest) (*llm.DiagnosisResponse, error) {
	c.calls++
	return &llm.DiagnosisResponse{EnhancedSummary: "model narrative"}, nil
}

func (c *countingLLM) Provider() string { return "counting" }

// The wiring cmd/main.go uses: the real checker is the diagnosis provider's
// gate. Every way a namespace becomes protected reaches the model call.
func TestAIProvider_UsesTheSafetyCheckerDenyList(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(clusterPersonaExcluding("staging")).Build()
	checker := NewSafetyChecker(c, testLogger(), WithProtectedNamespaces("dorgu-system", "payments"))

	tests := []struct {
		namespace string
		wantCalls int
	}{
		{"kube-system", 0},
		{"dorgu-system", 0},
		{"payments", 0},
		{"staging", 0},
		{"", 0},
		{"apps", 1},
	}
	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			model := &countingLLM{}
			provider := diagnosis.NewAIProvider(model, checker, logr.Discard())

			_, err := provider.Diagnose(context.Background(), []detection.Signal{{
				Type:       detection.SignalOOMKilled,
				Severity:   detection.SeverityCritical,
				Category:   detection.CategoryResource,
				Source:     "pod-collector",
				Message:    "Container killed due to OOM",
				Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: "worker-abc", Namespace: tt.namespace},
				DetectedAt: time.Now(),
			}})
			require.NoError(t, err)
			assert.Equal(t, tt.wantCalls, model.calls)
		})
	}
}
