---
description: Safety mode — warns before destructive commands. Intercepts rm -rf, DROP TABLE, force-push, kubectl delete, and similar dangerous operations.
---

# Careful

Enable safety warnings for destructive commands.

## What It Catches

**File System:**
- `rm -rf` (except safe targets: node_modules, .next, dist, __pycache__, .cache, tmp, build)
- `rm -r` on non-obvious directories

**Database:**
- `DROP TABLE`, `DROP DATABASE`
- `TRUNCATE`
- `DELETE FROM` without WHERE clause

**Git:**
- `git push --force` / `git push -f`
- `git reset --hard`
- `git clean -f`
- `git branch -D`
- `git checkout .` / `git restore .`

**Kubernetes:**
- `kubectl delete namespace`
- `kubectl delete -f` on production contexts
- `helm uninstall` on production releases

**Docker:**
- `docker system prune`
- `docker volume rm`

**Process:**
- `kill -9`
- `pkill`

## How It Works

When enabled, the system intercepts Bash commands before execution and checks them against the patterns above.

- **Safe exceptions** are allowed without warning (e.g., `rm -rf node_modules`)
- **Dangerous commands** trigger a warning: "This is a destructive operation: [description]. Proceed? (yes/no)"
- User can override to proceed

## Enable/Disable

- `/careful` — enable safety mode for this session
- Safety mode is enabled by default in profiles that include careful hooks
- Use `/guard` for careful + freeze combined

## Rules

- Never silently block — always explain what was caught and why
- Safe exceptions should cover common development cleanup patterns
- Don't be annoying: clearing build artifacts is normal and shouldn't trigger warnings
- DO warn on anything that could lose work, data, or affect production
