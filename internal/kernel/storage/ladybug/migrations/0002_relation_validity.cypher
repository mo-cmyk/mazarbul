// Adds the bi-temporal validity + identity columns to RELATES_TO so the
// dispatcher's mergeRelationEdge query can set uid, createTime, validFrom,
// validUntil, and validityScore on each edge.  Without these columns the
// MERGE Cypher emitted by dispatcher.go fails with "Cannot find property
// uid for r" against a freshly migrated graph DB.
//
// LadybugDB does not support ALTER REL TABLE in 0.13, so the table is
// dropped (IF EXISTS) and recreated.  This is safe because the graph
// store is a projection of the JSONL event log; rebuilding via `maz
// rebuild` is the documented recovery path.
DROP TABLE IF EXISTS RELATES_TO;

CREATE REL TABLE IF NOT EXISTS RELATES_TO(
    FROM Entity TO Entity,
    uid STRING,
    predicate STRING,
    confidence DOUBLE,
    createTime TIMESTAMP,
    validFrom TIMESTAMP,
    validUntil TIMESTAMP,
    validityScore DOUBLE
);
