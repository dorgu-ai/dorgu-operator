# Changelog

All notable changes to the Dorgu Operator are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.2] - 2026-07-23

### Fixed

- **Remediation dedup per persona + target — one OOM → one remediation** — a single OOM spawns two incidents (`…-oomkilled` and `…-crashloopbackoff`), and the WS8 dedup keyed only on the incident, so each incident still produced its own AI `RemediationAction` (two RAs for one root cause). The proposer now also stands down when an active (`Pending`/`Approved`/`Applying`/`Verifying`) `RemediationAction` for the **same persona already remediates the same target** — keyed on the persona-spec patch path (e.g. `spec.resources.limits.memory`), read from both the rule-based `Action.Patch` and every AI `Steps[].Patch`. So the trailing CrashLoopBackOff incident finds the OOM incident's memory fix and skips. Terminal-phase actions (`Completed`/`Rejected`/`RolledBack`/`Failed`/`Expired`) never block a fresh recurrence; a different persona or a different target (e.g. CPU vs memory) still proposes.
- **RBAC gap — `replicasets.apps is forbidden`** — added the missing ClusterRole rule so the manager can read ReplicaSets (`apps/replicasets`, `get`/`list`/`watch`), used by the event correlator and the pod→ReplicaSet→Deployment ownership walk. Regenerated `config/rbac/role.yaml` and synced the bundled chart RBAC, quieting the recurring `cannot list resource "replicasets" in API group "apps"` error.

## [0.7.1] - 2026-07-22

### Fixed

- **Remediation multiplicity — one remediation per incident** — the proposer now dedups: it skips proposing when an active (`Pending`/`Approved`/`Applying`/`Verifying`) `RemediationAction` already targets the incident, so the 60s health-check loop no longer creates a fresh `RemediationAction` every cycle. Additionally, the legacy rule-based OOM path in the ApplicationPersona reconciler stands down when the health-check reconciler is active (which owns unified detection→diagnosis→remediation), so a single OOM no longer yields both a rule-based and an AI action. One incident → at most one remediation.
- **RBAC gaps broke saturation/metrics/event detection** — added the missing ClusterRole rules so these signals work on managed clusters (e.g. EKS): core `events` (`get`/`list`/`watch`, for the event watcher) and `pods` in the `metrics.k8s.io` API group (`get`/`list`, for the metrics-usage checker). Regenerated `config/rbac/role.yaml` and the bundled chart RBAC.
- **Missing `spec.nodeName` pod field index** — the manager now registers the `spec.nodeName` field index in its cache at startup, so the resource-saturation checker can list pods-by-node instead of failing with `Index with name field:spec.nodeName does not exist`.
- **Status-update conflict noise** — wrapped the remaining IncidentMemory/ApplicationPersona status writes (incident conditions, persona-status sync, incident auto-resolution) in `retry.RetryOnConflict` with a re-fetch, quieting the frequent `object has been modified` log noise from racing reconcilers.

## [0.7.0] - 2026-07-09

### Added

- **AI-generated ordered remediation plans (Anthropic BYOK)** — new `internal/remediation/planner` produces ordered, multi-step remediation plans from diagnosed incidents using Claude. The AI proposer gathers cluster context, prompts the model, and emits a validated ordered plan; falls back to the deterministic proposer when no key is configured. Bring-your-own-key, gated by the LLM provider/API-key configuration.
- **`RemediationAction.Steps[]` — ordered remediation plans** — the `RemediationAction` CRD now carries an ordered `steps` array, letting a single remediation express a sequenced plan (each step with its own action, target, and parameters) instead of a single flat proposal. Schema regenerated into the `remediationactions` CRD.
- **Secure Helm AI-key injection + `values-local` workflow** — the Helm chart now injects the Anthropic API key via a managed `llm-secret` referenced through `secretKeyRef` (never rendered into the Deployment spec), plus a `values-local.example.yaml` workflow for supplying the key locally without committing it.

### Fixed

- **Reliable ClusterPersona auto-create** — the startup bootstrap that auto-creates the default `dorgu-cluster` ClusterPersona is now reliable, correctly gating on existing personas and applying the bootstrap/cluster-uid annotations under race conditions.

## [0.6.1] - 2026-04-17

### Added

- Operator now auto-creates a default ClusterPersona named `dorgu-cluster` on startup if none exists. Gated behind `--auto-create-cluster-persona` (default `true`); disable with `--set operator.autoCreateClusterPersona=false` for GitOps-managed clusters. The persona carries `dorgu.io/bootstrap: "true"` and `dorgu.io/cluster-uid` annotations for multi-cluster traceability.

