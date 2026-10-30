# PR Description Template

Copy this template when writing a PR description. Delete sections that do not apply. Keep it concise; reviewers read tens of PRs per week.

---

```markdown
## Summary

- [Bullet 1: what changed and why, the most important thing]
- [Bullet 2: secondary change if applicable]
- [Bullet 3: if needed, keep to 3 bullets maximum]

## Changes

**Kernel / storage** (if applicable):
- New event type: `XxxEvent` in `internal/kernel/events/xxx.go`
- New index hook: `internal/kernel/index/xxx.go`
- New migration: bumped event-log schema version to N

**CLI / TUI** (if applicable):
- New command: `maz xxx` in `internal/cli/xxx.go`
- New TUI screen: `internal/tui/xxx/`
- New flag: `--xxx` on `maz <command>`

**MCP** (if applicable):
- New tool: `xxx` in `internal/mcp/tools/xxx.go`
- Tool schema in `internal/mcp/schemas/xxx.json`

**Plugin** (if applicable):
- New provider/connector/extension: `<name>`
- Manifest: `<name>/plugin.toml`

**Docs** (if applicable):
- Updated: `agent-docs/<file>.md`
- New: `docs/<diataxis>/<page>.mdx`

## Checklist

- [ ] Changes focused on single objective
- [ ] Tests added/updated (table-driven, testify, race detector)
- [ ] JSON Schemas have description fields (for new MCP tools / new event types)
- [ ] Naming follows Google AIP (`createTime`, plural collection paths, CRUD verbs)
- [ ] Theming respected (no themed env vars / config keys / error codes / SDK names)
- [ ] No em-dashes in code, comments, docs, commit, or PR description
- [ ] `agent-docs/lessons-learned.md` updated if any new gotcha surfaced
- [ ] `mise run ci` green locally

## PR Size

**LoC**: <!-- GitHub auto-calculates -->
**Exception**: <!-- If >300 LoC, justify (initial impl, refactor, migration) -->

## Test plan

- [ ] `mise run ci` passes
- [ ] Manual verification: [describe what you tested manually, include data dir used, steps taken, observed behavior]
- [ ] TUI snapshot tests updated if `internal/tui/` changed (`mise run tui:snap`)
- [ ] VHS demo re-recorded if user-facing TUI changed (`mise run demo`)

## Screenshots / GIFs (if TUI changed)

| Before | After |
|--------|-------|
| *(GIF or N/A)* | *(GIF)* |

## Related

Closes #[issue-number] (if applicable)
```
