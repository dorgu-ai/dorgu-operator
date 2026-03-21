---
description: Update CHANGELOG.md with commits since the last release tag using Keep a Changelog format.
---

# Changelog Update

Update `CHANGELOG.md` with commits since the last release tag. The argument to this command is an optional target version (e.g. `/changelog v0.2.2`). If not provided, update the `[Unreleased]` section.

## Step 1: Find the last release tag and collect commits

```bash
# Find the most recent tag
git describe --tags --abbrev=0

# List all commits since that tag (one line per commit)
git log <last-tag>..HEAD --oneline --no-merges

# For more context on each commit
git log <last-tag>..HEAD --no-merges --format="%h %s (%an)"
```

## Step 2: Categorize commits

Map commit prefixes to changelog sections following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and [Conventional Commits](https://www.conventionalcommits.org/):

| Commit prefix | Changelog section |
|---------------|------------------|
| `feat:` | Added |
| `fix:` | Fixed |
| `chore:` | (omit or include under Changed if user-facing) |
| `docs:` | (omit unless it affects public docs) |
| `refactor:` | Changed |
| `perf:` | Changed |
| `test:` | (omit — internal) |
| `build:` / `ci:` | (omit — internal) |
| `security:` | Fixed or Added |

Only include changes that are user-facing or affect external behavior. Skip internal test/CI/build changes.

## Step 3: Update CHANGELOG.md

If updating `[Unreleased]`:

```markdown
## [Unreleased]

### Added
- <description> (commit <hash>)

### Changed
- <description>

### Fixed
- <description>
```

If cutting a release version (argument provided), move `[Unreleased]` content into a new versioned section:

```markdown
## [<VERSION>] - <YYYY-MM-DD>

### Added
- ...

### Fixed
- ...
```

And reset `[Unreleased]` to empty:

```markdown
## [Unreleased]
```

## Step 4: Review and clean up

- Write entries from the user's perspective, not the implementer's. "Fixed setup failing when app name contains underscores" not "Fixed RFC 1123 sanitization in internal_util.go".
- Group related commits into a single entry if they address the same feature or fix.
- Do not include commit hashes in the final entry (they're for cross-referencing during authoring).
- Check that the format matches existing entries in `CHANGELOG.md`.

## Step 5: Commit

```bash
git add CHANGELOG.md
git commit -m "docs: update changelog for <VERSION or upcoming changes>"
```
