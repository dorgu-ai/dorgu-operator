---
description: Maximum safety mode — combines /careful (destructive command warnings) and /freeze (scope-locked edits). Use for production debugging or high-risk work.
---

# Guard

Enable maximum safety mode by combining `/careful` and `/freeze`.

## Usage

```
/guard src/pkg/operator    # Enable careful warnings + freeze edits to src/pkg/operator/
/guard cmd/dorgu           # Enable careful warnings + freeze edits to cmd/dorgu/
```

## What It Does

1. **Careful mode** — warns before destructive commands (rm -rf, DROP TABLE, force-push, kubectl delete)
2. **Freeze mode** — restricts file edits to the specified directory

## When to Use

- Production incident debugging — prevent collateral damage
- High-risk refactoring — stay focused and safe
- Working near sensitive code — payment, auth, data deletion
- Any time the cost of a mistake is high

## Disable

```
/freeze .     # Remove scope restriction (careful mode stays active)
```

## Rules

- When guard is active, remind the user at the start of each major action
- If an edit outside scope is needed, ask the user to explicitly expand the scope
- Guard is the recommended mode for `/investigate` sessions
