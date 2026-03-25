---
description: Cut a new release with auto-detected version, changelog update, tagging, and GoReleaser verification.
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
# Must be on main branch and clean
git branch --show-current    # must be "main" or "master"
git status                   # must be clean (no uncommitted changes)

# Full CI check
make check

# Verify binary builds
make build
```

If `make check` fails, stop and fix the issues. Do not tag a failing build.

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

## Step 5: Commit the changelog

```bash
git add CHANGELOG.md
git commit -m "chore: release <VERSION>"
```

## Step 6: Tag the release

```bash
git tag -a <VERSION> -m "Release <VERSION>"
```

Version must follow semver (`vMAJOR.MINOR.PATCH`). Pre-releases use `-rc.N` suffix (e.g. `v0.3.0-rc.1`).

## Step 7: Verify the build with GoReleaser (dry run)

```bash
goreleaser release --snapshot --clean
```

Check that `./dist/` contains binaries for expected platforms (linux/darwin amd64/arm64, windows amd64).

## Step 8: Push tag to trigger release workflow

**Ask the user to confirm before pushing:**

> Ready to push tag `<VERSION>` to origin. This will trigger the release CI workflow.
> Push now?

```bash
git push origin main
git push origin <VERSION>
```

The release GitHub Actions workflow triggers on tag push and runs GoReleaser to publish binaries and create the GitHub Release.

## Step 9: Verify the release

After CI completes:
1. Check GitHub Releases page for `<VERSION>` with attached binaries and checksums
2. Test install: `go install <module-path>@<VERSION>`
3. Verify version output matches the tag

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
