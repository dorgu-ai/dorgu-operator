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

package gate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func TestPatchLeafPathsAndValues(t *testing.T) {
	raw := []byte(`{"spec":{"resources":{"limits":{"memory":"1Gi","cpu":null}},"flag":false}}`)

	assert.ElementsMatch(t, []string{memLimit, cpuLimit, "spec.flag"}, PatchLeafPaths(raw))
	assert.Equal(t, map[string]string{memLimit: "1Gi"}, PatchLeafValues(raw), "only string leaves are values")

	assert.Nil(t, PatchLeafPaths(nil))
	assert.Nil(t, PatchLeafPaths([]byte("not json")))
	assert.Empty(t, PatchLeafValues(nil))
	assert.Empty(t, PatchLeafValues([]byte("not json")))
	assert.True(t, PatchTouchesPath(raw, memLimit))
	assert.False(t, PatchTouchesPath(raw, "spec.resources.requests.memory"))
}

func TestParseResourcePatch(t *testing.T) {
	got, err := ParseResourcePatch([]byte(`{"spec":{"resources":{"limits":{"memory":"512Mi","note":"not a quantity!"}}}}`))
	require.NoError(t, err)
	assert.Contains(t, got, memLimit)
	assert.NotContains(t, got, "spec.resources.limits.note")

	_, err = ParseResourcePatch([]byte("{"))
	require.Error(t, err)
}

func TestObservedValueAndResourcePaths(t *testing.T) {
	ref := &dorguv1.WorkloadRef{Name: "web", ObservedResources: &dorguv1.ObservedResources{
		Limits:   &dorguv1.ResourceValues{Memory: "128Mi"},
		Requests: &dorguv1.ResourceValues{CPU: "100m"},
	}}

	v, ok := ObservedValue(ref, memLimit)
	assert.True(t, ok)
	assert.Equal(t, "128Mi", v)
	v, ok = ObservedValue(ref, "spec.resources.requests.cpu")
	assert.True(t, ok)
	assert.Equal(t, "100m", v)

	for _, path := range []string{cpuLimit, "spec.resources.requests.memory", "spec.policies.x", "spec.resources.limits.gpu"} {
		_, ok = ObservedValue(ref, path)
		assert.False(t, ok, path)
	}
	_, ok = ObservedValue(nil, memLimit)
	assert.False(t, ok)
	_, ok = ObservedValue(&dorguv1.WorkloadRef{Name: "web"}, memLimit)
	assert.False(t, ok)
	_, ok = ObservedValue(&dorguv1.WorkloadRef{Name: "web", ObservedResources: &dorguv1.ObservedResources{}}, memLimit)
	assert.False(t, ok)

	assert.True(t, IsResourcePath(memLimit))
	assert.True(t, IsResourcePath("spec.resources.requests.cpu"))
	assert.False(t, IsResourcePath("spec.resources.limits.gpu"))
	assert.False(t, IsResourcePath("spec.policies.security.runAsNonRoot"))
	assert.False(t, Resolved(nil))
	assert.False(t, Resolved(&dorguv1.WorkloadRef{}))
}

func TestGroundedPrePatch(t *testing.T) {
	ref := &dorguv1.WorkloadRef{Name: "web", ObservedResources: &dorguv1.ObservedResources{
		Limits: &dorguv1.ResourceValues{Memory: "128Mi"},
	}}
	got := GroundedPrePatch(jsonOf(`{"spec":{"resources":{"limits":{"memory":"256Mi","cpu":"1"}}}}`), ref)
	require.NotNil(t, got)
	assert.Equal(t, map[string]string{memLimit: "128Mi"}, PatchLeafValues(got.Raw), "live values at the paths the patch touches")

	assert.Nil(t, GroundedPrePatch(nil, ref))
	assert.Nil(t, GroundedPrePatch(jsonOf(`{}`), ref))
	assert.Nil(t, GroundedPrePatch(memPatch("1Gi"), nil))
	assert.Nil(t, GroundedPrePatch(jsonOf(`{"spec":{"resources":{"limits":{"cpu":"1"}}}}`), ref), "no live value, no snapshot")
}

