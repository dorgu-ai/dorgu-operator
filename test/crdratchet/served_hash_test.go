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
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// N-1 of the C3 re-review: the hash a ticket records must be the hash of the spec
// as the API server SERVES it (P2-D14), whatever way a client wrote it. Hashing the
// typed Go struct is not that: the struct drops empty arrays (omitempty), re-spells
// durations ("5m" is "5m0s") and truncates or re-zones timestamps.
//
// Each case below is one of the reviewer's divergent inputs. Every one is written
// two ways, a plain Create (kubectl create) and server-side apply, and both must
// give a ticket hash equal to the hash of the served .spec, and an apply-time check
// that never calls the unchanged spec SpecChanged.

type divergentSpec struct {
	name   string
	mutate func(spec map[string]any)
}

func divergentSpecs() []divergentSpec {
	return []divergentSpec{
		{"empty steps", func(spec map[string]any) { spec["steps"] = []any{} }},
		{"empty step safety", func(spec map[string]any) {
			spec["steps"].([]any)[0].(map[string]any)["safety"] = []any{}
		}},
		{"healthCheckAfter 5m", func(spec map[string]any) {
			spec["rollback"] = map[string]any{"enabled": true, "maxRetries": 1, "healthCheckAfter": "5m"}
		}},
		{"healthCheckAfter 300s", func(spec map[string]any) {
			spec["rollback"] = map[string]any{"enabled": true, "maxRetries": 1, "healthCheckAfter": "300s"}
		}},
		{"observedAt with fractional seconds", func(spec map[string]any) {
			spec["workloadRef"] = workloadRefObservedAt("2026-11-14T10:00:00.123Z")
		}},
		{"observedAt with a UTC offset", func(spec map[string]any) {
			spec["workloadRef"] = workloadRefObservedAt("2026-11-14T15:30:00+05:30")
		}},
		{"approval deadline with fractional seconds", func(spec map[string]any) {
			spec["approval"] = map[string]any{"required": true, "deadline": "2026-11-14T10:00:00.5Z"}
		}},
		{"everything at once", func(spec map[string]any) {
			spec["steps"].([]any)[0].(map[string]any)["safety"] = []any{}
			spec["rollback"] = map[string]any{"enabled": true, "maxRetries": 1, "healthCheckAfter": "5m"}
			spec["workloadRef"] = workloadRefObservedAt("2026-11-14T15:30:00.25+05:30")
		}},
	}
}

func workloadRefObservedAt(at string) map[string]any {
	return map[string]any{
		"kind": "Deployment", "name": "web", "namespace": "default", "managedBy": "unmanaged", "observedAt": at,
	}
}

