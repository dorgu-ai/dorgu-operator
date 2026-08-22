---
description: Cut a new operator release with auto-detected version, changelog update, chart bump, tagging, and image/chart verification.
---

# Release

Cut a new release. Optionally pass a version (e.g. `/release v0.3.0`). If no version is provided, it is auto-detected from commit history.

## Step 1: Auto-detect version (if not provided)

```bash
# Get latest tag
git describe --tags --abbrev=0

# List commits since last tag
git log $(git describe --tags --abbrev=0)..HEAD --oneline --no-merges
```

Determine the bump type from commit prefixes:

| Commit prefix | Bump |
|---------------|------|
| `feat:` or `add:` with `!` or `BREAKING` in message | **major** (vX.0.0) |
| `feat:` or `add:` | **minor** (v0.X.0) |
| `fix:`, `chore:`, `refactor:`, anything else | **patch** (v0.0.X) |

Calculate the next version by parsing the latest tag and applying the bump.

**Ask the user to confirm** the proposed version before proceeding:

> Proposed version: `v0.3.0` (minor bump — found `feat:` commits since `v0.2.0`)
>
> Commits included:
> - abc1234 feat: add interactive scaffolding
> - def5678 fix: validate flag input
>
> Proceed with v0.3.0?

If the user provides an explicit version as argument, skip auto-detection and use it directly.

## Step 2: Pre-flight checks

```bash
# Must be on the default branch and clean
git branch --show-current    # must be "master"
git status                   # must be clean (no uncommitted changes)

# Full test suite (fmt, vet, manifests, generate, envtest)
make test

# Verify the manager binary builds
make build
```

If `make test` fails, stop and fix the issues. Do not tag a failing build.

### Lint, with the caps off

`make lint` runs `golangci-lint run`, which truncates at **50 issues per linter**
and **3 per unique message** by default. A release check that stops counting is
how a red tree reads as a short list. Run it uncapped:

```bash
make golangci-lint    # installs the pinned version into ./bin
./bin/golangci-lint run --max-issues-per-linter=0 --max-same-issues=0
```

Must print `0 issues.` and exit 0. Lint is enforced in CI (`.github/workflows/lint.yml`),
but confirm it locally before tagging rather than trusting a green check: a cached
action run can report success without having linted the current tree.

## Step 3: Validate CHANGELOG.md

```bash
# Check that [Unreleased] section has content
head -30 CHANGELOG.md
```

If `[Unreleased]` is empty (no entries), warn the user:
> CHANGELOG.md has no entries under [Unreleased]. Run `/changelog` first to populate it, or add entries manually.

## Step 4: Update CHANGELOG.md

Move items from `[Unreleased]` into a new versioned section:

```markdown
## [<VERSION>] - <YYYY-MM-DD>

### Added
- ...

### Changed
- ...

### Fixed
- ...
```

Follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) conventions:
- **Added** — new features
- **Changed** — changes to existing functionality
- **Fixed** — bug fixes
- **Removed** — removed features

Leave `[Unreleased]` section empty and ready for the next cycle.

## Step 5: Bump the chart version

Release CI restamps `charts/dorgu-operator/Chart.yaml` from the tag when publishing
the OCI chart, but the committed values are what a contributor gets from
`helm install ./charts/dorgu-operator` — and `templates/deployment.yaml` defaults
the image tag to `appVersion`. Leaving them stale means a clone install runs an old
operator image.

```bash
# <VERSION> without the leading "v", e.g. 0.7.3
sed -i '' "s/^version:.*/version: <VERSION_NO_V>/" charts/dorgu-operator/Chart.yaml
sed -i '' "s/^appVersion:.*/appVersion: \"<VERSION_NO_V>\"/" charts/dorgu-operator/Chart.yaml

# Guarded by the chart tests; these must pass before tagging
go test ./charts/dorgu-operator/
```

