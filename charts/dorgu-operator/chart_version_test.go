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

// This file guards the committed chart version metadata.
//
// Release CI rewrites Chart.yaml's version/appVersion from the git tag at publish
// time, so the committed values are only ever read by someone installing from a
// clone (`helm install ./charts/dorgu-operator`). When they rot behind the latest
// tag, that install silently runs an old operator image, because
// templates/deployment.yaml defaults the image tag to .Chart.AppVersion.
package chart

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
	"sigs.k8s.io/yaml"
)

// repoRoot is this package's path relative to the repository root.
const repoRoot = "../.."

// chartMeta is the subset of Chart.yaml these tests assert on.
type chartMeta struct {
	Version    string `json:"version"`
	AppVersion string `json:"appVersion"`
}

// readChartMeta parses the chart's version fields.
func readChartMeta(t *testing.T) chartMeta {
	t.Helper()

	raw, err := os.ReadFile("Chart.yaml")
	if err != nil {
		t.Fatalf("reading Chart.yaml: %v", err)
	}

	var meta chartMeta
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("parsing Chart.yaml: %v", err)
	}
	if meta.Version == "" || meta.AppVersion == "" {
		t.Fatalf("Chart.yaml must declare both version and appVersion, got %+v", meta)
	}
	return meta
}

// latestGitTag returns the newest tag reachable from HEAD, or "" when the tag
// history is unavailable (tarball export, or a shallow clone without tags).
func latestGitTag(t *testing.T) string {
	t.Helper()

	out, err := exec.Command("git", "-C", repoRoot, "describe", "--tags", "--abbrev=0").Output()
	if err != nil {
		t.Logf("git describe failed (%v); the tag comparison will be skipped", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// (h) The committed appVersion must not lag the latest release tag — that is what
// makes `helm install ./charts/dorgu-operator` from a clone deploy a stale image.
func TestChartVersionMatchesLatestTag(t *testing.T) {
	meta := readChartMeta(t)

	// Release CI stamps both fields from the same tag, so they must never diverge.
	if meta.Version != meta.AppVersion {
		t.Errorf("Chart.yaml version (%s) and appVersion (%s) must match — release CI stamps both from the git tag",
			meta.Version, meta.AppVersion)
	}

	chartVersion := "v" + strings.TrimPrefix(meta.AppVersion, "v")
	if !semver.IsValid(chartVersion) {
		t.Fatalf("Chart.yaml appVersion %q is not valid semver", meta.AppVersion)
	}

	latest := latestGitTag(t)
	if latest == "" {
		t.Skip("no git tags available; cannot compare the chart appVersion against the latest release")
	}
	if !semver.IsValid(latest) {
		t.Fatalf("latest git tag %q is not valid semver", latest)
	}

	// Ahead of the latest tag is fine: /release bumps Chart.yaml in the release
	// commit, before the tag exists. Behind is the bug.
	if semver.Compare(chartVersion, latest) < 0 {
		t.Errorf("Chart.yaml appVersion %s is behind the latest release tag %s — `helm install ./charts/dorgu-operator` "+
			"from this clone would deploy the %s image. Bump charts/dorgu-operator/Chart.yaml.",
			meta.AppVersion, latest, meta.AppVersion)
	}
}

// (i) The bundled CRDs must match config/crd/bases byte for byte — the same
// invariant release CI enforces with `cp config/crd/bases/*.yaml crds/`. When they
// drift, a clone install gets an operator whose CRDs cannot store the fields its
// controllers write: a missing schema branch is silently pruned by the API server,
// and a missing CRD file means the resource cannot be created at all.
func TestBundledCRDsMatchGeneratedCRDs(t *testing.T) {
	generatedDir := filepath.Join(repoRoot, "config", "crd", "bases")

	generated, err := filepath.Glob(filepath.Join(generatedDir, "*.yaml"))
	if err != nil {
		t.Fatalf("globbing %s: %v", generatedDir, err)
	}
	if len(generated) == 0 {
		t.Fatalf("no generated CRDs found in %s — run 'make manifests'", generatedDir)
	}

	for _, src := range generated {
		name := filepath.Base(src)
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("reading %s: %v", src, err)
			}

			bundled := filepath.Join("crds", name)
			got, err := os.ReadFile(bundled)
			if err != nil {
				t.Fatalf("%s is missing from the chart (%v) — run "+
					"'cp config/crd/bases/*.yaml charts/dorgu-operator/crds/'", bundled, err)
			}

			if !bytes.Equal(want, got) {
				t.Errorf("%s is out of date — run "+
					"'make manifests && cp config/crd/bases/*.yaml charts/dorgu-operator/crds/'", bundled)
			}
		})
	}
}
