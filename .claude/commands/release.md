# Release

Cut a new Dorgu Operator release. Optionally pass a version (e.g. `/release v0.3.0`). If no version is provided, it is auto-detected from commit history.

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

**Ask the user to confirm** the proposed version:

> Proposed version: `v0.3.0` (minor bump — found `feat:` commits since `v0.2.0`)
>
> Commits included:
> - abc1234 feat: add incident memory CRD
> - def5678 fix: reconciler panic on nil status
>
> Proceed with v0.3.0?

If the user provides an explicit version as argument, skip auto-detection and use it directly.

## Step 2: Pre-flight checks

```bash
# Must be on main branch and clean
git branch --show-current
git status

# Run tests and linter
make test
make lint
```

If tests or lint fail, stop and fix. Do not tag a failing build.

## Step 3: Bump Helm chart version

Read and update `charts/dorgu-operator/Chart.yaml`:

```bash
cat charts/dorgu-operator/Chart.yaml
```

Update both fields to the new version (strip the `v` prefix for chart fields):
- `version: <VERSION without v>` (e.g. `0.3.0`)
- `appVersion: "<VERSION without v>"` (e.g. `"0.3.0"`)

Example: For `v0.3.0`, set:
```yaml
version: 0.3.0
appVersion: "0.3.0"
```

## Step 4: Validate CHANGELOG.md

```bash
head -30 CHANGELOG.md
```

If `[Unreleased]` is empty, warn the user to populate it first.

## Step 5: Update CHANGELOG.md

Move items from `[Unreleased]` into a new versioned section:

```markdown
## [<VERSION>] - <YYYY-MM-DD>

### Added
- ...

### Fixed
- ...
```

Follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) conventions. Leave `[Unreleased]` empty.

## Step 6: Commit the release

```bash
git add charts/dorgu-operator/Chart.yaml CHANGELOG.md
git commit -m "chore: release <VERSION>"
```

## Step 7: Tag the release

```bash
git tag -a <VERSION> -m "Release <VERSION>"
```

## Step 8: Build and verify operator image

```bash
# Build the operator image
make docker-build IMG=ghcr.io/dorgu-ai/dorgu-operator:<VERSION without v>

# Verify it built
docker images | grep dorgu-operator
```

Check the image exists with the correct tag.

## Step 9: Push image to GHCR

**Ask the user to confirm before pushing:**

> Ready to push operator image `ghcr.io/dorgu-ai/dorgu-operator:<VERSION>` to GHCR.
> This will make the image publicly available.
> Push now?

```bash
docker push ghcr.io/dorgu-ai/dorgu-operator:<VERSION without v>
```

## Step 10: Push tag to trigger release workflow

**Ask the user to confirm:**

> Ready to push tag `<VERSION>` to origin. This will trigger the release CI workflow
> which publishes the Helm chart to GHCR.
> Push now?

```bash
git push origin main
git push origin <VERSION>
```

The CI workflow triggers on tag push and publishes the Helm chart OCI artifact to `ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator`.

## Step 11: Verify the release

After CI completes:

1. Check GitHub Releases page for `<VERSION>`
2. Verify Helm chart is available:
   ```bash
   helm show chart oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator --version <VERSION without v>
   ```
3. Test installation on a cluster:
   ```bash
   helm install dorgu-operator \
     oci://ghcr.io/dorgu-ai/dorgu-operator-charts/dorgu-operator \
     --version <VERSION without v> \
     --namespace dorgu-system \
     --create-namespace --dry-run
   ```
4. Verify the operator image:
   ```bash
   docker pull ghcr.io/dorgu-ai/dorgu-operator:<VERSION without v>
   ```

## Step 12: Update CLI compatibility (if needed)

If this operator release requires a specific CLI version:
- Update the dorgu CLI's README.md operator version references
- Coordinate with CLI release if needed

## Rollback

If the release is broken:
```bash
# Delete the tag locally and remotely
git tag -d <VERSION>
git push origin :refs/tags/<VERSION>

# Delete the pushed image (requires GHCR admin access)
# Or publish a fix as a new patch release
```

Then fix the issue, revert Chart.yaml version, update CHANGELOG.md, and re-release.

## Conventions

- No `Co-Authored-By` or contributor attribution in the release commit
- Commit message: `chore: release <VERSION>`
- Tag message: `Release <VERSION>`
- Chart version and appVersion always match the tag (without `v` prefix)
- Do not skip hooks (`--no-verify`)
