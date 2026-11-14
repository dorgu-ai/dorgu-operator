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

package v1

import (
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func loadCRD(t *testing.T, file string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", file))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	return &crd
}

func stepItems(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	steps, ok := root.Properties["spec"].Properties["steps"]
	if !ok || steps.Items == nil || steps.Items.Schema == nil {
		t.Fatalf("%s has no spec.steps items schema", crd.Name)
	}
	return *steps.Items.Schema
}

// F-10: every free-text field of a step is bounded in the CRD, so one proposal cannot
// approach the API server's request limit through step text. RemediationAction steps
// are the same type, so a plan the operator drafts is held to the same bounds.
func TestCRDs_EveryFreeTextStepFieldHasAMaxLength(t *testing.T) {
	want := map[string]int64{"id": 128, "description": 1024, "rationale": 4000, "command": 1024}
	for _, file := range []string{"dorgu.io_remediationproposals.yaml", "dorgu.io_remediationactions.yaml"} {
		crd := loadCRD(t, file)
		step := stepItems(t, crd)
		for field, max := range want {
			prop, ok := step.Properties[field]
			if !ok {
				t.Fatalf("%s: step has no %q", crd.Name, field)
			}
			if prop.MaxLength == nil || *prop.MaxLength != max {
				t.Errorf("%s: step.%s maxLength = %v, want %d", crd.Name, field, prop.MaxLength, max)
			}
		}
		safety := step.Properties["safety"]
		if safety.MaxItems == nil || *safety.MaxItems != 16 {
			t.Errorf("%s: step.safety maxItems = %v, want 16", crd.Name, safety.MaxItems)
		}
		if safety.Items == nil || safety.Items.Schema == nil {
			t.Fatalf("%s: step.safety has no items schema", crd.Name)
		}
		for _, field := range []string{"field", "baseline", "requested", "permitted", "ratio", "maxRatio"} {
			prop := safety.Items.Schema.Properties[field]
			if prop.MaxLength == nil || *prop.MaxLength != 253 {
				t.Errorf("%s: step.safety.%s maxLength = %v, want 253", crd.Name, field, prop.MaxLength)
			}
		}
		if m := safety.Items.Schema.Properties["message"]; m.MaxLength == nil || *m.MaxLength != 1024 {
			t.Errorf("%s: step.safety.message maxLength = %v, want 1024", crd.Name, m.MaxLength)
		}
	}
}
