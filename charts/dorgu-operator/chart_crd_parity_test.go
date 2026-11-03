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
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Every CRD controller-gen writes to config/crd/bases has to reach both install
// paths. The kustomize list once stopped at the two persona CRDs, so `make
// deploy` (and the e2e suite) installed an operator whose incident, remediation
// and event controllers could never sync their caches. The chart had all five,
// so nothing that used Helm noticed.

func crdBaseFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("../../config/crd/bases/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no generated CRDs found in config/crd/bases: %v", err)
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	return names
}

func TestKustomizeInstallsEveryCRD(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/kustomization.yaml")
	if err != nil {
		t.Fatalf("reading config/crd/kustomization.yaml: %v", err)
	}
	var kustomization struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kustomization); err != nil {
		t.Fatalf("parsing config/crd/kustomization.yaml: %v", err)
	}

	for _, base := range crdBaseFiles(t) {
		if !slices.Contains(kustomization.Resources, "bases/"+base) {
			t.Errorf("config/crd/kustomization.yaml does not install %s", base)
		}
	}
}

func TestChartShipsEveryCRD(t *testing.T) {
	for _, base := range crdBaseFiles(t) {
		want, err := os.ReadFile(filepath.Join("../../config/crd/bases", base))
		if err != nil {
			t.Fatalf("reading %s: %v", base, err)
		}
		got, err := os.ReadFile(filepath.Join("crds", base))
		if err != nil {
			t.Errorf("chart crds/ is missing %s", base)
			continue
		}
		if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
			t.Errorf("chart crds/%s differs from config/crd/bases; copy the regenerated CRD into the chart", base)
		}
	}
}
