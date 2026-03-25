---
description: Auto-review pipeline that runs CEO → Design → Engineering reviews sequentially with auto-decisions on non-critical choices.
---

# Autoplan

Run the full review pipeline sequentially. Auto-decide non-critical choices using the 6 decision principles below.

## Pipeline

1. **CEO Review** (`/plan-ceo-review`) — strategic positioning and scope
2. **Design Review** (`/plan-design-review`) — UX and interaction quality (skip if no UI scope)
3. **Engineering Review** (`/plan-eng-review`) — architecture and technical completeness

## 6 Auto-Decision Principles

When a review raises a choice that isn't critical, auto-decide using:

1. **Choose completeness** — boil the lake, do the full thing
2. **Pragmatic** — pick the cleaner, simpler solution
3. **DRY** — reject duplication
4. **Explicit over clever** — prefer readable over elegant
5. **Search first** — prefer existing solutions over new ones
6. **Bias toward action** — when two approaches are close, pick one and move

## User Gates

Only pause and ask the user for:

1. **Premise confirmation** — after CEO review Phase 1, confirm the problem statement is correct
2. **Scope decisions** — when two approaches have meaningfully different scope
3. **Final approval** — at the end, present all decisions made and ask for go/no-go

## Process

1. Run CEO review. Present premise for user confirmation.
2. If plan has UI scope, run Design review. Auto-decide non-critical design choices.
3. Run Engineering review. Auto-decide non-critical technical choices.
4. Collect all decisions into an audit trail.
5. Present **Final Approval Gate**:

```
## Autoplan Summary

**Reviews Completed:** CEO, Design, Engineering

**Auto-Decisions Made:**
1. [Decision] — Principle: [which principle applied]
2. ...

**Deferred to User:**
1. [Decision that needs user input]
2. ...

**Final Verdict:** READY / NEEDS INPUT

Proceed with implementation? (yes / modify / no)
```

## Rules

- Run reviews at full depth — don't abbreviate because it's automated
- Log every auto-decision with which principle was applied
- If a review produces NEEDS REWORK verdict, stop the pipeline and present findings
- Never auto-decide on: scope changes, technology choices, security trade-offs