// rawRA is a RemediationAction as a client writes it: the typed fixture's JSON,
// altered as the case says, with no typed round trip afterwards.
func rawRA(t *testing.T, name string, mutate func(spec map[string]any)) *unstructured.Unstructured {
	t.Helper()
	typed := remediation(name, "kubectl get pods")
	raw, err := json.Marshal(typed.Spec)
	require.NoError(t, err)
	var spec map[string]any
	require.NoError(t, json.Unmarshal(raw, &spec))
	mutate(spec)

	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(dorguv1.GroupVersion.WithKind("RemediationAction"))
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func servedSpec(t *testing.T, c client.Client, key client.ObjectKey) map[string]any {
	t.Helper()
	served := &unstructured.Unstructured{}
	served.SetGroupVersionKind(dorguv1.GroupVersion.WithKind("RemediationAction"))
	require.NoError(t, c.Get(context.Background(), key, served))
	spec, ok := served.Object["spec"].(map[string]any)
	require.True(t, ok)
	return spec
}

// ticketedFromServer is the typed action the operator reconciles, read back from
// the API server, carrying a ticket whose hash is the given one and the approval
// fields CheckTicket needs to get as far as the spec check.
func ticketedFromServer(t *testing.T, c client.Client, key client.ObjectKey, specHash string) *dorguv1.RemediationAction {
	t.Helper()
	ra := &dorguv1.RemediationAction{}
	require.NoError(t, c.Get(context.Background(), key, ra))
	now := metav1.NewTime(time.Now())
	ra.Status.ApprovedAt = &now
	ra.Status.ApprovedBy = "alice@example.com"
	ra.Status.ApplyTicket = &dorguv1.ApplyTicket{
		SpecHash: specHash, ApprovedBy: "alice@example.com", IssuedAt: &now,
		ExpiresAt: metav1.NewTime(now.Add(time.Minute)), WorkloadUID: "uid-1",
	}
	return ra
}

func TestSpecHash_TheTicketHashIsTheHashOfTheServedSpec_NeverTheTypedOne(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	g := gate.New(c, logr.Discard(), gate.WithReader(c))
	ctx := context.Background()

	for i, tc := range divergentSpecs() {
		for _, how := range []string{"create", "server-side apply"} {
			t.Run(fmt.Sprintf("%s/%s", tc.name, how), func(t *testing.T) {
				name := fmt.Sprintf("served-%d-%s", i, map[string]string{"create": "c", "server-side apply": "s"}[how])
				u := rawRA(t, name, tc.mutate)
				if how == "create" {
					require.NoError(t, c.Create(ctx, u))
				} else {
					require.NoError(t, c.Patch(ctx, u, client.Apply, client.FieldOwner("n1-test"), client.ForceOwnership))
				}
				key := client.ObjectKey{Namespace: "default", Name: name}

				want, err := dorguv1.SpecHashFromMap(servedSpec(t, c, key))
				require.NoError(t, err)

				// Issue: the hash the gate binds into the ticket.
				typed := &dorguv1.RemediationAction{}
				require.NoError(t, c.Get(ctx, key, typed))
				issued, err := g.ServedSpecHash(ctx, typed)
				require.NoError(t, err)
				assert.Equal(t, want, issued, "the issued specHash must be the hash of the served .spec")

				// Apply: a ticket carrying the served hash on an unchanged spec is never SpecChanged.
				problem, err := g.CheckTicket(ctx, ticketedFromServer(t, c, key, want))
				require.NoError(t, err)
				if problem != nil {
					assert.NotEqual(t, gate.GuardrailSpecChanged, problem.Guardrail,
						"an unchanged spec was refused as SpecChanged: %s", problem.Reason)
				}
			})
		}
	}
}

func TestSpecHash_AnEditedSpecIsStillRefusedAsSpecChanged(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	g := gate.New(c, logr.Discard(), gate.WithReader(c))
	ctx := context.Background()

	u := rawRA(t, "edited", func(spec map[string]any) {
		spec["rollback"] = map[string]any{"enabled": true, "maxRetries": 1, "healthCheckAfter": "5m"}
	})
	require.NoError(t, c.Create(ctx, u))
	key := client.ObjectKey{Namespace: "default", Name: "edited"}
	before, err := dorguv1.SpecHashFromMap(servedSpec(t, c, key))
	require.NoError(t, err)

	// The edit a typed hash could not see: 5m to 300s is the same duration to Go and
	// a different document to the API server, and so to every other client.
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(dorguv1.GroupVersion.WithKind("RemediationAction"))
	require.NoError(t, c.Get(ctx, key, live))
	require.NoError(t, unstructured.SetNestedField(live.Object, "300s", "spec", "rollback", "healthCheckAfter"))
	require.NoError(t, c.Update(ctx, live))

	problem, err := g.CheckTicket(ctx, ticketedFromServer(t, c, key, before))
	require.NoError(t, err)
	require.NotNil(t, problem)
	assert.Equal(t, gate.GuardrailSpecChanged, problem.Guardrail, problem.Reason)
}

// A spec edit that landed after the operator read the action (a stale cache) is
// never hashed as if it were the spec the gate cleared: at issue the operator
// retries, at apply it refuses.
func TestSpecHash_ASpecNewerThanTheOneInHandIsNeverIssuedAHash(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	g := gate.New(c, logr.Discard(), gate.WithReader(c))
	ctx := context.Background()

	require.NoError(t, c.Create(ctx, rawRA(t, "newer", func(map[string]any) {})))
	key := client.ObjectKey{Namespace: "default", Name: "newer"}
	stale := &dorguv1.RemediationAction{}
	require.NoError(t, c.Get(ctx, key, stale))

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(dorguv1.GroupVersion.WithKind("RemediationAction"))
	require.NoError(t, c.Get(ctx, key, live))
	require.NoError(t, unstructured.SetNestedField(live.Object, "changed after the read", "spec", "explanation"))
	require.NoError(t, c.Update(ctx, live))

	_, err := g.ServedSpecHash(ctx, stale)
	assert.ErrorIs(t, err, gate.ErrSpecMoved, "the spec in hand is older than the served one; no hash may be issued for it")
}
