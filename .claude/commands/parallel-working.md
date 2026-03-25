---
description: Work in isolated git worktrees for parallel agent execution. Each agent gets its own branch and worktree from main/master, commits only scoped files, and raises a PR.
---

# Parallel Working

Set up an isolated git worktree so you can work in parallel with other agents without interference. Each agent operates on its own copy of the repo with its own branch.

## When to use

- Multiple agents refactoring different packages simultaneously
- Any task where you need an isolated working copy
- When your plan assigns you a specific scope of files

## Prerequisites

- You are inside a git repository
- Your plan section specifies: **branch name**, **file scope**, and **target branch**
- No uncommitted changes in the main working tree (check with `git status`)

## Steps

### 1. Detect the default branch

```bash
DEFAULT_BRANCH=$(git symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's@^refs/remotes/origin/@@')
if [ -z "$DEFAULT_BRANCH" ]; then
  DEFAULT_BRANCH=$(git branch -r | grep -E 'origin/(main|master)$' | head -1 | sed 's@.*origin/@@')
fi
echo "Default branch: $DEFAULT_BRANCH"
```

### 2. Ensure latest code

```bash
git fetch origin "$DEFAULT_BRANCH"
```

### 3. Create worktree

```bash
BRANCH_NAME="<your-assigned-branch-name>"
WORKTREE_DIR="../worktree-${BRANCH_NAME}"

git worktree add "$WORKTREE_DIR" "origin/$DEFAULT_BRANCH"
cd "$WORKTREE_DIR"
```

### 4. Create feature branch

```bash
git checkout -b "$BRANCH_NAME"
```

### 5. Do your scoped work

- Only modify files listed in your plan section
- After each logical change, run the project's test command:
  - Go: `make check` or `go test ./...`
  - Node: `npm test`
  - Python: `pytest`
- Commit after each passing change (see step 6)

### 6. Commit

Follow `/commit` conventions:

```bash
git add <only-your-scoped-files>
git commit -m "refactor: <short description of extraction>"
```

- Use the appropriate prefix: `refactor:`, `feat:`, `fix:`, etc.
- One line only, under 72 characters
- **No `Co-Authored-By` lines**
- **No `--no-verify`** — if hooks fail, fix the issue
- Only stage files within your assigned scope

### 7. Push and create PR

```bash
git push -u origin "$BRANCH_NAME"

gh pr create \
  --title "refactor: <short description>" \
  --body "$(cat <<'EOF'
## Summary
- <bullet points describing extractions>

## Scope
- Only modifies files in `<your-package-path>/`
- No public API changes
- All tests passing

## Verification
- `make check` passes
- No changes outside assigned scope
EOF
)" \
  --base "$DEFAULT_BRANCH"
```

- **No co-author lines** in the PR body
- Target the default branch (main/master)

### 8. Cleanup

After the PR is created:

```bash
cd -  # return to original directory
git worktree remove "$WORKTREE_DIR"
```

If the branch was merged and you want to clean up:

```bash
git branch -d "$BRANCH_NAME"
```

## Rules

- **Scope discipline:** Only modify files in your assigned scope. Never touch files outside your plan section.
- **No direct pushes:** Never push to main/master directly. Always use a PR.
- **No co-author:** No `Co-Authored-By` lines in commits or PR descriptions.
- **Test before commit:** Run the project's test suite before every commit. Do not commit failing code.
- **Atomic commits:** One logical extraction or change per commit. Keep commits small and reviewable.
- **No API changes:** Exported function signatures must not change unless your plan explicitly allows it.
- **Fix, don't skip:** If pre-commit hooks or tests fail, fix the issue. Never use `--no-verify`.
- **Clean worktrees:** Always remove your worktree after creating the PR.

## Worktree naming convention

Use the branch name as the worktree directory suffix:

| Branch | Worktree Directory |
|--------|--------------------|
| `refactor/setup-package` | `../worktree-refactor/setup-package` |
| `feat/new-feature` | `../worktree-feat/new-feature` |
| `fix/bug-description` | `../worktree-fix/bug-description` |