func TestDropAbsentResourceKeys(t *testing.T) {
	ref := &dorguv1.WorkloadRef{Name: "web", ObservedResources: &dorguv1.ObservedResources{
		Limits: &dorguv1.ResourceValues{Memory: "128Mi"},
	}}
	patch := jsonOf(`{"spec":{"resources":{"limits":{"memory":"200Mi","cpu":"1"}}}}`)

	got, dropped := DropAbsentResourceKeys(patch, ref)
	assert.Equal(t, []string{cpuLimit}, dropped)
	assert.Equal(t, []string{memLimit}, PatchLeafPaths(got.Raw))

	same, dropped := DropAbsentResourceKeys(memPatch("200Mi"), ref)
	assert.Empty(t, dropped)
	assert.Equal(t, `{"spec":{"resources":{"limits":{"memory":"200Mi"}}}}`, string(same.Raw))

	untouched, dropped := DropAbsentResourceKeys(patch, nil)
	assert.Empty(t, dropped, "an unread workload cannot be said to lack a key")
	assert.Equal(t, patch, untouched)

	none, dropped := DropAbsentResourceKeys(jsonOf(`{"spec":{"resources":{"limits":{"cpu":"1"}}}}`), ref)
	assert.Equal(t, []string{cpuLimit}, dropped)
	assert.Nil(t, none)
}

func TestPrunePatchPaths(t *testing.T) {
	patch := jsonOf(`{"spec":{"resources":{"limits":{"memory":"1","cpu":"2"}}}}`)

	got := PrunePatchPaths(patch, []string{cpuLimit})
	assert.Equal(t, []string{memLimit}, PatchLeafPaths(got.Raw))
	assert.Nil(t, PrunePatchPaths(patch, []string{cpuLimit, memLimit}), "nothing surviving is nil")
	assert.Equal(t, patch, PrunePatchPaths(patch, nil))
	assert.Nil(t, PrunePatchPaths(nil, []string{memLimit}))
	bad := jsonOf("{")
	assert.Equal(t, bad, PrunePatchPaths(bad, []string{memLimit}))
	assert.Equal(t, PatchLeafValues(patch.Raw), PatchLeafValues(PrunePatchPaths(patch, []string{"spec.nothing.here"}).Raw),
		"an absent path is a no-op")
}

func TestMergePatchesAndMaps(t *testing.T) {
	base := jsonOf(`{"spec":{"resources":{"limits":{"memory":"1Gi","cpu":"1"}}}}`)
	overlay := jsonOf(`{"spec":{"resources":{"limits":{"memory":"2Gi"}}}}`)

	got := MergePatches(base, overlay)
	assert.Equal(t, map[string]string{memLimit: "2Gi", cpuLimit: "1"}, PatchLeafValues(got.Raw), "overlay wins where both set")

	assert.Equal(t, overlay, MergePatches(nil, overlay))
	assert.Equal(t, overlay, MergePatches(jsonOf(""), overlay))
	assert.Equal(t, base, MergePatches(base, nil))
	assert.Equal(t, overlay, MergePatches(jsonOf("{"), overlay))
	assert.Equal(t, base, MergePatches(base, jsonOf("{")))

	merged := MergeMaps(
		map[string]any{"a": map[string]any{"x": "1", "y": "2"}, "keep": "k", "drop": "d", "scalar": "s"},
		map[string]any{"a": map[string]any{"y": "3"}, "drop": nil, "scalar": map[string]any{"now": "map"}, "new": map[string]any{"n": "1"}},
	)
	assert.Equal(t, map[string]any{
		"a": map[string]any{"x": "1", "y": "3"}, "keep": "k", "scalar": map[string]any{"now": "map"},
		"new": map[string]any{"n": "1"},
	}, merged)
}

