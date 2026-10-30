---
name: impl-check
description: Verifies that implemented code matches the planned scope and behaves end-to-end. USE WHEN user says 'verify what I built', 'check that it works', 'did I implement this correctly', 'does this match the plan', 'run through the acceptance criteria', or finishes implementation against a /issue-plan. Diffs current branch vs plan, runs targeted tests, reports gaps. NOT FOR static lint/typecheck (use /verify) or pre-PR gating (use /preflight).
allowed-tools: Bash, Read, Grep, Glob
argument-hint: [plan-file-path]
---

Post-implementation verification , validate that all plan items are complete, correct, and review-ready. Mirrors the canonical "Post Implementation Check" prompt.

## Steps

**Phase 1 , Plan Completeness**

Locate the active plan file. If `$ARGUMENTS` is set, use that path; otherwise find the most recent `.claude/plans/PLAN-*.md`.

Walk through every item in the plan. For each:

- ✅ fully implemented
- ⚠ partial / different from plan (explain divergence)
- ✗ missing / skipped (flag)

Produce a markdown checklist. No item can be marked ✅ without evidence (file path + line range).

**Phase 2 , Best Practice & Idiom Audit**

Audit all changed and newly created code against the authoritative source per layer:

- **Proto/API**: Google AIPs, Protobuf Style Guide, project CLAUDE.md/AGENTS.md
- **Go backend**: Google Go Style Guide, idiomatic Go, project conventions
- **Frontend (Next.js/React/TypeScript)**: Next.js docs, React 19, project frontend rules (Geist, shadcn/ui, TanStack Query, Connect-ES)
- **General**: Consistency with patterns already established in this codebase

Flag and fix violations: naming, error handling, structure, component patterns, accessibility.

**Phase 3 , Static Analysis**

Run all linters and confirm zero warnings and zero errors:

```bash
# Proto (if proto/ changed)
 && 

# Go
mise run lint

# Frontend (if web/ changed)
cd web && pnpm lint
cd web && pnpm tsc --noEmit
```

Fix any issues found before proceeding. Never use `// nolint` or `eslint-disable` unless the suppression is genuinely correct (document why inline).

**Phase 4 , Test Coverage Assessment**

Investigate existing test coverage for all changed and newly created code:

- Identify what is already tested and what is not.
- Evaluate whether current coverage is adequate for the complexity and risk of the code , *not every line needs a test*, but **critical paths, business logic, error handling, and edge cases should be covered**.
- Compare against codebase testing conventions: table-driven tests in Go, React Testing Library for components, co-located test files.
- If meaningful gaps exist, write targeted tests that add real value , **do not add tests purely for coverage numbers**.

**Phase 5 , Run Tests**

```bash
mise run test     # Go, with race detector
# Frontend (if web/ changed)
cd web && pnpm test
```

If any test fails, investigate and fix the root cause , **do not skip or disable**.

**Phase 6 , Final Summary**

Report:
- Checklist of all plan items with completion status (✅ / ⚠ / ✗)
- Best practice violations found and how they were resolved
- Lint result: ✅ N files clean / ✗ N issues fixed
- Test result: ✅ N pass / ✗ N fail (must be 0 fail)
- Test additions: N new tests for coverage gaps

If anything is still ✗ or ⚠, the implementation is **not done**. Surface what's left.

---

## Gotchas

- _(empty)_
