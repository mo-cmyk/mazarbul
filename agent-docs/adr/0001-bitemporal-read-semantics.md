# ADR-0001: Uniform bi-temporal read semantics across every read surface

## Status
Accepted

## Context
Mazarbul already exposes two temporal anchors at its read boundaries: `--at` /
`At` (valid-time) and `--as-of` / `AsOf` (transaction-time). They live in
`internal/kernel/query.go:40-55` (`RecallRequest.At`, `RecallRequest.AsOf`) and
in `internal/cli/navigate.go:39-101` (`navFilterFlags.at`, `navFilterFlags.asOf`,
mirroring the MCP `relationFilterInput` shape). They are documented per-surface
but were never ratified as a single contract, and the implementation proves it:
`appendRelationFilter` (`internal/kernel/navigate/navigate.go:1005-1032`) applies
both anchors to relation edges, while `FactsForSubject`
(`internal/kernel/navigate/facts.go:83-90`) accepts only `openOnly bool` and
honors neither anchor. The consequence is that the same `--at 2025-01-01` produces
a correctly time-sliced edge set but a present-tense fact set on the same entity
card. `recall` threads `At`/`AsOf` into graph-edge validity and event
`create_time`, but the fact-card path silently ignores `At`. Each read surface
(recall, get, doc canvas, relations, neighbors, and the not-yet-built timeline)
is free to interpret — or drop — the anchors differently. This is a correctness
hazard for any "what did we believe then / what was true then" query, and the
blast radius grows with every new read surface. This ADR ratifies the two anchors
as one cross-cutting contract and mandates a single shared predicate builder so
the semantics cannot drift per surface.

## Decision
We adopt two orthogonal temporal anchors with fixed, repo-wide meaning, applied
identically by **one** shared predicate builder that every read surface calls by
name (never re-inlines):

- **`--at <t>` — VALID-TIME.** "As the world was on date D." Filters facts and
  relations on their validity window using a half-open interval `[start, end)`:
  - facts: `COALESCE(event_start, create_time) <= @at AND (@at < event_end OR event_end IS NULL)`
  - relations: `(valid_from IS NULL OR valid_from = '' OR valid_from <= @at) AND (valid_until IS NULL OR valid_until = '' OR @at < valid_until) AND (superseded_by_uid IS NULL OR superseded_by_uid = '')`
  - When `@at` is nil it defaults to `time.Now().UTC()` ("current truth").
- **`--as-of <t>` — TRANSACTION-TIME.** "As we'd recorded it by date D." Filters
  on `create_time <= @asOf`, hiding facts, relations, and events recorded after
  that instant. When `@asOf` is nil, no transaction upper bound is applied.

Concrete, non-negotiable rules:

1. **Orthogonality.** `At` and `AsOf` are independent and may be set in any
   combination. Set together they answer "what did we believe on D2 (`@asOf`)
   about how the world was on D1 (`@at`)." Neither implies nor clamps the other.
2. **Anchors bind sub-queries, never the entity-identity row.** The anchors apply
   to the fact and relation sub-queries that hang off an entity. The timeless
   `entities` row — its `uid`, `kind`, `display_name` — is exempt and always
   passes through. An entity's *existence* is not bi-temporal; only its facts and
   edges are.
3. **Timestamps are RFC 3339, UTC.** Parsing and validation happen at the tool
   layer (MCP / CLI). Invalid or non-UTC input returns `E_VALIDATION` before any
   kernel call; the kernel only ever sees `*time.Time`. Comparisons use
   `julianday()` so RFC 3339 values of differing sub-second precision still order
   correctly.
4. **The half-open interval `[start, end)` is canonical.** Start inclusive, end
   exclusive. At a back-to-back supersession instant (`old.event_end ==
   new.event_start`) exactly one version — the new one — is live. This removes the
   double-count ambiguity that an inclusive-inclusive window would create.
5. **`includeInactive` bypasses BOTH the valid-window gate and the superseded
   filter** to return full history; it does not bypass `AsOf` (transaction time
   is still a hard recording bound).

