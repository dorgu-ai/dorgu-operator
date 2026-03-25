---
name: investigation
description: Root-cause debugging methodology. Activate when debugging issues, analyzing failures, or performing incident investigation. Supports /investigate command.
---

# Investigation Skill

Systematic methodology for finding root causes of bugs, failures, and unexpected behavior.

## When to Activate

- User reports a bug or unexpected behavior
- Tests are failing for unclear reasons
- Production incident investigation
- Performance degradation diagnosis
- During `/investigate` command

## Core Principle

**Never fix symptoms. Always find root causes.**

A symptom fix will break again. A root cause fix prevents the entire class of bugs.

## Investigation Framework

### Step 1: Stabilize (Don't Make It Worse)

Before investigating:
- Don't change code randomly hoping to fix it
- Don't restart services without capturing the current state
- Capture: logs, error messages, stack traces, recent changes
- Consider using `/freeze` to prevent accidental edits

### Step 2: Reproduce

A bug you can't reproduce is a bug you can't fix.

- **Exact steps** to trigger the issue
- **Minimal reproduction** — strip away everything that isn't necessary
- **Consistent?** — does it happen every time or intermittently?
- **Environment-specific?** — does it happen locally, in CI, in prod?

If intermittent, look for:
- Race conditions (timing-dependent)
- Resource exhaustion (memory, file descriptors, connections)
- External dependencies (network, third-party APIs)
- Data-dependent paths (specific inputs trigger it)

### Step 3: Timeline

Build a timeline of what changed:

```bash
# Recent commits
git log --oneline -20

# What changed in the affected files
git log --oneline -10 -- path/to/affected/file.go

# Diff against last known good state
git diff <last-good-commit> -- path/to/affected/
```

### Step 4: Hypothesis Formation

Form exactly 3 hypotheses, ranked by likelihood:

| # | Hypothesis | Evidence For | Evidence Against | Test |
|---|-----------|-------------|-----------------|------|
| 1 | Most likely | ... | ... | How to verify |
| 2 | Possible | ... | ... | How to verify |
| 3 | Less likely | ... | ... | How to verify |

### Step 5: Hypothesis Testing

For each hypothesis (most likely first):
1. Design a test that definitively confirms or denies
2. Run the test
3. Record the result
4. If confirmed → proceed to fix
5. If denied → cross off and move to next

### Step 6: Root Cause Fix

Once confirmed:
1. Fix the root cause (not the symptom)
2. Search for same pattern in codebase — fix all instances (boil the lake)
3. Write a regression test that fails without the fix
4. Run full test suite to verify no collateral damage

## Common Root Cause Patterns

### Go-Specific
- **Goroutine leak** — goroutine started but never exits (missing context cancellation)
- **Race condition** — run with `-race` flag: `go test -race ./...`
- **Nil pointer** — follow the nil from origin to crash point
- **Error swallowing** — `_ = err` hiding real failures
- **Slice aliasing** — multiple slices sharing underlying array
- **Interface nil** — interface containing typed nil (not equal to nil)
- **Closed channel** — sending on closed channel panics

### React/TypeScript-Specific
- **Stale closure** — useEffect/useCallback capturing old state
- **Missing dependency** — useEffect dependency array incomplete
- **Render loop** — setState in useEffect without proper guards
- **Key prop** — missing or wrong key causing DOM reuse bugs

### General
- **Config drift** — dev/staging/prod configs diverged
- **Dependency update** — transitive dependency changed behavior
- **Cache poisoning** — stale cached value served
- **Timezone** — server vs client vs database timezone mismatch
- **Encoding** — UTF-8 vs ASCII vs locale-specific encoding

## Investigation Tools

```bash
# Go: race detector
go test -race ./...

# Go: deadlock detector
go test -timeout 30s ./...

# Go: profiling
go test -cpuprofile cpu.prof -memprofile mem.prof ./...
go tool pprof cpu.prof

# Git: find which commit introduced the bug
git bisect start
git bisect bad HEAD
git bisect good <last-known-good>
# Then test each commit git bisect suggests

# General: search for error patterns
git log --all -p -S "the error string"
```

## Anti-Patterns

- **Shotgun debugging** — making random changes and re-running
- **Blame debugging** — "it worked before X's commit" without understanding why
- **Stack Overflow debugging** — copying solutions without understanding the root cause
- **Logging everything** — adding 50 log lines instead of reading the code
- **Restart and pray** — restarting the service hoping it fixes itself
