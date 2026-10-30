---
name: Engineering task
about: A single unit of implementation work. One worktree, one PR.
title: "[area:<x>] <verb-led short title>"
labels: []
assignees: ""
---

<!--
Add the correct routing label after opening the issue (area:cli, area:kernel,
area:mcp, area:tui, area:entity, area:enrichment, area:sync, area:plugin,
area:observability, area:docs, area:ci, area:agent-docs, area:agent-infra).
Frontmatter cannot hold a placeholder label because GitHub applies it literally.
-->


## Goal

One paragraph. What "done" looks like in plain language. No implementation steps; the "how" is gathered at implementation time inside the worktree (Claude plan mode).

## Acceptance criteria

- [ ] Concrete and testable. Each item is a behavior, not a file path.
- [ ] ...

## References

- Design doc: `agent-docs/designs/<subsystem>.md` (sections X)
- Parent epic: #N
- Depends on (blocked by): #N, #N

## Out of scope

- Things that look related but belong to another issue.

## Hot path?

<!-- Fill this in if the work touches a hot path so the implementer ships a
benchmark and records a baseline (`ns/op`, `B/op`, `allocs/op`) in the PR.
Skip this section otherwise. Examples of hot paths: event log writer (called
per remember), MCP request handler (called per tool invocation), recall
composer (called per recall), embedding call (called per remember + per
recall), four-layer dispatcher (called per remember). -->

`(none)` | `per remember` | `per recall` | `per startup` | `<frequency>`

## Owner subagent

`@golang-developer` (or whichever subagent in `.claude/agents/` matches the area label).
