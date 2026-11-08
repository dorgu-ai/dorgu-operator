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
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
)

// Shared fixtures for the proposer tests.

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = dorguv1.AddToScheme(s)
	return s
}

func testLogger() logr.Logger {
	return zap.New(zap.UseDevMode(true))
}

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

type countingLLM struct{ calls int }

func (c *countingLLM) EnhanceDiagnosis(context.Context, llm.DiagnosisRequest) (*llm.DiagnosisResponse, error) {
	c.calls++
	return &llm.DiagnosisResponse{EnhancedSummary: "model narrative"}, nil
}

func (c *countingLLM) Provider() string { return "counting" }

// The wiring cmd/main.go uses: the real checker is the diagnosis provider's
// gate. Every way a namespace becomes protected reaches the model call.
func TestAIProvider_UsesTheGateDenyList(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithRuntimeObjects(clusterPersonaExcluding("staging")).Build()
	checker := gate.New(c, testLogger(), gate.WithProtectedNamespaces("dorgu-system", "payments"))

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

// assertAppliable is the completeness invariant (CR-01) expressed over a plan and
// a diagnosis: an error when a resource change was diagnosed and no step applies
// it. The gate enforces the same rule on the final plan; this is the test-side
// reading of it.
func assertAppliable(action *dorguv1.RemediationAction, diag diagnosis.Diagnosis, ruleEngineDeclined bool) error {
	target := diagnosisTargetPath(diag)
	if ruleEngineDeclined || target == "" ||
		gate.AppliesDiagnosedChange(action.Spec.Steps, action.Spec.Action, target) {
		return nil
	}
	return fmt.Errorf("a plan that applies nothing for a diagnosed change to %s", target)
}
