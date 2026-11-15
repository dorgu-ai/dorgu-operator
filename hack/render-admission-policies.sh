#!/usr/bin/env bash
# Renders the chart's admission policies for the kustomize install, the envtest suites and
# the operator's own content check.
#
# The policies are written once, in charts/dorgu-operator/templates/admission-policies.yaml.
#
# 1. config/admission/policies.yaml is that template rendered with the operator identity the
#    kustomize install gives it: namespace dorgu-operator-system (config/default) and
#    ServiceAccount dorgu-operator-controller-manager (the namePrefix on controller-manager).
# 2. internal/intake/admission_reference.yaml is the same template rendered with a stand-in
#    identity (ReferenceOperatorUsername in internal/intake/policy_content.go). It is embedded
#    in the operator, which swaps in its own username and compares what the cluster holds with
#    it (C2 review F-3), so a policy that was edited, truncated or mis-templated closes the
#    intake instead of passing as "present".
#
# Tests in charts/dorgu-operator fail when either committed file and a fresh render differ in
# content (they compare parsed YAML documents, so the whitespace between documents, which
# Helm releases render differently, does not matter).
#
# The committed files are rendered with the Helm release CI installs (.github/workflows/test.yml),
# so a regeneration never changes them because of the renderer. Another release is refused.
# DORGU_HELM_ANY_VERSION=1 lifts that for the parity tests, which only compare content.
#
# Usage: hack/render-admission-policies.sh [output-file [reference-output-file]]
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Keep in step with the `version:` of azure/setup-helm in .github/workflows/test.yml
# (a test in charts/dorgu-operator fails when they differ).
pinned_helm="v4.1.4"

found_helm="$(helm version --template '{{.Version}}' 2>/dev/null || true)"
if [[ "${found_helm}" != "${pinned_helm}" ]]; then
  if [[ "${DORGU_HELM_ANY_VERSION:-}" == "1" ]]; then
    echo "render-admission-policies: Helm ${found_helm:-unknown} is not the pinned ${pinned_helm}; rendering anyway (DORGU_HELM_ANY_VERSION=1)" >&2
  else
    cat >&2 <<MSG
render-admission-policies: found Helm ${found_helm:-none}, need ${pinned_helm}.
The committed policy files are rendered with the Helm release CI pins (.github/workflows/test.yml),
so that regenerating them never changes them because of the renderer.
Install it: https://github.com/helm/helm/releases/tag/${pinned_helm}
  (or download the archive into ./bin and put that directory first on PATH)
MSG
    exit 1
  fi
fi

out="${1:-${root}/config/admission/policies.yaml}"
reference="${2:-${root}/internal/intake/admission_reference.yaml}"

helm template dorgu "${root}/charts/dorgu-operator" \
  --namespace dorgu-operator-system \
  --set serviceAccount.name=dorgu-operator-controller-manager \
  --show-only templates/admission-policies.yaml >"${out}"

helm template dorgu "${root}/charts/dorgu-operator" \
  --namespace dorgu-reference-ns \
  --set serviceAccount.name=dorgu-reference-sa \
  --show-only templates/admission-policies.yaml >"${reference}"
