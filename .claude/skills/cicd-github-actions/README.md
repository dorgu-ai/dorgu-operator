---
name: cicd-github-actions
description: GitHub Actions CI/CD patterns for Go, React, and Python projects. Covers workflow design, security, caching, matrix builds, and deployment strategies.
---

# CI/CD with GitHub Actions

Patterns for building reliable CI/CD pipelines with GitHub Actions.

## When to Activate

- Setting up or modifying GitHub Actions workflows
- Adding CI/CD to a new project
- Debugging workflow failures
- Optimizing build times
- Reviewing workflow security

## Workflow Structure

### Standard Go Project

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - run: go vet ./...
      - run: go test -race -coverprofile=coverage.out ./...
      - uses: actions/upload-artifact@v4
        with:
          name: coverage
          path: coverage.out

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - uses: golangci/golangci-lint-action@v6
        with:
          version: latest

  build:
    needs: [test, lint]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - run: go build -o bin/ ./...
```

### Standard React/TypeScript Project

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version-file: .nvmrc
          cache: npm
      - run: npm ci
      - run: npm run lint
      - run: npm run type-check
      - run: npm test -- --coverage

  build:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version-file: .nvmrc
          cache: npm
      - run: npm ci
      - run: npm run build
```

## Security Best Practices

### Pin Actions by SHA (Not Tag)
```yaml
# BAD — tags can be moved
- uses: actions/checkout@v4

# GOOD — SHA is immutable
- uses: actions/checkout@b4ffde65f46336ab88eb53be808477a3936bae11 # v4.1.1
```

### Minimal Permissions
```yaml
# Set at workflow level
permissions:
  contents: read

# Elevate only where needed
jobs:
  deploy:
    permissions:
      contents: read
      deployments: write
```

### Protect Against Script Injection
```yaml
# BAD — injectable via PR title
- run: echo "${{ github.event.pull_request.title }}"

# GOOD — use environment variable
- env:
    PR_TITLE: ${{ github.event.pull_request.title }}
  run: echo "$PR_TITLE"
```

### Secret Handling
- Never echo secrets in logs
- Use `GITHUB_TOKEN` over personal access tokens when possible
- Restrict secret access to specific environments
- Use OIDC for cloud provider auth (no long-lived credentials)

## Caching Strategies

### Go Module Cache
```yaml
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
    cache: true  # Caches GOMODCACHE and GOCACHE automatically
```

### Docker Layer Cache
```yaml
- uses: docker/build-push-action@v5
  with:
    cache-from: type=gha
    cache-to: type=gha,mode=max
```

### Custom Cache
```yaml
- uses: actions/cache@v4
  with:
    path: ~/.cache/my-tool
    key: ${{ runner.os }}-my-tool-${{ hashFiles('config.lock') }}
    restore-keys: |
      ${{ runner.os }}-my-tool-
```

## Matrix Builds

```yaml
jobs:
  test:
    strategy:
      matrix:
        go-version: ['1.21', '1.22']
        os: [ubuntu-latest, macos-latest]
      fail-fast: false  # Don't cancel other jobs if one fails
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ matrix.go-version }}
```

## Release Workflow (Go with GoReleaser)

```yaml
name: Release
on:
  push:
    tags: ['v*']

permissions:
  contents: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: goreleaser/goreleaser-action@v5
        with:
          version: latest
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

## Docker Build & Push

```yaml
name: Docker
on:
  push:
    tags: ['v*']

jobs:
  docker:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/metadata-action@v5
        id: meta
        with:
          images: ghcr.io/${{ github.repository }}
          tags: |
            type=semver,pattern={{version}}
            type=semver,pattern={{major}}.{{minor}}
            type=sha
      - uses: docker/build-push-action@v5
        with:
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          cache-from: type=gha
          cache-to: type=gha,mode=max
```

## Debugging Workflows

```bash
# Re-run with debug logging
gh run rerun <run-id> --debug

# View workflow run logs
gh run view <run-id> --log

# List recent workflow runs
gh run list --limit 10

# Watch a running workflow
gh run watch
```

## Anti-Patterns

- **Monolithic workflow** — one huge job instead of parallel jobs
- **No caching** — rebuilding everything from scratch every time
- **Unpinned actions** — using `@main` or `@v4` instead of SHA
- **Over-permissioned** — `permissions: write-all` instead of minimal
- **No fail-fast control** — matrix builds canceling each other
- **Secrets in logs** — echoing environment for debugging
- **No concurrency control** — multiple deploys racing each other

## Concurrency Control

```yaml
# Cancel in-progress runs for same branch
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true

# For deploy: queue instead of cancel
concurrency:
  group: deploy-production
  cancel-in-progress: false
```
