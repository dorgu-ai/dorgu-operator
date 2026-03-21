---
description: Create a pull request from the current branch with summary, key changes, commits, and test plan.
---

# PR

Create a pull request from the current branch into the target branch (default: `master`).
The argument to this command is the target branch, e.g. `/pr feat-cluster-setup` to merge into `feat-cluster-setup`, or `/pr` to merge into `master`.

This PR is always for **human review before merging**. Never merge without user approval.

## Steps

### 1. Confirm branch and target

```bash
git branch --show-current
# Verify you are on the correct feature branch, not master
```

### 2. Check for unpushed commits

```bash
git status
git log --oneline origin/<current-branch>..HEAD 2>/dev/null || git log --oneline -10
```

Push if needed:
```bash
git push -u origin <current-branch>
```

### 3. Gather commit history since diverging from target

```bash
git log --oneline <target-branch>..HEAD
git diff --stat <target-branch>...HEAD
```

### 4. Create the PR

Use `gh pr create` with a detailed body. The body must include:

- **Summary**: 3-6 bullet points describing the major changes and why they were made
- **Changed files**: key files added or modified (not a raw git diff, just the meaningful ones)
- **Commits**: full list of commit messages included in the PR
- **Test plan**: how to verify the changes work (commands to run, things to check)

```bash
gh pr create \
  --base <target-branch> \
  --title "<prefix>: <short description>" \
  --body "$(cat <<'EOF'
## Summary

- ...
- ...

## Key Changes

- `path/to/file` — what it does
- `path/to/other` — what it does

## Commits

- `feat: ...`
- `fix: ...`

## Test Plan

- [ ] Tests pass
- [ ] Manual verification steps
- [ ] ...
EOF
)"
```

### 5. Share the PR URL

Output the PR URL so the user can review it before merging.

## Conventions

- PR title follows the same prefix format as commits: `feat:`, `fix:`, `chore:`, `refactor:`
- Target branch defaults to `master` unless specified
- Always include a test plan — QA steps the user can run to validate the PR
- Do NOT merge the PR; leave that to the user
