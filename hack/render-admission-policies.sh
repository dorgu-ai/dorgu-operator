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
# Tests in charts/dorgu-operator fail when either committed file and a fresh render differ.
#
# Usage: hack/render-admission-policies.sh [output-file [reference-output-file]]
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
