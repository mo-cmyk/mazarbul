---
name: simplify
description: Aggressive code-complexity audit. USE WHEN diff feels sprawling, after a feature lands, when /go-strict requests cleanup, or when user says 'scrap this and implement the elegant solution'. Audits across 4 dimensions (reuse, dead code, premature abstractions, oversize) and surfaces concrete cuts before applying. NOT FOR active development , only after a feature is functionally complete.
---

# /simplify , Aggressive Complexity Audit

Inspired by Boris Cherny's parallel `/simplify` ("agents fan out to review for reuse, quality, efficiency in one pass") and Karpathy's **Simplicity First** principle.

## When to invoke

- After implementing a feature (before opening PR)
- When a file feels long or the diff is sprawling
- Before merging a refactor , to make sure it actually *reduced* complexity
- When the user says "scrap this and implement the elegant solution"

## How it runs

This is a **read-and-report** skill. It does NOT change code without explicit user approval.

### Step 1 , Scope

Identify the changed files (default: `git diff --name-only HEAD`). If the user named a specific file or directory, use that instead.

### Step 2 , Parallel audit (single message, multiple agents)

Spawn 4 parallel `Explore` agents covering these dimensions:

| Agent | Looks for |
|---|---|
| **Reuse** | Existing helpers in `internal/repository/base.go`, `internal/repository/enums.go`, `internal/api/v1/errors.go`, `internal/api/v1/pagination.go`, `internal/api/v1/fieldmask.go` that the new code re-implements |
| **Dead code** | Unused exports, leftover commented-out blocks, types declared but never instantiated, error branches that can't be reached |
| **Premature abstraction** | Interfaces with one implementation, generic helpers used in exactly one place, "Manager"/"Helper"/"Util" suffixes, options structs with default-only fields |
| **Oversize** | Files >400 LoC, functions >80 LoC, structs with >15 fields , and whether the long shape is essential or splittable |

### Step 3 , Report

Output a single table:

```
File                              | Concrete cut                                      | LoC saved
internal/cli/entity.go:142         | Reuse existing output helper instead of manual switch | -14
internal/tui/format/priority.go    | Inline into single call site                      | -22 (delete file)
internal/kernel/types.go:33-40     | Remove duplicate field, use existing Status        | -7
```

Sum the savings, then ask the user which cuts to apply.

### Step 4 , Apply selected cuts surgically

Only after explicit "apply X, Y, Z" from the user. Each cut is a separate Edit. Do not bundle unrelated changes.

## What this skill deliberately does NOT do

- Refactor without showing the diff first
- "Improve" adjacent code that wasn't flagged
- Touch generated code (`gen/`, `web/src/gen/`)
- Modify audit log code (append-only, compliance-protected)

## Gotchas

- **The audit must be honest about no-ops.** If the diff is already tight, say so , don't invent cuts to look productive.
- **Skip generated code unconditionally.** `gen/`, `web/src/gen/`, and `migrations/*.sql` are out of scope.
- **Watch for "almost duplicate" code.** Two near-identical 30-line blocks may *intentionally* differ on one branch , read both before suggesting a merge.
- **`internal/api/v1/` has shared helpers most agents miss.** Always check `errors.go`, `pagination.go`, `fieldmask.go` before flagging a "new" need.