Those tests assert that `appVersion` does not lag the latest tag, that `version`
and `appVersion` agree, that the bundled `crds/` match `config/crd/bases` byte for
byte, and that `NOTES.txt` reports the state the install actually landed in. The
NOTES tests need **Helm 4** on `PATH`; on Helm 3 they fail with "cluster
unreachable", because only Helm 4 makes `--dry-run=client` genuinely client-only.

## Step 6: Commit the changelog and chart bump

```bash
git add CHANGELOG.md charts/dorgu-operator/Chart.yaml
git commit -m "chore: release <VERSION>"
```

## Step 7: Tag the release

```bash
git tag -a <VERSION> -m "Release <VERSION>"
```

Version must follow semver (`vMAJOR.MINOR.PATCH`). Pre-releases use `-rc.N` suffix (e.g. `v0.3.0-rc.1`).

## Step 8: Verify the artifacts build (dry run)

This repo ships a **container image and an OCI Helm chart**, not Go binaries. There
is no GoReleaser here; `.github/workflows/release.yaml` builds the image with
Docker and packages the chart with Helm. Rehearse both locally:

```bash
# The image the release workflow will build and push
make docker-build IMG=ghcr.io/dorgu-ai/dorgu-operator:<VERSION_NO_V>

# The chart it will package, with CRDs regenerated the way CI does it
make manifests && cp config/crd/bases/*.yaml charts/dorgu-operator/crds/
helm package charts/dorgu-operator -d /tmp/dorgu-release
```

`/tmp/dorgu-release/dorgu-operator-<VERSION_NO_V>.tgz` should appear. If `cp`
changed anything under `crds/`, commit it: the chart test compares those files byte
for byte.

<!-- Package into /tmp, not ./dist. CI does use `-d dist/` on a fresh checkout,
     but `dist/install.yaml` is tracked in this repo, so cleaning up a local
     rehearsal with `rm -rf dist` deletes a committed file. -->

Then read the notes a user will actually get, which is the one artifact no test
sees until it is published:

```bash
helm install verify /tmp/dorgu-release/dorgu-operator-<VERSION_NO_V>.tgz \
  --dry-run=client -n dorgu-system | sed -n '/NOTES:/,$p'
```

## Step 9: Push tag to trigger release workflow

**Ask the user to confirm before pushing:**

> Ready to push tag `<VERSION>` to origin. This will trigger the release CI workflow.
> Push now?

```bash
git push origin master
git push origin <VERSION>
```

The release workflow triggers on the tag push and runs three jobs in order: build
and push the image to GHCR, package and push the chart to
`oci://ghcr.io/dorgu-ai/dorgu-operator-charts`, then create the GitHub Release with
notes extracted from `CHANGELOG.md`.

## Step 10: Verify the release

After CI completes:
1. Check the GitHub Releases page for `<VERSION>` with the chart `.tgz` attached
2. Confirm the image exists: `docker manifest inspect ghcr.io/dorgu-ai/dorgu-operator:<VERSION_NO_V>`
3. Pull the published chart and check what it actually contains:
   ```bash
   helm pull oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator \
     --version <VERSION_NO_V> --untar --untardir /tmp/verify
   grep -E "^(version|appVersion):" /tmp/verify/dorgu-operator/Chart.yaml
   ls /tmp/verify/dorgu-operator/crds/          # must list all five CRDs
   ```
4. Render the notes a user will see on install:
   ```bash
   helm install verify /tmp/verify/dorgu-operator --dry-run=client -n dorgu-system
   ```

## Rollback

If the release is broken:
```bash
# Delete the tag locally and remotely
git tag -d <VERSION>
git push origin :refs/tags/<VERSION>
```

Then fix the issue, update CHANGELOG.md, and re-tag.

## Conventions

- No `Co-Authored-By` or contributor attribution in the release commit
- Commit message: `chore: release <VERSION>`
- Tag message: `Release <VERSION>`
- Do not skip hooks (`--no-verify`)