### Fixed

- Remediation skip reasons are now logged at INFO level (previously logged at verbose level, invisible at default operator log level).
- AI diagnosis can no longer suppress `resource-adjustment` proposals by returning a non-proposable action (e.g., `investigate`). The guard allows the AI to freely change non-proposable base actions, but blocks downgrades from proposable to non-proposable.

## [0.6.0] - 2026-04-13

### Added

- **Helm chart: Phase 2a/2b values** — `healthCheck.enabled` and `healthCheck.interval` now expose the `--enable-health-check` and `--health-check-interval` operator flags via Helm. Previously, detection, diagnosis, and remediation were inaccessible through standard `helm install`.
- **Helm chart: metrics-server toggle** — `metricsServer.enabled` (default true) controls `--enable-metrics-server` flag.
- **Helm chart: LLM / AI diagnosis values** — `llm.provider`, `llm.apiKey`, and `llm.model` expose BYOK AI-enhanced diagnosis (Claude or Gemini) without requiring raw flag overrides.
- Helm chart version bumped to `0.6.0` to align with operator release.

## [0.5.3] - 2026-04-09

### Fixed

- Fix Helm chart ClusterRole missing RBAC rules for `incidentmemories`, `remediationactions`, `dorguevents`, and their `/status` subresources. Operators deployed via Helm were silently unable to create or update incident and remediation CRDs.
- Fix "object has been modified" status update conflicts in `HealthCheckReconciler` and `RemediationController`. Status updates are now retried with `retry.RetryOnConflict` and a re-fetch before each attempt, preventing concurrent reconciler races from failing incident updates silently.
- Fix `RemediationAction` lifecycle events not broadcasting over WebSocket. The `RemediationController` now calls `BroadcastRemediation` at each phase transition (created, approved, completed, rolledback, rejected, failed).
- Fix WebSocket `request` handler returning `unknown_topic` error for `incidents` and `remediations` topics. Clients can now request an initial snapshot of active incidents and pending remediations on connect.

## [0.5.2] - 2026-04-07

### Fixed

- Fix ApplicationPersona reconciler only matching deployments with `app.kubernetes.io/name` label. Now falls back to common `app` label, matching the pattern already used by the events correlator.

## [0.5.1] - 2026-04-07

### Fixed

- Fix operator crash (`panic: close of closed channel`) when starting with `websocket.enabled=true`. Signal handler is now called once and shared between the WebSocket server and controller manager.
- Fix OOM workloads not producing IncidentMemory or RemediationAction CRDs. Added persona correlator to the detection engine that matches pod signals to ApplicationPersonas by namespace and name, enabling the full detect-diagnose-incident pipeline.
- Fix addon version reporting showing "latest" for OpenObserve. Added `helm.sh/chart` label parsing as fallback when `app.kubernetes.io/version` is missing. Image tags of "latest" are now reported as "unknown".
- Fix ClusterPersona reconciler not applying `selfHealing` defaults. Missing `mode` and `trustLevel` fields are now filled in during reconciliation (mode "observe", trustLevel 2).

## [0.5.0] - 2026-04-05

### Added

- **Remediation engine** — generates RemediationAction CRDs from diagnosed incidents with resource adjustment proposals (memory/CPU increases for OOM and saturation). Integrated into the health check reconciler's detect→diagnose→propose loop.
- **Safety guardrails** — rate limiting (5 per persona per hour, 1 concurrent), blast radius caps (max 2x resource increase), dry-run default (all proposals require approval), and namespace deny list (kube-system excluded).
- **Remediation controller** — watches RemediationAction CRDs through the full lifecycle: Pending → Approved → Applying → Verifying → Completed (or RolledBack/Failed). Applies JSON merge patches to ApplicationPersona CRDs. Auto-rollback on degradation using pre-patch state snapshots.
- **Post-apply verification** — re-runs detection engine after configurable wait period (default 10m) to confirm remediation improved health. Updates IncidentMemory with resolution details (action, outcome, duration).
- **AI-enhanced diagnosis (BYOK)** — `AIProvider` wraps rule-based diagnosis with LLM-generated explanations. Supports Anthropic Claude and Google Gemini via `--llm-provider` flag and `ANTHROPIC_API_KEY`/`GEMINI_API_KEY` env vars. Graceful degradation when no key configured.
- **WebSocket broadcast** — broadcasts incident, remediation, and health update events to connected CLI clients for real-time streaming (`dorgu health --watch`).
- **CloudNativePG addon discovery** — CNPG now appears in ClusterPersona addon list after blessed stack installation.
- `--llm-provider` flag for AI diagnosis provider selection (claude, gemini).
- `--llm-api-key` flag for API key override (prefers env vars).
- `--llm-model` flag for model override.

