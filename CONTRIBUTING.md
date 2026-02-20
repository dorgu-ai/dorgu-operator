# Contributing to Dorgu Operator

Thank you for considering contributing to the Dorgu Operator. This document covers operator-specific steps; for general practices (code of conduct, issue and PR etiquette, code standards), see the [Dorgu CLI CONTRIBUTING guidelines](https://github.com/dorgu-ai/dorgu/blob/main/CONTRIBUTING.md).

---

## Code of conduct

This project adheres to the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code. Please report unacceptable behavior by opening a [GitHub issue](https://github.com/dorgu-ai/dorgu-operator/issues).

---

## Security

For **security-sensitive issues** (e.g. vulnerabilities), do not open a public issue. See [SECURITY.md](SECURITY.md) for how to report privately.

---

## How to contribute

### General guidelines

The [Dorgu CLI CONTRIBUTING](https://github.com/dorgu-ai/dorgu/blob/main/CONTRIBUTING.md) applies for:

- How to fork, clone, and create branches
- Commit message style and PR process
- Code format, lint, and test expectations

Below are operator-specific steps.

### Operator-specific workflow

1. **Fork and clone** the [dorgu-operator](https://github.com/dorgu-ai/dorgu-operator) repository. Create a branch from `main` (e.g. `fix/controller-name`, `feat/webhook-check`).

2. **Where to change code:**
   - **CRD types:** `api/v1/` (e.g. `applicationpersona_types.go`, `clusterpersona_types.go`)
   - **Controllers:** `internal/controller/` (ApplicationPersona, ClusterPersona, ArgoCD watcher)
   - **Webhook:** `internal/webhook/`
   - **WebSocket server:** `internal/websocket/`
   - **Prometheus client:** `internal/metrics/`
   - **Helm chart:** `charts/dorgu-operator/`

3. **After changing API types:** Run `make manifests generate` so CRDs and generated code (e.g. DeepCopy) stay in sync. The project uses [Kubebuilder](https://book.kubebuilder.io/introduction.html); see the [Kubebuilder book](https://book.kubebuilder.io/) for background.

4. **Before opening a PR:**
   - Run `make test` — all tests must pass.
   - Run `make lint` — fix any reported issues.
   - If you changed types, ensure `make manifests generate` was run and commit any updated generated files.

5. **Pull requests:** Open PRs against `main` of `dorgu-ai/dorgu-operator`. Fill in the PR template (description, related issue, how to verify, checklist). CI must pass.

### Useful commands

- `make help` — list all available targets.
- `make test` — run unit tests.
- `make lint` — run the linter.
- `make manifests` — regenerate CRD and RBAC manifests.
- `make generate` — regenerate DeepCopy and other generated code (run after editing types in `api/`).

---

## Questions

If something is unclear, open a [Discussion](https://github.com/dorgu-ai/dorgu-operator/discussions) or add a comment on the relevant issue or PR.

Thank you for contributing to the Dorgu Operator.
