---
description: Comprehensive Go code review for idiomatic patterns, concurrency safety, error handling, security, and project conventions.
---

# Go Code Review

Comprehensive Go-specific code review combining static analysis, security scanning, concurrency review, and idiomatic Go checks.

## What This Command Does

1. **Identify Go Changes**: Find modified `.go` files via `git diff`
2. **Run Static Analysis**: Execute `go vet`, `staticcheck`, and `golangci-lint`
3. **Security Scan**: Check for SQL injection, command injection, race conditions
4. **Concurrency Review**: Analyze goroutine safety, channel usage, mutex patterns
5. **Idiomatic Go Check**: Verify code follows Go conventions and best practices
6. **Project Patterns**: Verify adherence to project-specific patterns (error handling, interfaces, K8s patterns)
7. **Generate Report**: Categorize issues by severity with a verdict

## When to Use

Use `/go-review` when:
- After writing or modifying Go code
- Before committing Go changes
- Reviewing pull requests with Go code
- Onboarding to a new Go codebase
- Learning idiomatic Go patterns

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
golangci-lint run 2>/dev/null

# Race detection
go build -race ./...

# Security vulnerabilities
govulncheck ./... 2>/dev/null
```

### 3. Review each changed file

Read each changed file in full (not just the diff). Check for all issues in the categories below.

### 4. Output report

Produce a structured report with:
- Static analysis results
- Issues grouped by severity (CRITICAL, HIGH, MEDIUM)
- Security check summary
- Verdict

### 5. Verdict rules

- **APPROVE** — No CRITICAL or HIGH issues
- **REQUEST CHANGES** — Any CRITICAL or HIGH issues; list what must be fixed
- **NEEDS DISCUSSION** — Architectural concerns needing team input

## Review Categories

### CRITICAL (Must Fix)
- SQL/Command injection vulnerabilities
- Race conditions without synchronization
- Goroutine leaks
- Hardcoded credentials
- Unsafe pointer usage
- Ignored errors in critical paths
- Operator invariant violations (e.g. controller writes to workloads it should not)

### HIGH (Should Fix)
- Missing error wrapping with context
- Panic instead of error returns
- Context not propagated
- Unbuffered channels causing deadlocks
- Interface not satisfied errors
- Missing mutex protection
- Sentinel error patterns not followed

### MEDIUM (Consider)
- Non-idiomatic code patterns
- Missing godoc comments on exports
- Inefficient string concatenation
- Slice not preallocated
- Table-driven tests not used
- Naming convention violations

## What Gets Reviewed

- **Error handling** — wrapping with `fmt.Errorf("context: %w", err)`, sentinel errors, consistent patterns
- **Concurrency** — goroutine leaks, channel safety, mutex usage
- **Security** — secrets, injection, input validation, unsafe operations
- **K8s patterns** — Executor interfaces, namespace handling, resource cleanup (if applicable)
- **Idiomatic Go** — naming, interfaces, test patterns, package organization

## Approval Criteria

| Status | Condition |
|--------|-----------|
| APPROVE | No CRITICAL or HIGH issues |
| WARNING | Only MEDIUM issues (merge with caution) |
| BLOCK | CRITICAL or HIGH issues found |

## Integration with Other Commands

- Use `/go-test` first to ensure tests pass
- Use `/go-build` if build errors occur
- Use `/go-review` before committing
- Use `/code-review` for non-Go specific concerns
