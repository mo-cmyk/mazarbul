# Warning codes

maz surfaces non-fatal advice as stable, machine-readable warning codes so an
agent can branch on a CODE instead of parsing prose (which changes). A warning
never fails a call; it rides alongside the result as a `warnings: [{code,
message}]` array (MCP) or a stderr note (CLI).

Codes are `MAZ-<AREA>-NNN` and **append-only**: a code never changes meaning or
gets renumbered, so a branch on `MAZ-RECALL-001` keeps working across releases.
The frozen catalog is `internal/warncode/warncode.go`; this file is its
human-readable mirror and must stay in sync.

## How to read a warning

A warning means "your call succeeded, but here is something you might act on".
Most are informational. The ones worth a branch are called out below.

## Ontology authoring — `MAZ-KIND-*`

| Code | Meaning | What to do |
|---|---|---|
| `MAZ-KIND-001` | The kind you defined is a near-duplicate of an existing one; it was minted with a `closeMatch` alias. | Consider reusing the existing kind instead. The alias is in the response. |
| `MAZ-KIND-002` | Semantic dedup was skipped (no embedder, or it timed out); only lexical dedup ran. | Expected on a cold/offline embedder. A semantic synonym may exist; re-run later if it matters. |

## Schema evolution — `MAZ-SCHEMA-*`

| Code | Meaning | What to do |
|---|---|---|
| `MAZ-SCHEMA-001` | The change supersedes the kind to a new schema version (vN → vN+1). | Existing data is preserved; new writes use the new version. |
| `MAZ-SCHEMA-002` | The change would orphan entities relying on a removed/renamed field (count in message). | Review with `maz schema preview` before applying. |
| `MAZ-SCHEMA-003` | The candidate collapses into an existing kind rather than adding a distinct one. | No new kind was created; the existing one covers it. |

## Retrieval — `MAZ-RECALL-*`

| Code | Meaning | What to do |
|---|---|---|
| `MAZ-RECALL-001` | The embedder was unavailable or exceeded the per-recall deadline; results are FTS-only. | Results are lexical, ranking quality reduced. Retry when the embedder is warm for semantic recall. |
| `MAZ-RECALL-002` | Verbatim full-text matches were sparse, so a prefix/fuzzy fallback widened the query. | Useful when a term was misspelled; check that the widened hits are relevant. |

## Bulk load — `MAZ-LOAD-*`

| Code | Meaning | What to do |
|---|---|---|
| `MAZ-LOAD-001` | In merge mode a record collided on an existing UID and was skipped; the existing event is kept (merge is skip-if-present, not a field-level merge). | Idempotent re-load; nothing to do. |
| `MAZ-LOAD-002` | A malformed/invalid record was skipped (count in message). | Inspect the skipped records; `--dry-run` reports them without writing. |

## Feedback loop — `MAZ-FEEDBACK-*`

| Code | Meaning | What to do |
|---|---|---|
| `MAZ-FEEDBACK-001` | A recurring lesson crossed the recurrence threshold and auto-promoted into the per-session guidance. | Review with `maz feedback gotchas`; pin, edit, or veto it. |

## Adding a code

1. Add the const to `internal/warncode/warncode.go` with the next `NNN` for its area (never reuse a number).
2. Add a row here.
3. Emit it with `warncode.New(warncode.CodeX, "message %s", arg)`.
