---
description: Sprint retrospective from git history. Analyzes commits, work patterns, test ratios, and shipping velocity. Produces actionable insights.
---

# Retro

Analyze recent work from git history and produce a sprint retrospective.

## Data Collection

```bash
# Default: last 7 days. User can specify: /retro 14 (days) or /retro 2024-03-01..2024-03-15
git log --since="7 days ago" --format="%H|%an|%ae|%ad|%s" --date=short
git diff --stat HEAD~$(git rev-list --count --since="7 days ago" HEAD)..HEAD
```

## Analysis Sections

### 1. Shipping Velocity
- Total commits
- Total lines changed (added/removed/net)
- Files touched
- Commits per day trend

### 2. Work Distribution
- Breakdown by commit type (feat/fix/chore/refactor/docs/test)
- Which areas of the codebase were most active?
- Any files that were changed repeatedly? (churn — possible design issue)

### 3. Test Health
- Test-to-code ratio in new changes
- New tests added vs new code added
- Any commits that changed production code without corresponding tests?

### 4. Code Quality Signals
- Average commit size (large commits = harder to review)
- Revert commits (something went wrong)
- Fix commits shortly after feature commits (bugs introduced)
- Files with high churn (touched 3+ times in the period)

### 5. What Shipped
- List of features/fixes shipped (from commit messages)
- Group by area/component

### 6. Patterns & Insights
- **Shipping streaks** — consecutive days with commits
- **Focus vs context-switching** — how many different areas touched per day?
- **Momentum** — is velocity increasing, stable, or decreasing vs previous period?

## Output Format

```
## Sprint Retrospective — [Date Range]

### Summary
- X commits, Y files changed, +Z/-W lines
- N features shipped, M bugs fixed

### What Shipped
- [Feature/fix with commit reference]

### Velocity Trend
[Increasing/Stable/Decreasing] vs previous period

### Health Signals
- Test ratio: X% of changes include tests
- Avg commit size: X lines
- Churn hotspots: [files touched 3+ times]
- Reverts: X

### What Went Well
- [Specific praise anchored in commits]

### What Could Improve
- [Actionable suggestion based on data]

### Focus for Next Sprint
- [1-2 recommendations]
```

## Rules

- Base everything on git data — no speculation
- Compare against previous period when possible for trend analysis
- Be constructive: "improvement opportunity" not "you did poorly"
- If the user works solo, skip team breakdowns and focus on personal patterns
- Highlight wins — retrospectives should motivate, not just critique
