---
description: Unified ship workflow — test, review, and create PR in one flow. Orchestrates verification, code review, and PR creation.
---

# Ship

Automated ship pipeline. Runs tests, review, and creates PR — stopping only for failures or decisions that need human input.

## Pipeline

### Step 1: Pre-flight Check
- Verify on a feature branch (not main/master)
- Check for uncommitted changes — commit or stash first
- Fetch latest base branch

```bash
git status
git fetch origin main
```

### Step 2: Merge Base Branch
- Merge origin/main (or base branch) into current branch
- If conflicts: STOP and present conflicts for resolution
- If clean: continue

### Step 3: Run Tests
Run all applicable test suites:
- **Go:** `go test ./...`
- **Python:** `pytest`
- **TypeScript:** `npm test` or `npx vitest run`
- **Lint:** run language-specific linters (`golangci-lint`, `ruff`, `eslint`)

If tests fail: STOP and present failures. Do NOT proceed with failing tests.

### Step 4: Code Review
Run `/code-review` on the diff against base branch:
- Critical issues: STOP and fix before proceeding
- High issues: present and ask whether to fix or proceed
- Medium/Low: note in PR description

### Step 5: Test Coverage Check
- Check if new code has tests
- Flag any new functions/methods without corresponding tests
- If coverage gaps found: write the missing tests

### Step 6: Create Commits
- Ensure commits are logical and bisectable
- Each commit should represent one coherent change
- Use conventional commit format (feat/fix/chore/refactor/docs)

### Step 7: Push & Create PR
- Push branch to remote
- Create PR using `gh pr create` with:
  - Clear title (conventional commit style)
  - Summary of changes
  - Test plan
  - Link to any related issues

```bash
git push -u origin $(git branch --show-current)
gh pr create --title "..." --body "..."
```

## Stop Conditions

The pipeline halts for:
1. **Merge conflicts** — user must resolve
2. **Test failures** — must be fixed
3. **Critical review findings** — must be addressed
4. **Missing tests for new code** — must be written
5. **Uncommitted changes** — must be committed or stashed

## Skip Options

User can invoke with modifiers:
- `/ship --no-review` — skip code review step (for trivial changes)
- `/ship --no-test` — skip tests (DANGEROUS, requires confirmation)

## Output

On success:
```
## Ship Summary

**Branch:** feature/xyz
**Base:** main
**Tests:** ✓ All passing (X tests)
**Review:** ✓ No critical issues
**Coverage:** ✓ New code covered
**PR:** https://github.com/org/repo/pull/123

Ready for review.
```

## Rules

- Never skip tests without explicit user confirmation
- Never force-push during ship
- Always create PR against the correct base branch
- If the pipeline fails, present a clear summary of what went wrong and what to do next
