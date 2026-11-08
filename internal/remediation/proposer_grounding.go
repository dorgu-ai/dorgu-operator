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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// discloseGroundedBlastRadiusClamp runs the clamp disclosure against live
// values.
//
// discloseBlastRadiusClamp reads Patch versus PrePatchState, and PrePatchState
// holds persona values. With a stale persona that comparison both misses real
// clamps and invents imaginary ones, so the pre-patch fields are swapped for
// the live snapshot for the duration of the call and restored afterwards. The
// disclosure mutates prose and confidence only, never the patches, so the
// restore is exact.
func discloseGroundedBlastRadiusClamp(action *dorguv1.RemediationAction, ref *dorguv1.WorkloadRef) bool {
	if !gate.Resolved(ref) {
		return discloseBlastRadiusClamp(action)
	}

	type saved struct {
		target **apiextensionsv1.JSON
		value  *apiextensionsv1.JSON
	}
	var restore []saved

	swap := func(target **apiextensionsv1.JSON, patch *apiextensionsv1.JSON) {
		grounded := gate.GroundedPrePatch(patch, ref)
		if grounded == nil {
			return
		}
		restore = append(restore, saved{target: target, value: *target})
		*target = grounded
	}

	swap(&action.Spec.Action.PrePatchState, action.Spec.Action.Patch)
	for i := range action.Spec.Steps {
		swap(&action.Spec.Steps[i].PrePatchState, action.Spec.Steps[i].Patch)
	}

	disclosed := discloseBlastRadiusClamp(action)

	for _, s := range restore {
		*s.target = s.value
	}
	return disclosed
}
