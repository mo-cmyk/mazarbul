// Adds the structured attributes column to RELATES_TO so the dispatcher's
// mergeRelationEdge query can carry role / title / employment-type
// qualifiers as a JSON-encoded string on each edge. Without this column
// the role information extracted from "Rosie is the CEO of Erebor" lands
// only in the source episode's text and is invisible to graph-lane
// reasoning queries (boss-of, member-of-with-role, multi-hop management
// chains).
//
// LadybugDB does not support ALTER REL TABLE in 0.13, so the table is
// dropped (IF EXISTS) and recreated with the additional column. This
// follows the same pattern as 0002_relation_validity.cypher and is safe
// because the graph store is a projection of the JSONL event log;
// rebuilding via `maz rebuild` is the documented recovery path.
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
    attributes STRING
);
