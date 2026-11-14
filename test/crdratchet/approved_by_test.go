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
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// F-3: status.approvedBy has the bound of the ticket's copy, so an approval the
// ticket could not carry is refused where the approver writes it.
func TestApprovedBy_TheStatusFieldHasTheTicketsBound(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ctx := context.Background()
	ra := remediation("approver-bound", "kubectl get pods")
	require.NoError(t, c.Create(ctx, ra))

	ra.Status.ApprovedBy = strings.Repeat("a", dorguv1.MaxApprovedByLength+1)
	err := c.Status().Update(ctx, ra)
	require.Error(t, err, "an approver longer than the ticket can carry must be refused at the write")
	assert.True(t, apierrors.IsInvalid(err), err.Error())
	assert.Contains(t, err.Error(), "approvedBy")

	ra.Status.ApprovedBy = strings.Repeat("a", dorguv1.MaxApprovedByLength)
	require.NoError(t, c.Status().Update(ctx, ra), "exactly the bound is fine")
}

// The hash a ticket records is the hash of the spec as the API server serves it
// (P2-D14): the typed spec the operator reads and the unstructured spec a CLI or
// dashboard reads must give one value, defaults and normalised numbers included.
func TestSpecHash_TheTypedSpecAndTheServedSpecHashAlike(t *testing.T) {
	c := startEnv(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ctx := context.Background()
	ra := remediation("hash-served", "kubectl get pods")
	ra.Spec.Steps = append(ra.Spec.Steps, dorguv1.RemediationStep{
		Order: 2, ID: "s2", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true, Description: "raise memory",
		Patch: &apiextensionsv1.JSON{Raw: []byte(
			`{"spec":{"resources":{"limits":{"memory":"512Mi","weight":1.0,"big":12345678901234567890}}}}`)},
	})
	ra.Spec.Approval = &dorguv1.ApprovalSpec{Required: true}
	ra.Spec.Rollback = &dorguv1.RemediationRollbackSpec{Enabled: true}
	require.NoError(t, c.Create(ctx, ra))

	typed := &dorguv1.RemediationAction{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ra), typed))
	typedHash, err := typed.Spec.Hash()
	require.NoError(t, err)

	served := &unstructured.Unstructured{}
	served.SetGroupVersionKind(dorguv1.GroupVersion.WithKind("RemediationAction"))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ra), served))
	spec, ok := served.Object["spec"].(map[string]any)
	require.True(t, ok)
	servedHash, err := dorguv1.SpecHashFromMap(spec)
	require.NoError(t, err)

	assert.Equal(t, servedHash, typedHash,
		"the operator and a client reading the served spec must compute the same specHash")
	// What the author wrote differs from what is stored (1.0 is 1, the big integer
	// is a double), and the canonical form does not care.
	authoredJSON, err := json.Marshal(ra.Spec)
	require.NoError(t, err)
	authored, err := dorguv1.SpecHashFromJSON(authoredJSON)
	require.NoError(t, err)
	assert.Equal(t, servedHash, authored)
}
