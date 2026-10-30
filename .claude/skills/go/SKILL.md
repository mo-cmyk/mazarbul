---
name: go
description: Quick composite ship: verify → simplify → PR. USE WHEN diff is small (<30 LoC), straightforward refactor, or you're confident the work is clean. NOT FOR substantive features (use /go-strict instead , it auto-iterates verify/critique/dead/simplify until clean). The composite skill: chain /verify (gate) → /simplify (default-on) → /ship (open PR).
---

# /go , Composite ship skill (verify → simplify → PR)

Boris Cherny:

> "Many of my prompts look like 'Claude do blah blah /go'. /go is a composite: test end-to-end, run /simplify, put up a PR."

This is the **final step** of a task. Do not invoke mid-feature.

## When to invoke

- After feature implementation is complete in your head
- After the user says "ship it" / "open the PR" / "/go"
- When ready to land , not when exploring

## Sequence (run strictly in this order)

### Phase 1 , Verify (gate)

Invoke `/verify` (`.claude/skills/verify/SKILL.md`). If anything is red:

- **STOP.** Do not proceed.
- Fix the failures, then re-run `/verify`.
- Only continue when fully green.

### Phase 2 , Simplify (optional but default-on)

Invoke `/simplify` (`.claude/skills/simplify/SKILL.md`). Report the cuts table.

- If cuts are obvious wins (>3 LoC each, no behavior change): apply them, then re-verify.
- If cuts are judgment calls: ask the user "apply X, Y, Z or skip?" , wait for explicit approval.
- If diff is already tight: skip apply, note "no cuts surfaced , diff is already clean."

### Phase 3 , Final verify (re-gate)

Re-run `/verify` after any simplify changes. Must be green.

### Phase 4 , PR

Invoke `/ship` (`.claude/skills/ship/SKILL.md`) , runs `mise run ci` loop and opens PR.

If the user wants to update an existing PR instead of opening a new one, invoke `/review` (`.claude/skills/review/SKILL.md`) which handles the fix-loop on a branch.

## What this skill deliberately does NOT do

- Skip verification "to save time"
- Open a PR with red checks
- Apply simplify cuts without surfacing them first
- Force-push or rewrite history
- Merge the PR (always user-driven, never agent)

## Pairs well with

- `/loop` , "every hour run /go on stale branches"
- `/goal` , set completion condition, then `/go`
- `Stop` hook , same checks one final time

## Gotchas

- **Order matters.** Verify before simplify before verify before ship , skipping the second verify lets simplify-introduced bugs slip into the PR.
- **Don't run `/go` on someone else's branch.** Always confirm `git branch --show-current` looks right.
- **`/ship` and `/review` are separate skills** , pick deliberately. `/ship` = new PR. `/review` = iterate on existing CI failures.
- **Compliance gate**: if the diff touches `migrations/`, `audit/`, or `permission.go`, the user should review the PR before merge , say so explicitly in the PR description.
