---
description: Stage and commit changes with conventional commit prefixes (feat/fix/chore/refactor/docs), under 72 chars, no body.
---

# Commit

Stage and commit changes in the current repository.

## Conventions

**Commit message prefix:**
- `feat:` — new feature or capability
- `add:` — adding files, config, or resources (non-feature)
- `fix:` — bug fix
- `chore:` — maintenance, tooling, dependencies, CI
- `refactor:` — code restructure with no behavior change
- `docs:` — documentation-only changes (rarely needed; prefer keeping docs out of commits unless they are the primary deliverable)

**Message format:**
```
<prefix>: <short imperative description>
```
- One line only; no body
- No author info, no `Co-Authored-By`, no file lists
- Describe the **task or capability**, not the files changed
- Keep it under 72 characters

**Examples:**
```
feat: add cluster setup wizard
fix: handle missing ClusterPersona on setup
chore: update go dependencies
refactor: extract stack installer into setup package
```

**Branch naming:**
- `feat-<slug>` — new feature
- `fix-<slug>` — bug fix
- `refactor-<slug>` — refactor
- `bug-fix-<slug>` — alternative for bugs
- Slug is lowercase with hyphens, e.g. `feat-cluster-setup`

## What NOT to commit

- `docs-internal/` files — internal documentation, not application code
- `.env`, secrets, credentials
- Generated files that are in `.gitignore`
- Binary artifacts

## Steps

### 1. Check status

```bash
git status
git diff --stat
```

### 2. Stage files

Stage only application code files. Do not stage:
- `docs-internal/`
- `.env` files
- Files listed in `.gitignore`

```bash
git add <specific files or directories>
# Never use: git add -A or git add . without reviewing what gets staged
```

### 3. Confirm staged changes

```bash
git diff --cached --stat
```

### 4. Commit

```bash
git commit -m "feat: short description of the task"
```

No `--no-verify`. If hooks fail, fix the issue and retry.

### 5. Verify

```bash
git log --oneline -3
git status  # should be clean
```
