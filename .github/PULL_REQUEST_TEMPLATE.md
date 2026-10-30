## Description
<!-- Brief summary of changes -->

## Type of Change
- [ ] New feature
- [ ] Bug fix
- [ ] Breaking change
- [ ] Documentation
- [ ] Refactoring
- [ ] Infrastructure / tooling

## Checklist
- [ ] Changes focused on single objective
- [ ] **Tests in the same PR** for any new exported behavior in `internal/`. Patch coverage >= 60% per `codecov.yml` (informational during pre-alpha; below threshold needs a one-sentence justification here)
- [ ] JSON Schemas have doc comments (for new MCP tools / new event types)
- [ ] Naming follows Google AIP (createTime, plural collection paths, CRUD verbs)
- [ ] `agent-docs/lessons-learned.md` updated if any new gotcha surfaced
- [ ] **Hot path?** If yes (event log writer, four-layer dispatcher, MCP request handler, recall composer, embedding call, or anything the issue's `## Hot path?` field flags), benchmark added/updated and baseline recorded under "Performance" below (`ns/op`, `B/op`, `allocs/op`)
- [ ] **User-facing surface?** If yes (CLI command, MCP tool, TUI screen, config knob, env var, error code), `agent-docs/<area>.md` updated and `agent-docs/mintlify-backlog.md` has an entry
- [ ] `mise run ci` green locally
- [ ] If this closes an engineering issue, the PR description includes `Closes #N`

## PR Size
**LoC**: <!-- GitHub auto-calculates -->
**Exception**: <!-- If >300 LoC, justify (initial impl, refactor, migration) -->

## Performance
<!-- If this change is on a hot path, paste the benchmark baseline here. Format: -->
<!--   BenchmarkX-N    ops    ns/op    B/op    allocs/op -->
<!-- Empty if not a hot path. -->

## Docs
<!-- If this change is user-facing, link the agent-docs file you updated and the Mintlify backlog entry. Empty for internal-only refactors or tests. -->

## Related
<!-- Issue refs, design doc links, screenshots -->
