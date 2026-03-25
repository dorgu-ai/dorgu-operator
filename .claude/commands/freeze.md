---
description: Scope-lock edits to a specific directory. Prevents accidental changes outside the focus area during debugging or targeted work.
---

# Freeze

Restrict file edits to a specific directory scope. Useful during debugging or focused refactoring to prevent accidentally changing unrelated code.

## Usage

```
/freeze src/pkg/operator    # Only allow edits inside src/pkg/operator/
/freeze cmd/dorgu           # Only allow edits inside cmd/dorgu/
/freeze .                   # Allow edits anywhere (effectively unfreezes)
```

## How It Works

When freeze is active:
- **Edit, Write, MultiEdit** operations are checked against the allowed path
- Files inside the allowed path: proceed normally
- Files outside the allowed path: BLOCKED with explanation

## When to Use

- During `/investigate` — prevent "fixing" unrelated code while debugging
- During focused refactoring — ensure changes stay in scope
- When working on a specific module — avoid scope creep
- When paired with `/careful` via `/guard` — maximum safety

## What's Always Allowed (Even When Frozen)

- **Test files** — `*_test.go`, `*.test.ts`, `*.test.tsx`, `test_*.py`, `*_test.py`
- **Read operations** — reading files is never restricted
- **Git operations** — commits, pushes, etc. are not affected

## Rules

- Always show the current freeze scope when active
- When a blocked edit is attempted, explain: "Edit blocked: [file] is outside freeze scope [scope]. Use `/freeze .` to unfreeze or `/freeze [new-path]` to change scope."
- Freeze persists for the session — remind user it's active if they seem confused
- Test files are always editable because debugging often requires writing regression tests