func TestSnapshotPrePatch(t *testing.T) {
	p := persona("256Mi")

	got := SnapshotPrePatch(p, []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi","gpu":"1"}}}}`))
	require.NotNil(t, got)
	assert.Equal(t, map[string]string{memLimit: "256Mi"}, PatchLeafValues(got), "the persona's current value at each path it has")

	assert.Nil(t, SnapshotPrePatch(nil, []byte(`{}`)))
	assert.Nil(t, SnapshotPrePatch(p, []byte("{")))
	assert.Nil(t, SnapshotPrePatch(p, []byte(`{"spec":{"nothing":"here"}}`)))
}

func TestNarration(t *testing.T) {
	assert.Equal(t, "a", HumanFieldList([]string{"a"}))
	assert.Equal(t, "a and b", HumanFieldList([]string{"a", "b"}))
	assert.Equal(t, "a, b and c", HumanFieldList([]string{"a", "b", "c"}))
	assert.Equal(t, "the proposed value", HumanFieldList(nil))

	assert.Equal(t, "Old. New.", AppendNote("Old.", "New."))
	assert.Equal(t, "New.", AppendNote("", "New."))
	assert.Equal(t, "Old", AppendNote("Old", ""))
	assert.Equal(t, "Old. New.", AppendNote("Old", "New."), "a missing full stop is supplied")

	assert.Equal(t, "it", pluralField(1))
	assert.Equal(t, "them", pluralField(2))

	assert.Equal(t, "1.5x", FormatRatio(1.5))
	assert.Contains(t, BlastRadiusClampedMessage(dorguv1.StepSafety{Field: memLimit, Requested: "9Gi", Ratio: "9.0x", Baseline: "1Gi", MaxRatio: "2.0x", Permitted: "2Gi"}), "apply 2Gi instead")
	assert.Contains(t, BlastRadiusRejectedMessage(dorguv1.StepSafety{Field: memLimit, Requested: "9Gi", Ratio: "9.0x", Baseline: "1Gi", MaxRatio: "2.0x"}), "refused")
	assert.Contains(t, DerivedMessage(memLimit, "2Gi"), "2Gi")

	assert.Equal(t, "Base. Msg1 Msg2", DorguAuthoredDescription("Base.", []dorguv1.StepSafety{{Message: "Msg1"}, {Message: "Msg2"}}))
	assert.Equal(t, "This step applies nothing.", DorguAuthoredDescription("", nil))
	assert.NotEmpty(t, GuardedRationale())

	assert.Empty(t, describeStepPatch(nil))
	assert.Empty(t, describeStepPatch(jsonOf(`{"spec":{"flag":false}}`)))
	assert.Equal(t, "Set spec.resources.limits.cpu to 1 and spec.resources.limits.memory to 2 on the ApplicationPersona.",
		describeStepPatch(jsonOf(`{"spec":{"resources":{"limits":{"memory":"2","cpu":"1"}}}}`)))
}

func TestRefuseStepFieldsRecordsTheVerdictAndSpeaksForTheStep(t *testing.T) {
	step := &dorguv1.RemediationStep{
		Order: 1, ID: "step-1", Type: dorguv1.StepTypePersonaUpdate, AutoExecutable: true,
		Description:   "Raise the limit to 128Mi, well within a 2x ceiling",
		Rationale:     "the persona's recorded intent is 128Mi",
		Patch:         memPatch("128Mi"),
		PrePatchState: memPatch("8Mi"),
	}

	refuseStepFields(step, []Violation{{
		Rule:    RuleBlastRadius,
		Message: "resource change for spec.resources.limits.memory exceeds maximum: 16.0x increase (max 2.0x)",
		BlastRadius: &BlastRadiusViolation{
			Field: memLimit, Baseline: "8Mi", Requested: "128Mi", Ratio: 16, MaxRatio: 2,
		},
	}})

	assert.False(t, step.AutoExecutable)
	assert.Nil(t, step.Patch, "a refused field leaves nothing to apply")
	assert.Nil(t, step.PrePatchState)
	require.Len(t, step.Safety, 1)
	assert.Equal(t, dorguv1.SafetyVerdictRejected, step.Safety[0].Verdict)
	assert.Equal(t, "16.0x", step.Safety[0].Ratio)
	assert.Equal(t, "2.0x", step.Safety[0].MaxRatio)
	assert.Empty(t, step.Safety[0].Permitted)
	assert.Equal(t, "This step applies nothing. "+step.Safety[0].Message, step.Description)
	assert.NotContains(t, step.Description, "well within a 2x ceiling")
	assert.NotContains(t, step.Rationale, "128Mi")

	untouched := &dorguv1.RemediationStep{Description: "unchanged"}
	refuseStepFields(untouched, nil)
	assert.Equal(t, "unchanged", untouched.Description)
}

func TestBlastNumbersWithoutDetail(t *testing.T) {
	assert.Nil(t, blastNumbers(Violation{Rule: RuleBlastRadius}))
}

func TestAbsentFieldRecord(t *testing.T) {
	step := &dorguv1.RemediationStep{}
	ref := &dorguv1.WorkloadRef{Name: "web", Container: "app"}
	recordAbsentFields(step, jsonOf(`{"spec":{"resources":{"limits":{"cpu":"2"}}}}`), []string{cpuLimit}, ref)
	require.Len(t, step.Safety, 1)
	assert.Equal(t, dorguv1.SafetyRuleAbsentField, step.Safety[0].Rule)
	assert.Equal(t, "2", step.Safety[0].Requested)
	assert.Contains(t, AbsentKeyNote(ref, []string{cpuLimit, "spec.resources.requests.memory"}), "them")
	_ = apiextensionsv1.JSON{}
}
