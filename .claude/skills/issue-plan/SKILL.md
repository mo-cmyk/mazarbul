---
name: issue-plan
description: Drafts the implementation plan for a Mazarbul GitHub issue. USE WHEN user references a Mazarbul issue number ("#NN", "issue NN"), says "plan #NN", "implement issue NN", "pick up #NN", "design #NN", or wants a plan-mode breakdown before coding. Reads the issue via `gh issue view`, scopes affected files, checks the Depends-on list against merged state, drafts file-by-file changes + acceptance criteria as a local scratch plan in `.claude/plans/` (gitignored). NOT FOR free-floating refactors (use /complex-refactor) or quick tasks under 30 minutes (just do them).
allowed-tools: Bash, Read, Grep, Glob, Write, Skill
argument-hint: <issue-number>
---

Investigate a Mazarbul GitHub issue and produce a concrete, file-by-file implementation plan in `.claude/plans/` (gitignored, local scratch). Five phases: dependency check, issue + codebase analysis, best-practice grounding, plan, report. The plan is working scratch to align with the user before coding, not a committed artifact.

`$ARGUMENTS` is the issue number (digits only, e.g., `20`, not `#20`).

## Phase 1, Dependency Check

Read the issue body **and every comment** via `gh issue view $ARGUMENTS --json title,body,labels,milestone,state,comments` and surface:

- Title, milestone, area label, state (must be `OPEN`).
- The issue body **plus every comment on the thread**. Acceptance criteria and design decisions are often clarified in comments after the issue opens; ignoring them produces a plan that silently omits requirements.
- The `Depends on (blocked by):` line in the body **or any later comment that explicitly amends it**. Parse every `#N` token.

For each `#N`, run `gh issue view N --json state --jq '.state'`. Accept `CLOSED` (closed issue) or `MERGED` (merged PR) as done. Anything else means the dep is open and this issue is blocked.

If blockers exist, list them with their current statuses and **stop**. Do not proceed to planning. Surface "you need to land #N first". If a blocker is in active progress and might land soon, the user can override and you continue with a stub plan, but say so explicitly.

If no blockers, continue.

## Phase 2, Issue and Codebase Analysis

Re-read the issue body and **every comment captured in Phase 1**, the parent epic body and its comments (`Parent epic: #N` line), the referenced design doc (`Design doc: agent-docs/designs/<x>.md`) if it exists, and the interface contract file in `internal/`. Issue comments often carry the freshest scope clarifications; treat them as authoritative when they amend the original body.

Then map current code:

- `Grep` and `Glob` for symbols and files the issue references. The issue text may be ahead of or behind the code, current code is authoritative. Flag every discrepancy: "issue says X exists, code shows Y" or "issue assumes Z is missing, it was already implemented in commit abc123".
- Identify the touched packages under `internal/`. Mazarbul has no `proto/`, no `web/`, no `frontend`, no Connect-RPC. Skip those checks.
- Read related code paths that will be touched. Do not speculate, read.

## Phase 3, Best-Practice Grounding

Cite the authoritative source per layer in the plan:

- **Go kernel / `internal/*`**: Google Go Style Guide, Effective Go, the `agent-docs/synthesis.md` operating principles, and `agent-docs/<area>.md` for the subsystem-specific conventions. The `go-backend` skill auto-loads here.
- **API / schema / MCP tool surface**: Google AIP (aip.dev) for naming (`createTime` not `createdAt`, plural collection paths, CRUD verbs). The `aip-naming` skill auto-loads.
- **TUI / Bubble Tea**: `agent-docs/tui-design.md`, `agent-docs/lifecycle-theming.md`. The `tui-design` skill auto-loads.
- **MCP server / tools**: `agent-docs/tech/mcp-go.md`, the `mark3labs/mcp-go` library docs.
- **Performance**: `agent-docs/profiling.md` (`mise run bench`, `mise run pprof:cpu`, `mise run pprof:mem`). If the issue carries a `## Hot path?` field, the plan must include a benchmark step with a baseline.
- **Testing**: `agent-docs/quality-security.md` (test-during-implementation, race detector always on, integration tests use real SQLite + Kuzu + sqlite-vec, build tag `//go:build integration` for the cross-method tests).
- **General**: Established patterns in this codebase **take precedence** over generic advice. If the issue conflicts with `AGENTS.md` critical rules (no proto, AIP naming, no em-dashes, Greptile 5/5 before merge, etc.), flag the conflict.

## Phase 4, Implementation Plan

Draft a concrete, step-by-step plan that:

- Lists every file to create or modify, with a one-line summary of the change.
- Follows the layer-specific best practices from Phase 3.
- **Reuses existing helpers and patterns** rather than re-implementing them. Name the helper and its path.
- Calls out open questions or decisions that need user input as a numbered list.
- Includes acceptance criteria as a verification checklist, cross-referenced with the issue body.
- If the issue carries the `track:test` label or is a tests-only PR, the plan covers test design (table-driven cases, edge cases, race-clean assertions). If it carries `track:impl`, the plan includes tests alongside the implementation per `agent-docs/quality-security.md`.
- If the issue is on a hot path (event log writer, four-layer dispatcher, MCP request handler, recall composer, embedding call), the plan includes a benchmark step that records a baseline in the PR description.

Write the plan to `.claude/plans/PLAN-${ARGUMENTS}-<short-slug>.md` using the template at `agent-docs/design-doc-template.md`. The `PLAN-` prefix keeps the file discoverable by downstream tooling: `/impl-check` defaults its search glob to `.claude/plans/PLAN-*.md`, and other skills that find "the most recent plan" rely on the same pattern. `${ARGUMENTS}` is the issue number; the slug mirrors the worktree slug (lowercase alphanumerics + dashes, derived from the issue title's verb-led portion). Example: issue #20 -> `.claude/plans/PLAN-20-event-log-writer.md`. The file is local scratch (`.claude/plans/` is gitignored); never stage or commit it.

## Phase 5, Report

Print:

- Issue title + state + milestone.
- Blockers found (or "none, ready to implement").
- Touched packages.
- Plan file path.
- Open questions count (or "none").
- Recommended next move: "approve plan, then start implementing; ship via `/ship`, babysit via `/babysit-pr`".

---

## Gotchas

- _(empty, first entry lands when you hit a non-obvious pitfall)_

## Progressive disclosure, references

- `agent-docs/synthesis.md`: design synthesis, critical rules, architectural commitments.
- `agent-docs/designs/`: per-subsystem design docs.
- `agent-docs/design-doc-template.md`: the plan template.
- `agent-docs/worktree-workflow.md`: worktree mechanics; `.claude/agents/` lists the per-area owner subagents.
- `scripts/devstack`: the worktree spawner; `./scripts/devstack ready` computes the issue ready set.
