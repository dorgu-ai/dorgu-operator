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
	"os"
	"strings"
)

// OperatorNamespaceEnv is the env var the chart and kustomize manifests set
// from the downward API (metadata.namespace).
const OperatorNamespaceEnv = "POD_NAMESPACE"

// serviceAccountNamespaceFile is where Kubernetes mounts the pod's namespace.
// A variable so tests can point it elsewhere.
var serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// OperatorNamespace returns the namespace Dorgu itself runs in, so the
// deny-list protects it wherever it was installed. It reads POD_NAMESPACE,
// then the mounted service account namespace, and returns "" when neither is
// available (running outside a cluster).
func OperatorNamespace() string {
	if ns := strings.TrimSpace(os.Getenv(OperatorNamespaceEnv)); ns != "" {
		return ns
	}
	data, err := os.ReadFile(serviceAccountNamespaceFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
