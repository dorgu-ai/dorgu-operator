# Changelog

All notable changes to the Dorgu Operator are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
