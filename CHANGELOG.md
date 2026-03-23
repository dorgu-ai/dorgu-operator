# Changelog

All notable changes to the Dorgu Operator are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
