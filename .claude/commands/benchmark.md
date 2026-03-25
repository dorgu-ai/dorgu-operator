---
description: Performance regression detection. Runs Go benchmarks, profiles memory/CPU, and compares before/after. Catches regressions before they ship.
---

# Benchmark

Detect performance regressions by comparing before/after benchmarks.

## Process

### Step 1: Establish Baseline

```bash
# Save current branch
BRANCH=$(git branch --show-current)

# Checkout base branch and run benchmarks
git stash
git checkout main
go test -bench=. -benchmem -count=5 ./... > /tmp/bench-baseline.txt 2>&1
git checkout "$BRANCH"
git stash pop
```

### Step 2: Run Current Benchmarks

```bash
go test -bench=. -benchmem -count=5 ./... > /tmp/bench-current.txt 2>&1
```

### Step 3: Compare Results

```bash
# If benchstat is available
benchstat /tmp/bench-baseline.txt /tmp/bench-current.txt
```

If `benchstat` is not available, parse and compare manually.

### Step 4: Analyze Results

For each benchmark, check:
- **Time regression** — >10% slower = WARNING, >25% = CRITICAL
- **Memory regression** — >20% more allocations = WARNING, >50% = CRITICAL
- **Allocation count** — any increase in allocs/op is notable

### Language-Specific Approaches

**Go (primary):**
```bash
go test -bench=. -benchmem -count=5 -cpuprofile=cpu.prof -memprofile=mem.prof ./...
go tool pprof -top cpu.prof
go tool pprof -top mem.prof
```

**React/TypeScript:**
```bash
# Bundle size analysis
npx vite build 2>&1 | grep -E "dist/|chunk"
# Compare with baseline bundle sizes
```

**Python:**
```bash
python -m pytest --benchmark-only
# Or manual timing
python -m timeit -n 1000 "import module; module.function()"
```

## Output Format

```
## Benchmark Report

**Baseline:** main (commit abc123)
**Current:** feature/xyz (commit def456)

### Results

| Benchmark | Baseline | Current | Delta | Status |
|-----------|----------|---------|-------|--------|
| BenchmarkX | 150ns/op | 160ns/op | +6.7% | OK |
| BenchmarkY | 2.1μs/op | 3.5μs/op | +66% | CRITICAL |

### Memory

| Benchmark | Baseline | Current | Delta | Status |
|-----------|----------|---------|-------|--------|
| BenchmarkX | 2 allocs/op | 2 allocs/op | 0% | OK |
| BenchmarkY | 5 allocs/op | 12 allocs/op | +140% | CRITICAL |

### Verdict: PASS / REGRESSIONS FOUND

**Regressions:**
- BenchmarkY: 66% slower, 140% more allocations
  - Likely cause: [analysis of what changed in the code path]
  - Suggestion: [how to fix]
```

## Rules

- Always use `-count=5` or more for statistical significance
- Compare against main branch, not arbitrary commits
- Flag regressions but don't block on micro-optimizations (<5% delta)
- If no benchmarks exist, suggest which functions need them based on the diff
- For Go: focus on hot paths, handlers, and data processing functions
- For React: focus on bundle size and lighthouse scores when applicable
