#!/usr/bin/env bash
# Renders the chart's admission policies for the kustomize install and the envtest suites.
#
# The policies are written once, in charts/dorgu-operator/templates/admission-policies.yaml.
# config/admission/policies.yaml is that template rendered with the operator identity the
# kustomize install gives it: namespace dorgu-operator-system (config/default) and
# ServiceAccount dorgu-operator-controller-manager (the namePrefix on controller-manager).
# A test in charts/dorgu-operator fails when the committed file and a fresh render differ.
#
# Usage: hack/render-admission-policies.sh [output-file]
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-${root}/config/admission/policies.yaml}"

helm template dorgu "${root}/charts/dorgu-operator" \
  --namespace dorgu-operator-system \
  --set serviceAccount.name=dorgu-operator-controller-manager \
  --show-only templates/admission-policies.yaml >"${out}"