### Fixed

- Apply gofmt formatting to remediation controller files.

## [0.4.1] - 2026-03-31

### Fixed

- Add GitHub Release creation to release workflow — new releases now appear on the GitHub Releases page with release notes and Helm chart attached.

## [0.4.0] - 2026-03-29

### Added

- **IncidentMemory CRD** — namespaced CRD for tracking cluster incidents with detection signals, root cause analysis, confidence scoring, and resolution tracking. Supports cross-namespace relationship tracking via `relatedResources` field.
- **RemediationAction CRD** — namespaced CRD for remediation proposals with YAML diff, approval workflow, rollback spec, and progressive trust levels. Type definitions ready for Phase 2b execution.
- **DorguEvent CRD** — lightweight event persistence with TTL-based cleanup. Hybrid architecture: CRD-backed storage with in-memory LRU cache for fast reads.
- **Detection engine** with pluggable signal collectors: node health (Ready, MemoryPressure, DiskPressure, PIDPressure, NetworkUnavailable), pod failures (OOMKilled, CrashLoopBackOff, ImagePullBackOff, Evicted, long-Pending, high restarts), resource saturation (CPU/memory request vs allocatable with configurable thresholds), and control plane health (healthz/readyz endpoints, ComponentStatus, Lease freshness).
- **Optional metrics-server integration** — detects actual CPU/memory usage when metrics-server is available; graceful degradation without it.
- **Diagnosis engine** with deterministic rule-based provider covering 8 failure patterns: OOM root cause, CrashLoop correlation, node pressure, node down, resource saturation, control plane issues, image pull failures, and long-pending pods. Confidence scoring with signal clarity and time proximity factors.
- **Event processing pipeline** — K8s Event watcher via informers, event classifier (severity/category mapping), persona correlator (pod→deployment→ApplicationPersona matching), and K8s Event emitter for `kubectl describe` visibility.
- **Health check reconciler** — timer-based reconciler (configurable interval, default 60s) that runs the detect→diagnose→incident loop. Creates/updates IncidentMemory CRDs with deduplication via label-based matching. Auto-resolves incidents when triggering signals clear.
- **Incident controller** — watches IncidentMemory CRDs for lifecycle management, label maintenance, condition updates, and ApplicationPersona status synchronization (`status.activeIncidents`, `status.lastIncidentTime`).
- **SelfHealing policy** fields on ClusterPersona spec: `mode` (observe/propose/auto-approve), `trustLevel` (L0-L5, default L2), `maxRemediationsPerHour`, `excludeNamespaces`, and rollback configuration.
- `--enable-health-check` flag to opt into the health check reconciler and event pipeline.
- `--health-check-interval` flag for configurable reconciliation interval.
- `--enable-metrics-server` flag for metrics-server integration.

### Fixed

- Correct Confidence printcolumn type from number to string in CRD manifests.
- Resolve TOCTOU race in event dedup and startupTime data race in event watcher.
- Prefer pod version label over image digest for addon discovery.

## [0.3.0] - 2026-03-23

### Added

- Claude Code project configuration files for better project management.

### Fixed

- Handle JSON unmarshal errors and use server context in WebSocket handlers.

### Changed

- Extracted flag parsing from `cmd/main.go` into `cmd/config.go` with `operatorConfig` struct, removing `nolint:gocyclo` suppression.
- Refactored webhook validators to return slices instead of mutating pointer arguments.
- Extracted controller helpers: `setCondition`, validation, and status helpers into dedicated files.
- Extracted ClusterPersona discovery and addon helpers into dedicated files.
- Extracted WebSocket message handlers into `handlers.go` and replaced magic numbers with named constants.

## [0.2.5] - 2026-03-11

### Added

- OpenObserve addon discovery in ClusterPersona controller.
- Go reviewer command and agent.

### Fixed

- Correct NODES printer column and prevent phase regression.
- Naming changes for consistency.
- Resolved lint issues (ginkgo-linter, goconst, staticcheck).

## [0.2.x]

### Added

- ApplicationPersona and ClusterPersona CRD controllers with validation and lifecycle management.
- WebSocket server for real-time CLI communication.
- Prometheus metrics endpoint with custom persona metrics.
- Helm chart for operator deployment.
