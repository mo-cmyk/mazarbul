// 0005_scope: add scope property to graph tables for team-scope defense-in-depth.
//
// The authoritative scope isolation boundary is the SQLite relational read spine
// (internal/kernel/scoped_reader.go + navigate.go:531-533 scope-leak guard).
// The graph layer carries scope as an additional defense-in-depth layer so that
// Cypher traversal queries cannot inadvertently leak cross-scope data through the
// graph lane even when the relational guard is bypassed. See the ladybug package
// doc for the full rationale.
//
// Backfill: node properties default to 'personal' (kernel.DefaultScope), the
// fail-safe direction — a cross-scope visibility miss is preferable to a leak.
// Accurate per-event scope materialises on the next `maz rebuild`, which replays
// every event through the updated dispatcher graph writers.
//
// Relationship tables: LadybugDB does not support ALTER REL TABLE, so RELATES_TO
// and MENTIONS are dropped and recreated with the scope column. All relationship
// data reconciles on rebuild (the graph store is a projection of the JSONL event
// log). Node data (Entity, Episode) is preserved via ALTER TABLE and backfilled
// to 'personal'; the self-entity seed survives.
ALTER TABLE Entity ADD scope STRING DEFAULT 'personal';
ALTER TABLE Episode ADD scope STRING DEFAULT 'personal';

DROP TABLE IF EXISTS MENTIONS;

CREATE REL TABLE IF NOT EXISTS MENTIONS(
    FROM Episode TO Entity,
    scope STRING
);

DROP TABLE IF EXISTS RELATES_TO;

CREATE REL TABLE IF NOT EXISTS RELATES_TO(
    FROM Entity TO Entity,
    uid STRING,
    predicate STRING,
    confidence DOUBLE,
    createTime TIMESTAMP,
    validFrom TIMESTAMP,
    validUntil TIMESTAMP,
    validityScore DOUBLE,
    attributes STRING,
    scope STRING
);
