# Go Review

Review changed Go files for quality, security, concurrency, and adherence to Dorgu project patterns.

Runs static analysis (`go vet` + `staticcheck`), reads changed files, and produces a tiered report (CRITICAL/HIGH/MEDIUM) with a verdict.

## What gets reviewed

- **Error handling** — wrapping, sentinel errors, `errSilent` pattern
- **Concurrency** — goroutine leaks, channel safety, mutex usage
- **Security** — operator invariant (never writes workloads), secrets, injection, input validation
- **K8s patterns** — Executor interface, namespace handling, resource cleanup
- **Idiomatic Go** — naming, interfaces, test patterns

## Steps

### 1. Identify changed Go files

```bash
# Changed files vs HEAD
git diff --name-only HEAD -- '*.go'

# Fallback: staged files
git diff --cached --name-only -- '*.go'

# Fallback: vs main branch
git diff --name-only main...HEAD -- '*.go'
```

If no Go files changed, stop: "No Go files changed."

### 2. Run static analysis

```bash
go vet ./...
which staticcheck >/dev/null 2>&1 && staticcheck ./...
```

### 3. Review

Read each changed file in full (not just the diff). Apply the review checklist from the `go-reviewer` agent.

### 4. Report

Output a structured report:
- Static analysis results
- Issues grouped by severity (CRITICAL → HIGH → MEDIUM)
- Security check summary
- Verdict: **APPROVE** / **REQUEST CHANGES** / **NEEDS DISCUSSION**

### 5. Verdict rules

- **APPROVE** — No CRITICAL or HIGH issues
- **REQUEST CHANGES** — Any CRITICAL or HIGH issues; list what must be fixed
- **NEEDS DISCUSSION** — Architectural concerns needing team input
