---
name: product-thinking
description: Product framing methodology for features and projects. Supports /office-hours, /plan-ceo-review, and strategic planning. Activate when the user is deciding WHAT to build, not HOW.
---

# Product Thinking Skill

Framework for thinking about products, features, and user problems before writing code.

## When to Activate

- User is brainstorming a new feature or product
- User asks "should we build X?" or "what should we build next?"
- User is prioritizing between multiple options
- Before `/plan` when the problem isn't clearly defined
- During `/office-hours` or `/plan-ceo-review`

## Core Frameworks

### 1. Problem-Solution Fit

Before building, answer:
- **Who** has this problem? (specific persona, not "developers")
- **How acute** is the pain? (nice-to-have vs can't-live-without)
- **How frequent** is the encounter? (daily vs yearly)
- **What do they do today?** (current workaround — your real competitor)
- **Would they pay/switch?** (willingness to change behavior)

### 2. Narrowest Wedge

Find the smallest version that delivers value:

```
Full vision → Cut features → Cut scope → Cut audience → Narrowest wedge
```

Rules:
- If you can't describe the wedge in one sentence, it's too wide
- If it takes more than 2 weeks to build, it's too wide
- If it serves more than one persona, it's too wide
- The wedge should be embarrassingly small but genuinely useful

### 3. 10-Star Experience

Rate the current experience 1-10, then imagine each level:

| Level | Experience |
|-------|-----------|
| 1-star | Broken — doesn't work |
| 3-star | Works but painful |
| 5-star | Acceptable — gets the job done |
| 7-star | Good — better than alternatives |
| 8-star | Great — users recommend it |
| 10-star | Magical — users can't imagine going back |

Work backwards from 10-star to find the 7-star that's actually buildable.

### 4. Competitive Positioning

Map the landscape:

```
            Simple
              |
    Low-end   |   Your wedge?
    tools     |
              |
Cheap --------+---------- Premium
              |
    OSS       |   Enterprise
    solutions |   platforms
              |
            Complex
```

Where do you sit? Where is there a gap?

### 5. Search Before Building

Three layers (from the ethos):
1. **Tried-and-true** — what's been done before in this space?
2. **New-and-popular** — what's recently emerged? (check GitHub trending, HN, recent blog posts)
3. **First-principles** — is there a fundamentally better approach nobody's tried?

## Decision Framework

When choosing between options:

| Signal | Action |
|--------|--------|
| Users asking for it | Strong signal — investigate deeper |
| Competitors all have it | Table stakes — build it unless you have a reason not to |
| Cool technology | Weak signal — technology is not a product |
| "I think users will want..." | Red flag — validate before building |
| Pain observed in support/feedback | Strong signal — fix the pain |

## Anti-Patterns

- **Building for yourself** — your workflow isn't everyone's workflow
- **Feature parity** — copying competitors without understanding why
- **Resume-driven development** — choosing tech because it's interesting, not because it's right
- **Scope creep via "while we're at it"** — stay focused on the wedge
- **Solution-first thinking** — "we should use GraphQL" before understanding the problem

## Quick Reference

```
Problem → Persona → Pain Level → Current Workaround → Narrowest Wedge → Build
  ↑                                                                        |
  └────────── Validate with real users/data ──────────────────────────────┘
```
