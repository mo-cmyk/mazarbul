# Draft scope — private staging before durable memory

The `draft` scope is a personal staging area. Records written with
`--scope draft` (CLI) or `scope:"draft"` (MCP `remember`) are stored in the
event log and projected into the derived stores, but they are **excluded from
every default read path** until you explicitly promote or discard them.

Use draft when you want to capture something now without immediately committing
it to your personal or project memory: rough notes from an interrupted meeting,
candidate entity definitions you want to review before they enter the graph, or
any content that may turn out to be noise.

## Why it exists

The append-only log is durable by design. Once an episode is in `personal` or
`project` scope it is embedded, extracted, and sync-able. Draft gives you a
pre-commitment window: you can read the staged content, compare it against your
live memory, and then decide. Promotion is a deliberate act, not an automatic
one.

## The security boundary

`draft` is **not in `DefaultReadSet`**. The default read set is
`{project, personal}`. Passing `scopes:["draft"]` to any general-purpose read
verb (`entities`, `get`, `neighbors`, `relations`, `canvas`, `unrelate`, or
`recall`) returns:

```
E_VALIDATION: scope "draft" is not readable here; use the draftStage tool to inspect the staging area
```

This is enforced in `scopesFromParam` (`internal/mcp/tools/scopes.go`), which
rejects `ScopeDraft` even though `kernel.ParseScopeSet` recognizes it. The
rejection is structural — there is no code path through which a general read
verb reaches draft rows. The **only** way to read the staging area is through
the dedicated tools and CLI verbs below.

## Tools and CLI verbs

| MCP tool | CLI verb | Effect |
|---|---|---|
| `draftStage({view:"list"})` | `maz scope draft list` | List draft-scoped episodes, entities, and relations |
| `draftStage({view:"diff"})` | `maz scope draft diff` | Compare draft set against personal set side by side |
| `promoteDraft({name})` | `maz scope draft promote <uid>` | Move draft episode into personal scope |
| `discardDraft({name, reason?})` | `maz scope draft discard <uid>` | Remove draft episode from staging |

`draftStage`, `promoteDraft`, and `discardDraft` are **unlisted** MCP tools
(they do not appear in `tools/list`) — they back the CLI surface and are
callable by agents that have been steered to use the staging workflow. They are
not part of the default agent tool surface.

## Promote semantics

`promoteDraft` appends a `scopePromoted` event with `fromScope:"draft"` and
`toScope:"personal"`. The dispatcher's `applyScopePromoted` re-scopes the
target episode and all derived rows (mentions, relations, facts) from `draft` to
`personal`. After promotion the record is indistinguishable from one written
directly to personal scope.

**Target scope is always `personal`.** The `promoteDraft` tool hardcodes
`toScope = ScopePersonal`. Promoting into `team` is explicitly refused by the
dispatcher: pushing unreviewed draft data into a shared, encrypted scope is the
highest-risk leak, and team writes must go through the explicit team
grant/membership path. The dispatcher enforces this as a silent no-op with a
`Warn` log rather than an error, so a crafted event cannot brick the projection
on replay.

## Discard semantics

`discardDraft` appends a `scopeDiscarded` event. The dispatcher's
`applyScopeDiscarded` DELETEs:

- The draft episode row.
- All entity mentions sourced from that episode (entities that only existed
  because of this episode, and that are still in `draft` scope).
- Their vector sidecars in `episodes_vec` / entity embeddings.
- The episode's provenance rows.

The provenance DELETE is gated on the target episode still being a `draft`
episode in the derived store — a stale discard event cannot strip provenance
from a row that was already promoted. Draft entities referenced directly (not
only derived from the episode) are also cleaned up, leaving no orphaned draft
data behind.

The event log is append-only. `discardDraft` does not physically remove the
`scopeDiscarded` event or the original draft episode event from the JSONL log.
The audit trail survives; only the projection rows are removed.

## Dispatcher security gates

All gates are in `internal/kernel/storage/dispatcher/scope_stage.go`:

1. **`fromScope` must be `"draft"`** before any UPDATE/DELETE is applied. An
   event with a different `fromScope` is silently ignored.
2. **`toScope` must be `"personal"` or `"project"`** for promotion. Any other
   value (including `"team"`) is silently ignored with a `Warn` log.
3. **Provenance DELETE is gated** on the target episode still being a draft row.
   A stale `scopeDiscarded` event cannot touch a promoted (live) episode's
   provenance.
4. Silent no-ops rather than errors on gate failure: a `return nil` keeps the
   dispatch loop alive on replay so a crafted event can never brick the
   projection.

## Rebuild determinism

Both `scopePromoted` and `scopeDiscarded` events are idempotent on replay. On a
full log rebuild, the original draft event projects the row first; the scope
lifecycle events replay in log order and apply the same mutations, converging to
the same derived-store state as the live system.

## Events and storage

- Draft records land in the standard `episodes`, `entity_mentions`, `entities`,
  and `relations` tables with `scope = 'draft'`.
- `scopePromoted` (`ontology.KindScopePromoted`) — carries `{targetUid,
  fromScope, toScope}`.
- `scopeDiscarded` (`ontology.KindScopeDiscarded`) — carries `{targetUid,
  reason?}`.
- No migration is required beyond the existing scope column: `draft` is a valid
  scope value in the existing schema.

## Relevant source files

| File | Role |
|---|---|
| `internal/kernel/storage/dispatcher/scope_stage.go` | Dispatcher projections for `scopePromoted` and `scopeDiscarded` |
| `internal/mcp/tools/scopes.go` | `scopesFromParam` — rejects `draft` from general read verbs |
| `internal/mcp/tools/draft_stage.go` | `draftStage` MCP tool (list + diff views) |
| `internal/mcp/tools/promote_draft.go` | `promoteDraft` MCP tool |
| `internal/mcp/tools/discard_draft.go` | `discardDraft` MCP tool |
