# Structured Query Surface Design Document

Status: implemented (2026-06-05)
Worktree: `mass-functions`

## Context

Mazarbul had two read surfaces: **semantic recall** (fuzzy, ranked) and
**seed-then-traverse navigation** (`get`/`neighbors`/`relations`/`entities`/
`introspect`). Missing was the *structured-data* surface a CRM-style user
(human or agent) expects: "list all people", "people with `status = pending`",
"how many people named Marcus", and arbitrary analytical edge cases.

The blocker was data-model shaped: entities carry only a handful of typed
columns, and the natural home for scalar attributes — the `facts` table (EAV:
`subject_uid`, `state_key`, `value`, bi-temporal `event_start`/`event_end`,
`content_hash` dedup, `superseded_by_uid`) — existed in schema (migration 0014)
but had **no writer**. Relation qualifiers (`role`, `title`) were already
populated in `relations.attributes`.

## Goals

- Filter and count entities by attribute value, deterministically, LLM-free.
- Give agents a read-only SQL escape hatch for the long tail, over a stable
  virtual schema that survives ontology churn in the `dynamic-ontology`
  worktree.
- Make entity attributes (`status`, `firstName`, …) filterable now by wiring
  the facts writer.

## Non-goals

- Teaching the LLM extractor to emit facts — that overlaps `dynamic-ontology` +
  mem0-parity. This effort ships **direct assertion** (`maz fact set`, the
  `fact` tool) only.
- Numeric aggregates (avg/min/max) — deferred until numeric fact slots exist.
- The canonical attribute-name → `state_key` mapping — owned by
  `dynamic-ontology`. The filter compiler maps unknown fields → fact slots
  generically, so it auto-activates as that worktree defines typed attributes.

## Proposal — three layers

### Layer 1 — Facts writer (the attribute store)

The dispatcher gains `KindFact` handling (`applyFact`, mirroring `applyRelation`):
an idempotent **insert-then-reconcile-slot** supersede. `writeFactRow` inserts
with content-hash dedup (a re-emitted identical fact is a no-op);
`reconcileFactSlot` re-derives the validity window and supersede chain as a pure
function of the current row set for a `(subject_uid, state_key)` slot, so replay
is idempotent. `RegisterFact` is wired into the ontology so `Append` validates
Fact payloads.

Direct-assertion surface (no LLM): the `fact` MCP tool and `maz fact set <subject>
<stateKey> <value>` / `maz fact get <subject>`. `FactsForSubject` makes a
subject's open facts visible to `get(entities/{uid})` (facts *are* attributes)
and to the filter compiler.

### Layer 2 — AIP-160 structured filter + count/aggregate

A hand-rolled [AIP-160](https://google.aip.dev/160)-subset parser
(`internal/kernel/navigate/filtersql`) → a SQL compiler (`navigate/filter.go`).
Every value is a bound `?`; only allow-listed field names reach the query.

Field mapping (the virtual schema agents filter against):
- `kind`, `displayName`, `confidence`, `resolutionStatus`, `createTime` →
  `entities` columns.
- `related("<predicate>", "<target>")` → `EXISTS` over `relations`, current
  validity by default (matches on object uid or display name).
- **any other identifier → a fact slot**: `status = "pending"` → `EXISTS` over
  the entity's open fact for that `state_key`.

Operators `= != < > <= >= :` with `AND`/`OR`/`NOT` and parentheses; OR binds
tighter than AND, per spec. Navigator gains `CountEntities` and
`AggregateEntities` (GROUP BY an entity column or a fact slot), and `ListEntities`
grows a compiled-filter path. A malformed filter wraps `ErrInvalidFilter`, which
the tool layer maps to `E_VALIDATION` (not `E_INTERNAL`).

Surface: `entities --filter` (and the `entities` tool's `filter` param) plus a
new `count` tool + `maz count --filter '…' [--group-by <field>]` — the clean
home for "how many…".

### Layer 3 — Read-only SQL escape hatch (MCP-listed)

Curated views (migration 0017) are the documented, stable target schema:
- `v_entities` — entity projection + optional coordinates.
- `v_relations` — endpoint display names hydrated, `attributes` JSON verbatim,
  an `active` flag for the live-now slice.
- `v_facts` — currently-open scalar facts, one row per live slot, subject name
  hydrated.

The `query` tool + `maz query "SELECT …"` run a single statement on a hard
read-only connection. Safety stack:
1. Dedicated read-only connection (`mode=ro` reader pool + per-connection
   `PRAGMA query_only = ON`) — writes are physically impossible.
2. Statement validation (`validateReadOnlySQL`): scans the SQL once, skipping
   string/identifier literals and comments, so a banned word inside a literal is
   not mistaken for a keyword. Enforces a single statement, a `SELECT`/`WITH`
   start, and rejects any mutating/admin keyword (closing the `WITH … DELETE`
   form). This is defense-in-depth on top of (1), and the source of clean
   `E_VALIDATION` messages.
3. Row cap as a **fetch limit** (default 200, max 1000) rather than a SQL
   rewrite, so it composes with any `LIMIT` the caller wrote and with `WITH`
   queries; rows beyond the cap set `truncated`.
4. Context timeout (5s) honored by SQLite via interrupt.

`introspect` reports the view catalog (names + columns + descriptions) so an
agent writes correct SQL in one shot; `TestQueryViewCatalog_MatchesLiveViews`
validates the catalog against the live schema so it cannot silently drift.

### Documents

The filter surface is kind-agnostic, so documents become filterable the moment
they land as entities (`kind = "document" AND …`) — no doc-specific surface.
Document body full-text stays in recall/FTS. The `unstructured-data` worktree
owns doc bodies; add `v_documents` to Layer 3 once it lands.

## Cross-worktree coordination

- **This effort owns** attribute storage (`facts` writer), query (filter
  compiler + tools), and read surface.
- **`dynamic-ontology` owns** schema/validation: which `state_keys` are valid
  per kind and the canonical attribute-name → `state_key` mapping.

They compose along the existing `Fact` EAV contract; no re-plumbing needed as
typed attributes land.

## Alternatives considered

- **einride/cel-go for AIP-160** — not a dependency; would add a heavy tree.
  Hand-rolled a parser for the same syntax contract.
- **A SQL parser dependency for Layer 3 validation** — none in `go.mod`. The
  read-only connection is the hard guarantee; a single-pass token scan suffices
  for clean errors.
- **SQL `LIMIT` rewrite vs fetch cap** — rewriting arbitrary SQL (compound
  selects, `WITH`) is brittle; the fetch cap is robust and composes.

## Risks

- The static view catalog could drift from migration 0017 — mitigated by a test
  that binds every catalogued column against the live views.
- Mapping unknown filter fields to fact slots means a typo'd field silently
  matches nothing rather than erroring. Acceptable: it mirrors SQL semantics and
  keeps the compiler decoupled from the (churning) attribute schema.

## Acceptance criteria

- `maz fact set/get` round-trips; a second set to the same slot supersedes
  (verified through the built binary + `dispatcher_fact_test.go`).
- `maz entities --filter` and `maz count [--group-by]` return correct results;
  `related(...)` and fact-slot filters work.
- `maz query` runs SELECTs over the views; writes / chained statements / PRAGMA /
  DDL are rejected with `E_VALIDATION`; an injection payload inside a literal is
  inert; `introspect` lists the views.
- `/preflight` green (golangci-lint, `go test -race`, `mise run dead`).

## Open questions

- Numeric fact slots + numeric aggregates (avg/min/max) — when `dynamic-ontology`
  introduces typed values.
- Whether to expose a `v_documents` view (depends on `unstructured-data`).
