# Security Policy

## Supported Versions

We provide security updates for the following versions of the Dorgu Operator:

| Version | Supported          |
| ------- | ------------------ |
| 0.2.x   | :white_check_mark: |
| < 0.2   | :x:                |

During the 0.x release line, we focus on the latest minor version (e.g. 0.2.x). When we release a new major or minor line, we will update this table and may end support for older lines with notice.

## How to Report a Vulnerability

We take security seriously. If you believe you have found a security vulnerability in the Dorgu Operator, please report it privately so we can address it before public disclosure.

**Preferred method:** Open a **private** security advisory on GitHub:

1. Go to [github.com/dorgu-ai/dorgu-operator](https://github.com/dorgu-ai/dorgu-operator).
2. Click **Security** → **Advisories** → **New draft security advisory**.
3. Describe the vulnerability, steps to reproduce, and impact. Do not disclose it in public issues or PRs.

**What to expect:**

- We will acknowledge your report within **5 business days**.
- We will work with you to understand and validate the issue.
- We will not disclose the vulnerability publicly before a fix is available, and we ask that you do the same during the process.
- We will credit you in the advisory (unless you prefer to remain anonymous) when we publish the fix.

## Scope

**In scope:**

- The Dorgu Operator code: controllers (ApplicationPersona, ClusterPersona, ArgoCD watcher), validating webhook, WebSocket server, Prometheus client, CRD handling, and the Helm chart in this repository.
- Security impact of operator behavior (e.g. RBAC, admission decisions, status updates).

**Out of scope:**

- The [Dorgu CLI](https://github.com/dorgu-ai/dorgu); report CLI issues in that repository.
- Kubernetes itself, ArgoCD, Prometheus, or other third-party components.
- General usage questions; use [Discussions](https://github.com/dorgu-ai/dorgu-operator/discussions) or [Issues](https://github.com/dorgu-ai/dorgu-operator/issues) for those.

Thank you for helping keep the Dorgu Operator and its users safe.
