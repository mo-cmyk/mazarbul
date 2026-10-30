---
name: write-docs
description: PARKED. Mazarbul's public Mintlify site is not live yet. Until then, use agent-docs/mintlify-backlog.md to log user-facing surfaces that need a Mintlify page when the site activates. When Mintlify lights up, this skill gets a Mazarbul-native rewrite. NOT for internal agent-docs/ updates (those land alongside the implementing PR per AGENTS.md rule 11).
allowed-tools: Read
argument-hint: (none)
---

This skill is parked. Mazarbul has no public Mintlify site yet.

## What to do instead

If you are about to ship a user-facing surface (CLI command, MCP tool, TUI screen, config knob, env var, error code):

1. Update the corresponding `agent-docs/<area>.md` file in the same PR.
2. Append a row to `agent-docs/mintlify-backlog.md` recording the intended Mintlify page path, the source PR/issue, status (`backlog`), a short description, and today's date.

Per AGENTS.md rule 11.

If you are writing **internal** documentation aimed at Claude / agents (architecture notes, design docs, conventions, lessons learned), edit the relevant file under `agent-docs/` directly. The `/lessons` skill handles appending to `agent-docs/lessons-learned.md`.

## When this skill activates

When the public Mintlify site is set up, this file gets rewritten to drive `docs/<area>/<page>.mdx` pages. The rewrite will:

- Use VHS-recorded TUI demos under `docs/demos/` for visuals (Mazarbul has no web UI).
- Read the existing entry in `agent-docs/mintlify-backlog.md` to find the next page to draft.
- Source content from the implementing PR's `agent-docs/<area>.md` update and from interface comments under `internal/`.
- Cover both the CLI and MCP surface where a feature has both.
- Default to English; add a German translation path only if a sibling `docs/de/` tree exists.

Until then, do nothing here.
