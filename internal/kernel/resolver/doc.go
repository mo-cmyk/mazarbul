// Package resolver implements Mazarbul's entity resolution pipeline.
// It is Phase 3 of the memory-layer bootstrap (plan: we-want-to-do-zazzy-newell.md).
//
// # Three-step matching pipeline
//
// Each incoming Mention passes through up to three matching steps before either
// merging with an existing Entity or creating a new one.
//
// Step 1 (exact + name-embedding): an exact (displayName, kind) lookup in SQLite
// is tried first. On a miss, a name-vector nearest-neighbour search against
// entities_vec where vector_type='name' AND kind=mentionKind is run. Cosine >=
// 0.95 is a confirmed match. The kind filter is required to prevent cross-kind
// merges (a Person named "Berlin" must not merge with a Place named "Berlin").
//
// Step 2 (identifier): if the Mention carries any Identifier values, the relational
// store is queried for Entities whose identifier set intersects. A matching
// identifier auto-confirms the merge regardless of name similarity.
//
// Step 3 (summary-embedding): a nearest-neighbour search against entities_vec where
// vector_type='summary'. Cosine bands:
//   - >= 0.92: auto-merge, resolutionStatus=confirmed
//   - 0.78 <= x < 0.92: LLM disambiguation; verdict decides confirmed/tentative/new
//   - < 0.78: new Entity, resolutionStatus=confirmed
//
// All thresholds live in Config so the eval harness (P7) can tune them.
//
// # Bi-temporal supersede semantics
//
// Relations between Entities carry validFrom/validUntil pairs. When a new
// Relation arrives whose (subject, predicate) already exists with a different
// object and the predicate is functional (at most one current value per subject),
// the existing Relation is unconditionally superseded: its validUntil is closed
// to the new Relation's validFrom and a relationRetracted event is emitted.
// For non-functional predicates an LLM contradiction check gates the supersede.
//
// # Predicate vocabulary
//
// The predicates table (migration 0006) holds the functional/non-functional
// classification for every predicate. A seed list of ten predicates is inserted
// on first boot. Unknown predicates trigger an LLM classification call
// (predicate-canon-v1 prompt); the result is inserted and a predicateRegistered
// event emitted. The in-memory cache makes subsequent lookups O(1).
//
// # Evolving entity summaries
//
// Each Entity tracks how many new Mentions have accumulated since the last summary
// regeneration and when the last regen happened. Two thresholds trigger regen:
//   - N (default 5) new Mentions since the last regen
//   - 24 hours elapsed since SummaryUpdateTime
//
// Regen calls the ChatProvider with the summary-v1 prompt over the last K=20
// Mentions and emits an EntityUpdated event. A sync.Map dedupe guard prevents
// thundering-herd stacks during eval-harness backfill.
//
// # Coordinate forwarding
//
// When a place Mention carries coordinates (set by the upstream connector or
// maz remember --coords), the resolver copies them to the matched/created Entity
// together with the coordinatesProvenance string. The provenance rank order is
// user_provided > calendar:google > geocoded:nominatim; lower-rank sources never
// overwrite higher-rank coordinates.
package resolver
