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

package chart

import (
	"os"
	"regexp"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// R5 of the #54 adversarial review: excludeNamespaces entries are namespace
// names, so the API server rejects anything that is not a DNS-1123 label. A
// capital letter, a wildcard or a stray space can never match a namespace, and
// used to protect nothing while looking as if it did.
const dns1123Label = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`

func excludeNamespacesItems(t *testing.T, path string) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	policy := spec.Properties["policies"].Properties["selfHealing"]
	items := policy.Properties["excludeNamespaces"].Items
	if items == nil || items.Schema == nil {
		t.Fatalf("%s: excludeNamespaces has no item schema", path)
	}
	return *items.Schema
}

func TestExcludeNamespacesItemsAreDNS1123Labels(t *testing.T) {
	for _, path := range []string{
		"../../config/crd/bases/dorgu.io_clusterpersonas.yaml",
		"crds/dorgu.io_clusterpersonas.yaml",
	} {
		item := excludeNamespacesItems(t, path)
		if item.Pattern != dns1123Label {
			t.Errorf("%s: excludeNamespaces item pattern = %q, want %q", path, item.Pattern, dns1123Label)
		}
		if item.MaxLength == nil || *item.MaxLength != 63 {
			t.Errorf("%s: excludeNamespaces item maxLength = %v, want 63", path, item.MaxLength)
		}
		re := regexp.MustCompile(item.Pattern)
		for _, bad := range []string{" shop", "Shop", "shop ", "*", "a.b", "-a", "a-", ""} {
			if re.MatchString(bad) {
				t.Errorf("%s: pattern accepts %q", path, bad)
			}
		}
		if !re.MatchString("shop") || !re.MatchString("a-b-1") {
			t.Errorf("%s: pattern rejects a valid namespace name", path)
		}
	}
}
