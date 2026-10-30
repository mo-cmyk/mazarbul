// 0004_self_entity_seed: seed the canonical "self" Entity node into the graph.
//
// SQLite migration 0012_self_entity_seed seeds `self` into the relational
// `entities` table only; nothing seeds the LadybugDB node. Without this node,
// mergeRelationEdge's `MATCH (o:Entity {uid:"self"})` binds nothing and every
// self-anchored edge (self --has_name--> X, document --mentions--> self) is
// silently dropped from the graph, so recall's graph-hop lane cannot traverse
// through `self`. Issue #175.
//
// `self` is LOCAL identity, deliberately seeded as a projection bootstrap (here
// + SQLite 0012), NOT as a synced event in the JSONL log — a shared genesis
// event would conflate every future team member's identity onto one node. When
// multi-user lands this becomes a per-principal seed; both seeds change together.
//
// MERGE ... ON CREATE SET is idempotent and replay-safe (does not clobber a
// later EntityUpdated event for self). Values mirror SQLite 0012 exactly; the
// genesis createTime is fixed because migrations run literal cypher (no now()).
MERGE (e:Entity {uid: 'self'})
ON CREATE SET
    e.kind             = 'person',
    e.displayName      = 'Me',
    e.confidence       = 1.0,
    e.resolutionStatus = 'resolved',
    e.createTime       = timestamp('2026-05-30T00:00:00Z');
