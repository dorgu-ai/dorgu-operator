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
	"os"
	"strings"
	"testing"
)

// F-8: the chart declares the Kubernetes minimum. CRD validation ratcheting, which
// the step command rule relies on to leave stored RemediationActions updatable, is
// on by default (beta) from Kubernetes 1.30 and generally available in 1.33; below
// 1.30 a status update on a stored action with an old-style command is refused.
func TestKubeVersion_IsDeclaredAs130(t *testing.T) {
	raw, err := os.ReadFile("Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `kubeVersion: ">=1.30.0-0"`) {
		t.Error(`Chart.yaml must declare kubeVersion: ">=1.30.0-0" (the -0 admits provider suffixes such as v1.30.4-eks-xyz)`)
	}
}

func TestKubeVersion_RefusesBelow130AndAcceptsAtOrAbove(t *testing.T) {
	refused := []string{"v1.29.9", "v1.28.0"}
	for _, v := range refused {
		out, err := helmTemplateArgs(t, "--kube-version", v)
		if err == nil {
			t.Errorf("helm template accepted Kubernetes %s", v)
		} else if !strings.Contains(out, "kubeVersion") && !strings.Contains(out, "Kubernetes version") {
			t.Errorf("Kubernetes %s: the failure must say it is the Kubernetes version:\n%s", v, out)
		}
	}
	for _, v := range []string{"v1.30.0", "v1.30.4-eks-1234567", "v1.35.0", "v1.31.2+k3s1"} {
		if out, err := helmTemplateArgs(t, "--kube-version", v); err != nil {
			t.Errorf("helm template refused Kubernetes %s: %v\n%s", v, err, out)
		}
	}
}