### Asymmetry: why the entity row is exempt
Facts filter on the open-slot validity window; relations filter on their validity
window; the entity-identity row filters on neither. The reason is failure-mode,
not aesthetic. The entity row is the *navigation anchor* the caller has already
named (by `uid` or resolved name) — it is the seed for `get`, `neighbors`,
`relations`, and the recall graph lanes. If we time-sliced the row itself, an
entity created after `@asOf`, or one whose first fact post-dates `@at`, would
vanish and seed resolution would return `E_NOT_FOUND` for an anchor the caller is
explicitly holding — turning a "show me what was true then" query into a "this
entity does not exist" lie. Existence is established by the seed; the temporal
question being answered is strictly "which facts/edges were true (`@at`) and/or
known (`@asOf`)." Deletion is modeled as retraction of facts/edges, not as a
validity window on the identity row, so there is nothing bi-temporal to filter on
the row. Entity rows are therefore timeless in the read model; only their facts
and edges are time-sliced.

### Implementation seam
The two predicates (valid-time window, transaction-time bound) for each of the
two timelined tables (`facts`, `relations`) live in **one** predicate builder
each, with the today-divergent paths converging on it: `appendRelationFilter`
keeps its current behavior and `FactsForSubject` gains the same anchors
(replacing the bare `openOnly bool` with the shared valid-time window plus the
`AsOf` bound). recall, get, doc canvas, relations, neighbors, and timeline all
call the shared builder by name. No surface re-implements the SQL.

## Consequences
- **Positive:** One contract, one predicate builder per table; the same `--at` /
  `--as-of` produces identical slices on every surface, so an entity card from
  `get` and the edges from `relations` agree. Closes the `FactsForSubject` gap.
- **Positive:** `maz timeline`, TUI time-scrubbers, and doc-snapshot-at-time
  become thin wrappers that vary only the anchor values — no new temporal logic.
- **Positive:** Tool-layer `E_VALIDATION` on bad timestamps keeps the kernel
  total over `*time.Time`; the failure mode is named and lives at the boundary.
- **Negative:** Touching `FactsForSubject`'s signature ripples through every
  fact-card caller (get, recall fact lane, doc canvas) — a mechanical but
  non-trivial change that must land atomically to avoid a half-migrated read path.
- **Negative:** `julianday()`-based comparison assumes well-formed RFC 3339 in the
  stored columns; legacy rows with empty/`''` validity bounds rely on the explicit
  `IS NULL OR = ''` guards already present in the relation predicate, which the
  fact predicate must replicate exactly.
- **Open questions:** Should `recall`'s `Since`/`Until` (event `create_time` band)
  be folded into the same builder, or do they remain a distinct coarse pre-filter?
  How does `AsOf` interact with team-scope envelope-encrypted edges whose
  `create_time` reflects merge time rather than authorship time after CRDT sync?

## Alternatives considered
- **Per-surface temporal interpretation (status quo).** Each read path parses and
  applies the anchors however it likes. Rejected: it is exactly what produced the
  `FactsForSubject`-vs-`appendRelationFilter` divergence, and the inconsistency
  cost compounds with every new surface — the opposite of a contract.
- **Single conflated `at` anchor (drop transaction-time).** Collapse `At` and
  `AsOf` into one "point in time." Rejected: it cannot express "what did we
  believe on D2 about D1," which is the entire value of bi-temporality and is
  already a requirement encoded in `RecallRequest.AsOf`'s historical-lens use case.
- **Time-slice the entity row too (fully bi-temporal entities).** Apply the anchors
  to the `entities` row as well. Rejected: it breaks seed resolution (anchors the
  caller holds would `E_NOT_FOUND`), and entity existence has no validity window to
  filter on — deletion is fact/edge retraction, not row expiry.
- **Validity window as closed interval `[start, end]`.** Rejected: it double-counts
  at back-to-back supersession boundaries, where the half-open `[start, end)`
  convention yields exactly one live version.
